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
	"net/http"
	"strconv"
	"time"

	"github.com/IBM/sarama"
	config "github.com/eiffel-community/etos-api/internal/configs/sse"
	"github.com/eiffel-community/etos-api/pkg/application"
	"github.com/eiffel-community/etos-api/pkg/events"
	"github.com/julienschmidt/httprouter"
	"github.com/sirupsen/logrus"
)

const (
	KafkaServerAddress = "localhost:9092" // TODO: Obviously not
)

type SSEApplication struct {
	logger *logrus.Entry
	cfg    config.Config
	ctx    context.Context
	cancel context.CancelFunc
}

type SSEHandler struct {
	logger *logrus.Entry
	cfg    config.Config
	ctx    context.Context
}

// Close cancels the application context
func (a *SSEApplication) Close() {
	a.cancel()
}

// New returns a new SSEApplication object/struct
func New(cfg config.Config, log *logrus.Entry, ctx context.Context) application.Application {
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
	handler := &SSEHandler{a.logger, a.cfg, a.ctx}
	router.GET("/v1/selftest/ping", handler.Selftest)
	router.GET("/v1/events/:identifier", handler.GetEvents)
	router.GET("/v1/event/:identifier/:id", handler.GetEvent)
}

// Selftest is a handler to just return 204.
func (h SSEHandler) Selftest(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNoContent)
}

// Subscribe subscribes to an ETOS suite runner instance and gets logs and events from it and
// writes them to a channel.
func (h SSEHandler) Subscribe(ch chan<- events.Event, logger *logrus.Entry, ctx context.Context, counter int, identifier string) {
	defer close(ch)

	consumer, err := sarama.NewConsumer([]string{KafkaServerAddress}, sarama.NewConfig())
	if err != nil {
		logger.WithError(err).Error("failed to initialize consumer")
		return
	}
	defer func() {
		if err = consumer.Close(); err != nil {
			logger.WithError(err).Error("failed to close the consumer")
		}
	}()

	offset := sarama.OffsetOldest
	if counter != 0 {
		offset = int64(counter - 1)
	}
	partitionConsumer, err := consumer.ConsumePartition(identifier, 0, offset)
	if err != nil {
		logger.WithError(err).Error("failed to initialize partition consumer")
		return
	}
	defer func() {
		if err = partitionConsumer.Close(); err != nil {
			logger.WithError(err).Error("failed to close the partition consumer")
		}
	}()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("Client lost, closing subscriber")
			return
		case <-ping.C:
			ch <- events.Event{Event: "ping"}
		case msg := <-partitionConsumer.Messages():
			event, err := events.New(msg.Value)
			if err != nil {
				logger.WithError(err).Error("failed to parse SSE event")
				continue
			}
			event.ID = counter
			ch <- event
			counter++
		}
	}
}

// GetEvent is an endpoint for getting a single event from an ESR instance.
func (h SSEHandler) GetEvent(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	identifier := ps.ByName("identifier")
	id := ps.ByName("id")
	// Making it possible for us to correlate logs to a specific connection
	logger := h.logger.WithField("identifier", identifier)

	counter, err := strconv.Atoi(id)
	if err != nil {
		logger.WithError(err).Errorf("could not parse ID as integer: %s", id)
		return
	}

	consumer, err := sarama.NewConsumer([]string{KafkaServerAddress}, sarama.NewConfig())
	if err != nil {
		logger.WithError(err).Error("failed to initialize consumer")
		return
	}
	defer func() {
		if err = consumer.Close(); err != nil {
			logger.WithError(err).Error("failed to close the consumer")
		}
	}()

	partitionConsumer, err := consumer.ConsumePartition(identifier, 0, int64(counter-1))
	if err != nil {
		logger.WithError(err).Error("failed to initialize partition consumer")
		return
	}
	defer func() {
		if err = partitionConsumer.Close(); err != nil {
			logger.WithError(err).Error("failed to close the partition consumer")
		}
	}()

	select {
	case msg := <-partitionConsumer.Messages():
		event, err := events.New(msg.Value)
		if err != nil {
			logger.WithError(err).Error("failed to parse message as SSE event")
			return
		}
		event.ID = counter
		if err = event.Write(w); err != nil {
			logger.WithError(err).Error("failed to write event response")
		}
	case <-r.Context().Done():
		return
	}
}

// GetEvents is an endpoint for streaming events and logs from an ESR instance.
func (h SSEHandler) GetEvents(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	identifier := ps.ByName("identifier")

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

	receiver := make(chan events.Event) // Channel is closed in Subscriber
	go h.Subscribe(receiver, logger, r.Context(), lastID, identifier)

	for {
		select {
		case <-r.Context().Done():
			logger.Info("Client gone from SSE")
			return
		case <-h.ctx.Done():
			logger.Info("Shutting down")
			return
		case event := <-receiver:
			if err := event.Write(w); err != nil {
				logger.Error(err)
				continue
			}
			flusher.Flush()
		}
	}
}
