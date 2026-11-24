// Copyright Axis Communications AB.
//
// For a full list of individual contributors, please see the commit history.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// package sse for v1alpha uses the old SSE implemenation from the ESR log listener
// and the old version of the ETOS client.
package sse

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/eiffel-community/etos-api/internal/config"
	"github.com/eiffel-community/etos-api/internal/kubernetes"
	"github.com/eiffel-community/etos-api/pkg/application"
	"github.com/eiffel-community/etos-api/pkg/events"
	"github.com/julienschmidt/httprouter"
	"github.com/sirupsen/logrus"
)

type SSEApplication struct {
	logger *logrus.Entry
	cfg    config.SSEConfig
	ctx    context.Context
	cancel context.CancelFunc
}

type SSEHandler struct {
	logger *logrus.Entry
	cfg    config.SSEConfig
	ctx    context.Context
	kube   *kubernetes.Kubernetes
}

// Close cancels the application context
func (a *SSEApplication) Close() {
	a.cancel()
}

// New returns a new SSEApplication object/struct
func New(cfg config.SSEConfig, log *logrus.Entry, ctx context.Context) application.Application {
	ctx, cancel := context.WithCancel(ctx)
	return &SSEApplication{
		logger: log,
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
	}
}

// LoadRoutes loads all the v1alpha routes.
func (a SSEApplication) LoadRoutes(router *httprouter.Router) {
	kube := kubernetes.New(a.cfg, a.logger)
	handler := &SSEHandler{a.logger, a.cfg, a.ctx, kube}
	router.GET("/sse/v1alpha/selftest/ping", handler.Selftest)
	router.GET("/sse/v1alpha/logs/:identifier", handler.GetEvents)
}

// Selftest is a handler to just return 204.
func (h SSEHandler) Selftest(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNoContent)
}

// Subscribe subscribes to an ETOS suite runner instance and gets logs and events from it and
// writes them to a channel.
func (h SSEHandler) Subscribe(ch chan<- events.Event, logger *logrus.Entry, ctx context.Context, counter int, identifier string, url string) {
	defer close(ch)

	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("Client lost, closing subscriber")
			return
		case <-ping.C:
			ch <- events.Event{Event: "ping"}
		case <-tick.C:
			messages, err := GetFrom(ctx, url)
			if err != nil {
				// The context sent to IsFinished may be canceled due to client-side
				// throttling by Kubernetes. We don't want IsFinished to cancel the
				// the request context from our clients, causing a ConnectionReset,
				// so we create a new context here.
				if h.kube.IsFinished(context.Background(), identifier) {
					logger.Info("ESR finished, shutting down")
					ch <- events.Event{Event: "shutdown"}
					return
				}
				logger.Warning(err.Error())
				continue
			}
			// SSE starts ad ID 1, but slices start with index 0, so we do -1 here.
			if len(messages) >= counter-1 {
				for _, message := range messages[counter-1:] {
					event := events.Event{
						Event: "message",
						ID:    counter,
						Data:  message,
					}
					ch <- event
					counter++
				}
			}
		}
	}
}

// GetFrom gets all events from an ESR instance
func GetFrom(ctx context.Context, url string) (_ []string, resultErr error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}

	var messages []string
	scanner := bufio.NewScanner(response.Body)
	defer func() {
		if err := response.Body.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close log response: %w", err))
		}
	}()
	for scanner.Scan() {
		messages = append(messages, scanner.Text())
	}
	return messages, nil
}

// url finds the url of an ESR instance.
func (h SSEHandler) url(ctx context.Context, identifier string) (string, error) {
	ip, err := h.kube.LogListenerIP(ctx, identifier)
	if err != nil {
		return "", err
	}
	if ip == "" {
		return "", errors.New("no IP from ESR yet")
	}
	return fmt.Sprintf("http://%s:8000/log", ip), nil
}

// forceKillConnection hijacks the underlying TCP connection between the client and server
// and stops it forcefully. This will cause a panic in the goroutine running the connection
// but this is the only way for us to be compatible with the old ETOS SSE implementation.
func forceKillConnection(w http.ResponseWriter, logger *logrus.Entry) {
	logger.Warning("hijacking the connection in order to force kill it. This may result in panics in the log")
	hijack, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	connection, _, err := hijack.Hijack()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if err := connection.Close(); err != nil {
		logger.Errorf("failed to close hijacked connection: %v", err)
	}
}

// GetEvents is an endpoint for streaming events and logs from an ESR instance.
func (h SSEHandler) GetEvents(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	identifier := ps.ByName("identifier")
	if h.kube.IsFinished(r.Context(), identifier) {
		http.NotFound(w, r)
		return
	}
	// Making it possible for us to correlate logs to a specific connection
	logger := h.logger.WithField("identifier", identifier)

	url, err := h.url(r.Context(), identifier)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	last_id := 1
	lastEventID := r.Header.Get("Last-Event-ID")
	if lastEventID != "" {
		var err error
		last_id, err = strconv.Atoi(lastEventID)
		if err != nil {
			logger.Error("Last-Event-ID header is not parsable")
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.NotFound(w, r)
		return
	}
	logger.Info("Client connected to SSE")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Transfer-Encoding", "chunked")

	receiver := make(chan events.Event) // Channel is closed in Subscriber
	go h.Subscribe(receiver, logger, r.Context(), last_id, identifier, url)

	for {
		select {
		case <-r.Context().Done():
			logger.Info("Client gone from SSE")
			return
		case <-h.ctx.Done():
			logger.Info("Shutting down")
			return
		case event := <-receiver:
			if event.Event == "shutdown" {
				forceKillConnection(w, logger)
				return
			}

			if err := event.Write(w); err != nil {
				logger.Error(err)
				if err == http.ErrHijacked {
					return
				}
				continue
			}
			flusher.Flush()
		}
	}
}
