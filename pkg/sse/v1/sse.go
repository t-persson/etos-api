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
package sse

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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

// LoadRoutes loads all the v1 routes.
func (a SSEApplication) LoadRoutes(router *httprouter.Router) {
	kube := kubernetes.New(a.cfg, a.logger)
	handler := &SSEHandler{a.logger, a.cfg, a.ctx, kube}
	router.GET("/sse/v1/selftest/ping", handler.Selftest)
	router.GET("/sse/v1/events/:identifier", handler.GetEvents)
	router.GET("/sse/v1/event/:identifier/:id", handler.GetEvent)
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

	// TODO: Test a streaming approach.
	tick := time.NewTicker(2 * time.Second)
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
			newEvents, err := GetFrom(ctx, url, fmt.Sprint(counter))
			if err != nil {
				// The context sent to IsFinished may be canceled due to client-side
				// throttling by Kubernetes. We don't want IsFinished to cancel the
				// the request context from our clients, causing a ConnectionReset,
				// so we create a new context here.
				if h.kube.IsFinished(context.Background(), identifier) {
					logger.Info("ESR finished, shutting down")
					// If the shutdown event is not sent to the client, then the client will
					// reconnect and the message will be received next time.
					ch <- events.Event{Event: "shutdown", Data: "ESR finished, shutting down"}
					// We expect the client to close the connection, as such we continue here
					// instead of ending the subscriber.
					continue
				}
				logger.Warning(err.Error())
				continue
			}
			for _, event := range newEvents {
				ch <- event
				counter++
			}
		}
	}
}

// GetFrom gets all events from an ESR instance starting from id (including the id specified).
func GetFrom(ctx context.Context, url string, id string) (_ []events.Event, resultErr error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	query := request.URL.Query()
	query.Add("start", id)
	request.URL.RawQuery = query.Encode()

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close event response: %w", err))
		}
	}()

	etosEvents := []events.Event{}
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		event, err := events.New(scanner.Bytes())
		if err != nil {
			// TODO: Log it?
			continue
		}
		etosEvents = append(etosEvents, event)
	}
	return etosEvents, nil
}

// GetOne gets a single event from an ESR instance.
func GetOne(ctx context.Context, logger *logrus.Entry, url string, id string) (_ events.Event, resultErr error) {
	event := events.Event{}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return event, err
	}
	query := request.URL.Query()
	query.Add("start", id)
	query.Add("end", id)
	request.URL.RawQuery = query.Encode()

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return event, err
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close event response: %w", err))
		}
	}()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		logger.Errorf("Request failed (could not read response body): %+v", request)
		return event, err
	}
	return events.New(body)
}

// url find the url of an ESR instance.
func (h SSEHandler) url(ctx context.Context, identifier string) (string, error) {
	url, err := h.kube.LogListenerIP(ctx, identifier)
	if err != nil {
		return "", err
	}
	if url == "" {
		return "", errors.New("esr has not started yet")
	}
	return fmt.Sprintf("http://%s:8000/v1/log", url), nil
}

// GetEvent is an endpoint for getting a single event from an ESR instance.
func (h SSEHandler) GetEvent(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	identifier := ps.ByName("identifier")
	counter := ps.ByName("id")
	if h.kube.IsFinished(r.Context(), identifier) {
		http.NotFound(w, r)
		return
	}
	// Making it possible for us to correlate logs to a specific connection
	logger := h.logger.WithField("identifier", identifier)

	url, err := h.url(r.Context(), identifier)
	if err != nil {
		logger.Error(err)
		http.NotFound(w, r)
		return
	}

	event, err := GetOne(r.Context(), logger, url, counter)
	if err != nil {
		logger.Error(err)
		// TODO: Message client
		return
	}
	if err := event.Write(w); err != nil {
		logger.Error(err)
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
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	last_id := 1
	lastEventID := r.Header.Get("Last-Event-ID")
	if lastEventID != "" {
		var err error
		last_id, err = strconv.Atoi(lastEventID)
		if err != nil {
			logger.Error("Last-Event-ID header is not parsable")
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.NotFound(w, r)
		return
	}

	logger.Info("Client connected to SSE")

	// Context to be canceled if the SSE server is shutting down
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	receiver := make(chan events.Event) // Channel is closed in Subscriber
	go h.Subscribe(receiver, logger, ctx, last_id, identifier, url)

	for {
		select {
		// Check if SSE server is shutting down, canceling the subscriber contexts.
		case <-h.ctx.Done():
			if ctx.Err() == nil {
				logger.Info("Shutting down, canceling subscriber context")
				cancel()
			}
		case event, ok := <-receiver:
			// Check if subscriber has closed the channel
			if !ok {
				if ctx.Err() != nil {
					logger.Infof("Subscriber shutdown: %s", ctx.Err().Error())
				} else {
					logger.Info("Subscriber shutdown")
				}
				return
			}
			if err := event.Write(w); err != nil {
				logger.Error(err)
				continue
			}
			flusher.Flush()
		}
	}
}
