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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eiffel-community/etos-api/internal/config"
	ssemetrics "github.com/eiffel-community/etos-api/internal/metrics"
	"github.com/eiffel-community/etos-api/internal/stream"
	"github.com/eiffel-community/etos-api/pkg/application"
	"github.com/eiffel-community/etos-api/pkg/events"
	schema "github.com/eiffel-community/etos/schemas/messaging/v2alpha"
	"github.com/julienschmidt/httprouter"

	"github.com/sirupsen/logrus"
)

const pingInterval = 15 * time.Second

type Application struct {
	logger   *logrus.Entry
	cfg      config.SSEConfig
	ctx      context.Context
	cancel   context.CancelFunc
	streamer stream.Streamer
	metrics  *ssemetrics.SSEMetrics
}

type Handler struct {
	logger   *logrus.Entry
	cfg      config.SSEConfig
	ctx      context.Context
	streamer stream.Streamer
	metrics  *ssemetrics.SSEMetrics
}

// Close cancels the application context.
func (a *Application) Close() {
	a.cancel()
	a.streamer.Close()
}

// New returns a new Application object/struct.
func New(ctx context.Context, cfg config.SSEConfig, log *logrus.Entry, streamer stream.Streamer, metrics *ssemetrics.SSEMetrics) application.Application {
	ctx, cancel := context.WithCancel(ctx)
	return &Application{
		logger:   log,
		cfg:      cfg,
		ctx:      ctx,
		cancel:   cancel,
		streamer: streamer,
		metrics:  metrics,
	}
}

// LoadRoutes loads all the v2alpha routes.
func (a Application) LoadRoutes(router *httprouter.Router) {
	handler := &Handler{a.logger, a.cfg, a.ctx, a.streamer, a.metrics}
	router.GET("/sse/v2alpha/selftest/ping", handler.Selftest)
	router.GET("/sse/v2alpha/events/:identifier", handler.GetEvents)
}

// Selftest is a handler to just return 204.
func (h Handler) Selftest(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNoContent)
}

// cleanFilter will clean up the filters received from clients.
func (h Handler) cleanFilter(identifier string, filters []string) {
	for i, filter := range filters {
		if len(strings.Split(filter, ".")) != 3 {
			filters[i] = fmt.Sprintf("%s.%s", identifier, filter)
		}
	}
}

type ErrorEvent struct {
	Retry  bool   `json:"retry"`
	Reason string `json:"reason"`
}

// subscribe subscribes to stream and gets logs and events from it and writes them to a channel.
func (h Handler) subscribe(ctx context.Context, logger *logrus.Entry, streamer stream.Stream, ch chan<- events.Event, lastID int, filter []string) {
	defer close(ch)
	var err error

	consumeCh := make(chan []byte, 0)

	offset := -1
	counter := 1 // lastID will default to 1 and the first event will be 1

	closed, err := streamer.WithChannel(consumeCh).WithOffset(offset).WithFilter(filter).Consume(ctx)
	if err != nil {
		logger.WithError(err).Error("failed to start consuming stream")
		b, _ := json.Marshal(ErrorEvent{Retry: false, Reason: err.Error()})
		sendEvent(ctx, ch, events.Event{Event: "error", Data: string(b)})
		return
	}
	defer streamer.Close()

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	var event events.Event
	for {
		select {
		case <-ctx.Done():
			logger.Info("Client lost, closing subscriber")
			return
		case <-ping.C:
			if !sendEvent(ctx, ch, events.Event{Event: "ping"}) {
				return
			}
		case <-closed:
			logger.Info("Stream closed, closing down")
			b, _ := json.Marshal(ErrorEvent{Retry: true, Reason: "Streamer closed the connection"})
			sendEvent(ctx, ch, events.Event{Event: "error", Data: string(b)})
			return
		case msg := <-consumeCh:
			// We have no reliable way of getting a specific offset on the SSE stream so
			// we will need to iterate all events until we reach the last known ID.
			if counter < lastID {
				counter++
				continue
			}

			event, err = events.New(msg)
			if err != nil {
				logger.WithError(err).Error("failed to parse SSE event")
				h.metrics.EventDropped(eventType(msg), "parse_error")
				continue
			}
			if err := schema.Validate(msg); err != nil {
				logger.WithError(err).Warning("dropping SSE event that does not match the protocol")
				h.metrics.EventDropped(event.Event, "schema_invalid")
				continue
			}
			event.ID = counter
			if !sendEvent(ctx, ch, event) {
				return
			}
			counter++
		}
	}
}

// sendEvent stops a subscriber instead of blocking after its HTTP handler exits.
func sendEvent(ctx context.Context, ch chan<- events.Event, event events.Event) bool {
	select {
	case ch <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// eventType extracts only the event discriminator for bounded drop metrics.
func eventType(data []byte) string {
	var event struct {
		Event string `json:"event"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return "unknown"
	}
	return event.Event
}

// GetEvents is an endpoint for streaming events and logs from ETOS.
func (h Handler) GetEvents(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	identifier := ps.ByName("identifier")
	// Filters may be passed multiple times (?filter=log.info&filter=log.debug)
	// in order to parse multiple values into a slice r.ParseForm() is used.
	// The filters are accessible in r.Form["filter"] after r.ParseForm() has been
	// called.
	r.ParseForm()

	// Making it possible for us to correlate logs to a specific connection
	logger := h.logger.WithField("identifier", identifier)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	lastID := 1
	lastEventID := r.Header.Get("Last-Event-ID")
	if lastEventID != "" {
		var err error
		lastID, err = strconv.Atoi(lastEventID)
		h.metrics.Reconnect(err == nil)
		if err != nil {
			logger.Error("Last-Event-ID header is not parsable")
		}
	}

	filter := r.Form["filter"]
	h.cleanFilter(identifier, filter)

	streamer, err := h.streamer.NewStream(r.Context(), logger, identifier)
	if err != nil {
		// Failing to open a stream is a server-side (broker) failure and not caused by the
		// client request, so report it as retryable and keep the details out of the response.
		logger.WithError(err).Error("Could not start a new stream")
		http.Error(w, "event stream is temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.NotFound(w, r)
		return
	}
	logger.Info("Client connected to SSE")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	receiver := make(chan events.Event) // Channel is closed in Subscriber
	go h.subscribe(ctx, logger, streamer, receiver, lastID, filter)
	terminalResultWritten := false

	for {
		select {
		case <-r.Context().Done():
			if h.ctx.Err() == nil && !terminalResultWritten {
				h.metrics.Disconnect("request_context_cancelled")
			}
			logger.Info("Client gone from SSE")
			return
		case <-h.ctx.Done():
			logger.Info("Shutting down")
			return
		case event, ok := <-receiver:
			if !ok {
				return
			}
			if err := event.Write(w); err != nil {
				logger.Error(err)
				h.metrics.EventDelivery(event.Event, false)
				h.metrics.Disconnect("write_error")
				if event.Event == "shutdown" {
					h.metrics.TerminalResult("write_error")
				}
				if retryable(event) {
					h.metrics.Retry(false)
				}
				return
			}
			flusher.Flush()
			h.metrics.EventDelivery(event.Event, true)
			if event.Event == "shutdown" {
				h.metrics.TerminalResult("write_success")
				terminalResultWritten = true
			}
			if retryable(event) {
				h.metrics.Retry(true)
			}
		}
	}
}

// retryable reports whether an error event asks the client to reconnect.
func retryable(event events.Event) bool {
	if event.Event != "error" {
		return false
	}
	var errorEvent ErrorEvent
	return json.Unmarshal([]byte(event.Data), &errorEvent) == nil && errorEvent.Retry
}
