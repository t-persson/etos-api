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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/eiffel-community/etos-api/internal/config"
	ssemetrics "github.com/eiffel-community/etos-api/internal/metrics"
	"github.com/eiffel-community/etos-api/internal/stream"
	"github.com/julienschmidt/httprouter"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cfg struct {
	config.Config
}

func (c cfg) RabbitMQURI() string {
	return ""
}

func (c cfg) RabbitMQStreamName() string {
	return "test"
}

// TestSSEGetEvents tests that a client can subscribe to an SSE stream and get events.
func TestSSEGetEvents(t *testing.T) {
	data := []byte(`{"event":"message","data":{"message":"hello world","name":"etos","@timestamp":"2026-08-31T10:00:00Z"}}`)
	testrunID := "test_sse_get_events"
	os.WriteFile(testrunID, data, 0644)
	defer func() {
		os.Remove(testrunID)
	}()
	ctx, done := context.WithTimeout(context.Background(), time.Second*1)
	defer done()

	log := logrus.WithFields(logrus.Fields{})
	streamer, err := stream.NewFileStreamer(100*time.Millisecond, log)
	assert.NoError(t, err)
	handler, _ := newTestHandler(streamer)
	responseRecorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", fmt.Sprintf("/v2alpha/events/%s", testrunID), nil)
	request = request.WithContext(ctx)
	ps := httprouter.Params{httprouter.Param{Key: "identifier", Value: testrunID}}
	handler.GetEvents(responseRecorder, request, ps)

	assert.Equal(t, http.StatusOK, responseRecorder.Code)
	body, err := io.ReadAll(responseRecorder.Body)
	assert.NoError(t, err)
	fmt.Println(string(body))
	assert.Equal(t, body, []byte(`id: 1
event: message
data: {"@timestamp":"2026-08-31T10:00:00Z","message":"hello world","name":"etos"}

`))
	assert.Contains(t, scrapeTestMetrics(t, handler.metrics), `etos_version="9.1.0",event="message",outcome="write_success"} 1`)
}

// TestSSEGetEventsDropsInvalid tests that events which do not match the protocol
// are dropped and never forwarded to the client.
func TestSSEGetEventsDropsInvalid(t *testing.T) {
	// A "message" event whose data is a plain string does not match the Log
	// protocol and must be dropped.
	data := []byte(`{"event":"message","data":"hello world"}`)
	testrunID := "test_sse_drops_invalid"
	os.WriteFile(testrunID, data, 0644)
	defer func() {
		os.Remove(testrunID)
	}()
	ctx, done := context.WithTimeout(context.Background(), time.Second*1)
	defer done()

	log := logrus.WithFields(logrus.Fields{})
	streamer, err := stream.NewFileStreamer(100*time.Millisecond, log)
	assert.NoError(t, err)
	handler, _ := newTestHandler(streamer)
	responseRecorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", fmt.Sprintf("/v2alpha/events/%s", testrunID), nil)
	request = request.WithContext(ctx)
	ps := httprouter.Params{httprouter.Param{Key: "identifier", Value: testrunID}}
	handler.GetEvents(responseRecorder, request, ps)

	assert.Equal(t, http.StatusOK, responseRecorder.Code)
	body, err := io.ReadAll(responseRecorder.Body)
	assert.NoError(t, err)
	// The invalid event is dropped, so no data event is written to the client.
	assert.NotContains(t, string(body), "hello world")
	assert.Contains(t, scrapeTestMetrics(t, handler.metrics), `etos_version="9.1.0",event="message",outcome="schema_invalid"} 1`)
}

// TestSSEGetEventsInvalidShutdownIsNotTerminal checks that a rejected shutdown event is
// counted as dropped and not as a terminal result, since the stream is not closed.
func TestSSEGetEventsInvalidShutdownIsNotTerminal(t *testing.T) {
	data := []byte(`{"event":"shutdown","data":"not a result"}`)
	testrunID := "test_sse_invalid_shutdown"
	require.NoError(t, os.WriteFile(testrunID, data, 0644))
	defer func() {
		assert.NoError(t, os.Remove(testrunID))
	}()
	ctx, done := context.WithTimeout(context.Background(), time.Second*1)
	defer done()

	log := logrus.WithFields(logrus.Fields{})
	streamer, err := stream.NewFileStreamer(100*time.Millisecond, log)
	assert.NoError(t, err)
	handler, _ := newTestHandler(streamer)
	responseRecorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", fmt.Sprintf("/v2alpha/events/%s", testrunID), nil)
	request = request.WithContext(ctx)
	ps := httprouter.Params{httprouter.Param{Key: "identifier", Value: testrunID}}
	handler.GetEvents(responseRecorder, request, ps)

	metrics := scrapeTestMetrics(t, handler.metrics)
	assert.NotContains(t, responseRecorder.Body.String(), "event: shutdown")
	assert.Contains(t, metrics, `etos_version="9.1.0",event="shutdown",outcome="schema_invalid"} 1`)
	assert.NotContains(t, metrics, `etos_sse_terminal_results_total{api_version="v2alpha",etos_version="9.1.0",outcome="schema_invalid"}`)
	assert.Contains(t, metrics, `etos_sse_terminal_results_total{api_version="v2alpha",etos_version="9.1.0",outcome="write_success"} 0`)
}

// oneEventStreamer supplies one event or simulates a closed stream for handler tests.
type oneEventStreamer struct {
	event            []byte
	closeImmediately bool
}

// NewStream returns a stream that emits its configured event.
func (s oneEventStreamer) NewStream(context.Context, *logrus.Entry, string) (stream.Stream, error) {
	return &oneEventStream{event: s.event, closeImmediately: s.closeImmediately}, nil
}

// CreateStream does nothing.
func (s oneEventStreamer) CreateStream(context.Context, *logrus.Entry, string) error { return nil }

// Close does nothing.
func (s oneEventStreamer) Close() {}

type oneEventStream struct {
	event            []byte
	channel          chan<- []byte
	closeImmediately bool
}

// WithChannel configures the event destination.
func (s *oneEventStream) WithChannel(channel chan<- []byte) stream.Stream {
	s.channel = channel
	return s
}

// WithOffset keeps the configured stream offset unchanged for this test stream.
func (s *oneEventStream) WithOffset(int) stream.Stream { return s }

// WithFilter keeps the configured stream filter unchanged for this test stream.
func (s *oneEventStream) WithFilter([]string) stream.Stream { return s }

// Consume emits the configured event and waits for cancellation, or reports a closed stream.
func (s *oneEventStream) Consume(ctx context.Context) (<-chan error, error) {
	closed := make(chan error, 1)
	if s.closeImmediately {
		closed <- errors.New("test stream closed")
		return closed, nil
	}
	go func() {
		if s.event != nil {
			select {
			case s.channel <- s.event:
			case <-ctx.Done():
				return
			}
		}
		<-ctx.Done()
	}()
	return closed, nil
}

// Close does nothing.
func (s *oneEventStream) Close() {}

// TestSSEGetEventsReconnectAndTerminalResult records a resume and a shutdown result write.
func TestSSEGetEventsReconnectAndTerminalResult(t *testing.T) {
	terminal := []byte(`{"event":"shutdown","data":{"conclusion":"Successful","verdict":"Passed","description":"done"}}`)
	handler, metrics := newTestHandler(oneEventStreamer{event: terminal})
	request := httptest.NewRequest("GET", "/v2alpha/events/testrun-private-id", nil)
	request.Header.Set("Last-Event-ID", "1")
	ctx, cancel := context.WithTimeout(request.Context(), 40*time.Millisecond)
	defer cancel()
	request = request.WithContext(ctx)
	writer := httptest.NewRecorder()
	params := httprouter.Params{{Key: "identifier", Value: "testrun-private-id"}}

	handler.GetEvents(writer, request, params)

	assert.Contains(t, writer.Body.String(), "event: shutdown")
	body := scrapeTestMetrics(t, metrics)
	assert.Contains(t, body, `etos_version="9.1.0",outcome="parsed"} 1`)
	assert.Contains(t, body, `etos_sse_terminal_results_total{api_version="v2alpha",etos_version="9.1.0",outcome="write_success"} 1`)
	assert.Contains(t, body, `etos_sse_disconnects_total{api_version="v2alpha",etos_version="9.1.0",outcome="request_context_cancelled"} 0`)
	assert.NotContains(t, body, "testrun-private-id")
}

// TestSSEGetEventsMalformedReconnectRecordsBoundedOutcome verifies malformed IDs retain existing stream behavior.
func TestSSEGetEventsMalformedReconnectRecordsBoundedOutcome(t *testing.T) {
	message := []byte(`{"event":"message","data":{"message":"hello","name":"etos","@timestamp":"2026-08-31T10:00:00Z"}}`)
	handler, metrics := newTestHandler(oneEventStreamer{event: message})
	request := httptest.NewRequest("GET", "/v2alpha/events/testrun-private-id", nil)
	request.Header.Set("Last-Event-ID", "private-event-id")
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Millisecond)
	defer cancel()
	request = request.WithContext(ctx)
	writer := httptest.NewRecorder()
	params := httprouter.Params{{Key: "identifier", Value: "testrun-private-id"}}

	handler.GetEvents(writer, request, params)

	assert.Equal(t, http.StatusOK, writer.Code)
	assert.Contains(t, writer.Body.String(), "event: message")
	body := scrapeTestMetrics(t, metrics)
	assert.Contains(t, body, `etos_sse_reconnects_total{api_version="v2alpha",etos_version="9.1.0",outcome="parse_error"} 1`)
	assert.NotContains(t, body, "private-event-id")
}

// TestSSEGetEventsRequestCancellationCountsDisconnect verifies early request cancellation is distinct from final-result closure.
func TestSSEGetEventsRequestCancellationCountsDisconnect(t *testing.T) {
	handler, metrics := newTestHandler(oneEventStreamer{})
	request := httptest.NewRequest("GET", "/v2alpha/events/testrun-private-id", nil)
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Millisecond)
	defer cancel()
	request = request.WithContext(ctx)
	params := httprouter.Params{{Key: "identifier", Value: "testrun-private-id"}}

	handler.GetEvents(httptest.NewRecorder(), request, params)

	assert.Contains(t, scrapeTestMetrics(t, metrics), `etos_sse_disconnects_total{api_version="v2alpha",etos_version="9.1.0",outcome="request_context_cancelled"} 1`)
}

// TestSSEGetEventsRetryCountsRetryableErrorWrites verifies retry metrics reflect emitted retry events.
func TestSSEGetEventsRetryCountsRetryableErrorWrites(t *testing.T) {
	handler, metrics := newTestHandler(oneEventStreamer{closeImmediately: true})
	writer := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/v2alpha/events/testrun-private-id", nil)
	params := httprouter.Params{{Key: "identifier", Value: "testrun-private-id"}}

	handler.GetEvents(writer, request, params)

	assert.Contains(t, writer.Body.String(), `"retry":true`)
	assert.Contains(t, scrapeTestMetrics(t, metrics), `etos_sse_retries_total{api_version="v2alpha",etos_version="9.1.0",outcome="write_success"} 1`)
}

type failingResponseWriter struct {
	header http.Header
}

// Header returns the headers for a failing response writer.
func (w *failingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

// Write simulates a client disconnect during an SSE frame write.
func (w *failingResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}

// WriteHeader does nothing for a response that cannot be written.
func (w *failingResponseWriter) WriteHeader(int) {}

// Flush does nothing for a response that cannot be written.
func (w *failingResponseWriter) Flush() {}

// TestSSEGetEventsTerminalWriteFailureCountsDrop verifies a terminal result write failure is visible.
func TestSSEGetEventsTerminalWriteFailureCountsDrop(t *testing.T) {
	terminal := []byte(`{"event":"shutdown","data":{"conclusion":"Failed","verdict":"Failed","description":"failed"}}`)
	handler, metrics := newTestHandler(oneEventStreamer{event: terminal})
	request := httptest.NewRequest("GET", "/v2alpha/events/testrun-private-id", nil)
	params := httprouter.Params{{Key: "identifier", Value: "testrun-private-id"}}

	handler.GetEvents(&failingResponseWriter{}, request, params)

	body := scrapeTestMetrics(t, metrics)
	assert.Contains(t, body, `etos_sse_terminal_results_total{api_version="v2alpha",etos_version="9.1.0",outcome="write_error"} 1`)
	assert.Contains(t, body, `etos_sse_disconnects_total{api_version="v2alpha",etos_version="9.1.0",outcome="write_error"} 1`)
	assert.Contains(t, body, `etos_version="9.1.0",event="shutdown",outcome="write_error"} 1`)
	assert.NotContains(t, body, "testrun-private-id")
}

// newTestHandler creates a v2alpha handler with an isolated metrics registry.
func newTestHandler(streamer stream.Streamer) (Handler, *ssemetrics.SSEMetrics) {
	log := logrus.WithFields(logrus.Fields{})
	metrics := ssemetrics.NewSSEMetrics("9.1.0")
	return Handler{log, &cfg{}, context.Background(), streamer, metrics}, metrics
}

// scrapeTestMetrics returns the exposition from the handler's metrics registry.
func scrapeTestMetrics(t *testing.T, metrics *ssemetrics.SSEMetrics) string {
	t.Helper()
	writer := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(writer, httptest.NewRequest("GET", "/metrics", nil))
	return writer.Body.String()
}

// unavailableStreamer is a stream.Streamer whose NewStream always fails, simulating a
// broker that cannot be reached when a client subscribes.
type unavailableStreamer struct {
	err error
}

// NewStream returns the configured error.
func (s unavailableStreamer) NewStream(context.Context, *logrus.Entry, string) (stream.Stream, error) {
	return nil, s.err
}

// CreateStream does nothing.
func (s unavailableStreamer) CreateStream(context.Context, *logrus.Entry, string) error {
	return nil
}

// Close does nothing.
func (s unavailableStreamer) Close() {}

// TestSSEGetEventsStreamUnavailable tests that a failure to open a stream is reported as
// a retryable 503 Service Unavailable, without leaking the backend error to the client.
func TestSSEGetEventsStreamUnavailable(t *testing.T) {
	backendErr := errors.New("dial tcp rabbitmq.internal:5552: connection refused")
	handler, _ := newTestHandler(unavailableStreamer{err: backendErr})
	responseRecorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/v2alpha/events/test_sse_stream_unavailable", nil)
	ps := httprouter.Params{httprouter.Param{Key: "identifier", Value: "test_sse_stream_unavailable"}}
	handler.GetEvents(responseRecorder, request, ps)

	assert.Equal(t, http.StatusServiceUnavailable, responseRecorder.Code)
	assert.Equal(t, "text/plain; charset=utf-8", responseRecorder.Header().Get("Content-Type"))
	body := responseRecorder.Body.String()
	assert.Equal(t, "event stream is temporarily unavailable\n", body)
	assert.NotContains(t, body, "rabbitmq.internal")
}
