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
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eiffel-community/etos-api/internal/config"
	"github.com/eiffel-community/etos-api/internal/metrics"
	"github.com/eiffel-community/etos-api/internal/stream"
	"github.com/eiffel-community/etos-api/pkg/events"
	"github.com/julienschmidt/httprouter"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type cfg struct {
	config.Config
}

// TestNewMetrics verifies explicit telemetry is reused and nil creates default metrics.
func TestNewMetrics(t *testing.T) {
	for _, name := range []string{"provided", "nil"} {
		t.Run(name, func(t *testing.T) {
			var telemetry *metrics.SSEMetrics
			if name == "provided" {
				telemetry = metrics.NewSSEMetrics()
			}
			app := New(context.Background(), cfg{}, logrus.New().WithField("test", t.Name()), nil, telemetry)
			t.Cleanup(app.cancel)
			if telemetry != nil {
				assert.Same(t, telemetry, app.metrics)
			} else {
				assert.NotNil(t, app.metrics)
			}
		})
	}
}

// failingStream fails consumption before delivering any event.
type failingStream struct{}

// WithChannel keeps the failure fixture unchanged.
func (s failingStream) WithChannel(chan<- stream.Message) stream.Stream { return s }

// WithOffset keeps the failure fixture unchanged.
func (s failingStream) WithOffset(int64) stream.Stream { return s }

// FirstOffset reports an empty stream start.
func (s failingStream) FirstOffset() (int64, error) { return 0, nil }

// Position reports the start of the stream.
func (s failingStream) Position() int64 { return 0 }

// WithFilter keeps the failure fixture unchanged.
func (s failingStream) WithFilter([]string) stream.Stream { return s }

// Consume simulates a failed broker subscription.
func (s failingStream) Consume(context.Context) (<-chan error, error) {
	return nil, errors.New("subscription failed")
}

// Close satisfies the fixture's stream interface.
func (s failingStream) Close() {}

// TestSubscribeErrorExitsWhenDeliveryIsCanceled verifies failed subscriptions exit on cancellation.
func TestSubscribeErrorExitsWhenDeliveryIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := logrus.New().WithField("test", t.Name())
	handler := &Handler{metrics: metrics.NewSSEMetrics()}
	ch := make(chan events.Event)
	var wg sync.WaitGroup
	wg.Go(func() {
		handler.subscribe(ctx, logger, failingStream{}, ch, 1, nil)
	})
	cancel()
	wg.Wait()
	_, ok := <-ch
	assert.False(t, ok)
}

func (c cfg) RabbitMQURI() string {
	return ""
}

func (c cfg) RabbitMQStreamName() string {
	return "test"
}

// TestSSEGetEvents tests that a client can subscribe to an SSE stream and get events.
func TestSSEGetEvents(t *testing.T) {
	t.Setenv("ETOS_CLUSTER", "")
	data := []byte(`{"event":"message","data":{"message":"hello world","name":"etos","@timestamp":"2026-08-31T10:00:00Z"}}`)
	testrunID := filepath.Join(t.TempDir(), "test_sse_get_events")
	assert.NoError(t, os.WriteFile(testrunID, data, 0644))
	ctx, done := context.WithTimeout(context.Background(), time.Second*1)
	defer done()

	log := logrus.WithFields(logrus.Fields{})
	streamer, err := stream.NewFileStreamer(100*time.Millisecond, log)
	assert.NoError(t, err)
	reader, provider := newTestMetrics()
	handler := Handler{logger: log, cfg: &cfg{}, ctx: context.Background(), streamer: streamer, metrics: metrics.NewSSEMetricsWithMeter(provider.Meter("test"))}
	responseRecorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", fmt.Sprintf("/v2alpha/events/%s", testrunID), nil)
	request.Header.Set("Last-Event-ID", "0")
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
	cluster := attribute.String("etos_cluster", "unassigned")
	assert.EqualValues(t, 0, sumValue(t, reader, "etos_sse.connections.active", cluster))
	assert.EqualValues(t, 1, sumValue(t, reader, "etos_sse.disconnects", cluster, attribute.String("reason", "client_disconnect")))
	assert.EqualValues(t, 1, sumValue(t, reader, "etos_sse.reconnects", cluster))
}

// newTestMetrics returns a meter provider whose measurements can be collected by the reader.
func newTestMetrics() (*sdkmetric.ManualReader, *sdkmetric.MeterProvider) {
	reader := sdkmetric.NewManualReader()
	return reader, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
}

// sumValue returns the counter value for the attributes, or -1 when absent.
func sumValue(t *testing.T, reader *sdkmetric.ManualReader, name string, attrs ...attribute.KeyValue) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	want := attribute.NewSet(attrs...)
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == name {
				for _, p := range sum.DataPoints {
					if p.Attributes.Equals(&want) {
						return p.Value
					}
				}
			}
		}
	}
	return -1
}

// TestSSEGetEventsDropsInvalid tests that events which do not match the protocol
// are dropped and never forwarded to the client.
func TestSSEGetEventsDropsInvalid(t *testing.T) {
	// A "message" event whose data is a plain string does not match the Log
	// protocol and must be dropped.
	data := []byte(`{"event":"message","data":"hello world"}`)
	testrunID := filepath.Join(t.TempDir(), "test_sse_drops_invalid")
	assert.NoError(t, os.WriteFile(testrunID, data, 0644))
	ctx, done := context.WithTimeout(context.Background(), time.Second*1)
	defer done()

	log := logrus.WithFields(logrus.Fields{})
	streamer, err := stream.NewFileStreamer(100*time.Millisecond, log)
	assert.NoError(t, err)
	handler := Handler{logger: log, cfg: &cfg{}, ctx: context.Background(), streamer: streamer, metrics: metrics.NewSSEMetrics()}
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
	t.Setenv("ETOS_CLUSTER", "")
	backendErr := errors.New("dial tcp rabbitmq.internal:5552: connection refused")
	log := logrus.WithFields(logrus.Fields{})
	reader, provider := newTestMetrics()
	handler := Handler{logger: log, cfg: &cfg{}, ctx: context.Background(), streamer: unavailableStreamer{err: backendErr}, metrics: metrics.NewSSEMetricsWithMeter(provider.Meter("test"))}
	responseRecorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/v2alpha/events/test_sse_stream_unavailable", nil)
	ps := httprouter.Params{httprouter.Param{Key: "identifier", Value: "test_sse_stream_unavailable"}}
	handler.GetEvents(responseRecorder, request, ps)

	assert.Equal(t, http.StatusServiceUnavailable, responseRecorder.Code)
	assert.Equal(t, "text/plain; charset=utf-8", responseRecorder.Header().Get("Content-Type"))
	body := responseRecorder.Body.String()
	assert.Equal(t, "event stream is temporarily unavailable\n", body)
	assert.NotContains(t, body, "rabbitmq.internal")
	assert.EqualValues(t, 1, sumValue(t, reader, "etos_sse.errors", attribute.String("etos_cluster", "unassigned"), attribute.String("category", "backend_failure")))
}

// TestSSEGetEventsShuttingDown tests that a stream is not started when the application is
// shutting down, and that the client is told to retry.
func TestSSEGetEventsShuttingDown(t *testing.T) {
	appCtx, stop := context.WithCancel(context.Background())
	stop()

	log := logrus.WithFields(logrus.Fields{})
	streamer, err := stream.NewFileStreamer(100*time.Millisecond, log)
	assert.NoError(t, err)
	handler := Handler{logger: log, cfg: &cfg{}, ctx: appCtx, streamer: streamer, metrics: metrics.NewSSEMetrics()}
	responseRecorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/v2alpha/events/test_sse_shutting_down", nil)
	ps := httprouter.Params{httprouter.Param{Key: "identifier", Value: "test_sse_shutting_down"}}
	handler.GetEvents(responseRecorder, request, ps)

	assert.Equal(t, http.StatusServiceUnavailable, responseRecorder.Code)
}

// TestSSEGetEventsShutdownAbortsStream tests that an active stream is aborted, rather than
// ended cleanly, when the application shuts down, so that clients reconnect.
func TestSSEGetEventsShutdownAbortsStream(t *testing.T) {
	data := []byte(`{"event":"message","data":{"message":"hello world","name":"etos","@timestamp":"2026-08-31T10:00:00Z"}}`)
	testrunID := "test_sse_shutdown_aborts_stream"
	os.WriteFile(testrunID, data, 0644)
	defer func() {
		os.Remove(testrunID)
	}()
	appCtx, stop := context.WithCancel(context.Background())
	defer stop()

	log := logrus.WithFields(logrus.Fields{})
	streamer, err := stream.NewFileStreamer(10*time.Millisecond, log)
	assert.NoError(t, err)
	handler := Handler{logger: log, cfg: &cfg{}, ctx: appCtx, streamer: streamer, metrics: metrics.NewSSEMetrics()}
	router := httprouter.New()
	router.GET("/v2alpha/events/:identifier", handler.GetEvents)
	server := httptest.NewServer(router)
	defer server.Close()

	response, err := http.Get(fmt.Sprintf("%s/v2alpha/events/%s", server.URL, testrunID))
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)

	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	assert.NoError(t, err)
	assert.Equal(t, "id: 1\n", line)

	stop()
	result := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(reader)
		result <- err
	}()
	select {
	case err := <-result:
		// A cleanly ended stream would return a nil error here.
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	case <-time.After(5 * time.Second):
		t.Fatal("stream was not closed after the application shut down")
	}
}

type controlledStreamer struct {
	stream *controlledStream
}

// NewStream returns the test stream.
func (s *controlledStreamer) NewStream(context.Context, *logrus.Entry, string) (stream.Stream, error) {
	return s.stream, nil
}

// CreateStream is not needed by this test streamer.
func (s *controlledStreamer) CreateStream(context.Context, *logrus.Entry, string) error { return nil }

// Close is not needed by this test streamer.
func (s *controlledStreamer) Close() {}

type controlledStream struct {
	channel      chan<- stream.Message
	channelReady chan struct{}
	closed       chan struct{}
	once         sync.Once
}

// WithChannel records the channel used to forward data.
func (s *controlledStream) WithChannel(ch chan<- stream.Message) stream.Stream {
	s.channel = ch
	close(s.channelReady)
	return s
}

// WithOffset keeps the existing test stream.
func (s *controlledStream) WithOffset(int64) stream.Stream { return s }

// FirstOffset reports a stream that starts at the first offset.
func (s *controlledStream) FirstOffset() (int64, error) { return 0, nil }

// Position reports no progress.
func (s *controlledStream) Position() int64 { return 0 }

// WithFilter keeps the existing test stream.
func (s *controlledStream) WithFilter([]string) stream.Stream { return s }

// Consume starts a test subscription without contacting RabbitMQ.
func (s *controlledStream) Consume(context.Context) (<-chan error, error) {
	return make(chan error), nil
}

// Close records when the subscriber releases its consumer.
func (s *controlledStream) Close() {
	s.once.Do(func() { close(s.closed) })
}

type failingWriter struct {
	httptest.ResponseRecorder
	written chan struct{}
}

type blockingWriter struct {
	httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
}

// Write waits until the test releases its simulated slow client.
func (w *blockingWriter) Write(data []byte) (int, error) {
	select {
	case <-w.entered:
	default:
		close(w.entered)
	}
	<-w.release
	return w.ResponseRecorder.Write(data)
}

// Write reports a failed response and signals the test.
func (w *failingWriter) Write([]byte) (int, error) {
	close(w.written)
	return 0, errors.New("client disconnected")
}

// TestSubscribeCancelUnblocksEventSend checks that an unbuffered send to the
// handler stops on cancellation and the stream consumer is released.
func TestSubscribeCancelUnblocksEventSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &controlledStream{closed: make(chan struct{}), channelReady: make(chan struct{})}
	receiver := make(chan events.Event)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		(&Handler{metrics: metrics.NewSSEMetrics()}).subscribe(ctx, logrus.NewEntry(logrus.New()), s, receiver, 1, nil)
	}()
	select {
	case <-s.channelReady:
	case <-time.After(time.Second):
		t.Fatal("subscriber did not start")
	}
	sent := make(chan struct{})
	go func() {
		s.channel <- stream.Message{Data: []byte(`{"event":"message","data":{"message":"hello","name":"etos","@timestamp":"2026-08-31T10:00:00Z"}}`)}
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive the message")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("subscriber blocked sending to the disconnected handler")
	}
	select {
	case <-s.closed:
	default:
		t.Fatal("stream consumer was not closed")
	}
}

// TestGetEventsWriteFailureClosesStream checks that a failed HTTP write ends
// the request and closes its stream instead of continuing to consume.
func TestGetEventsWriteFailureClosesStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s := &controlledStream{closed: make(chan struct{}), channelReady: make(chan struct{})}
	h := Handler{logger: logrus.NewEntry(logrus.New()), ctx: context.Background(), streamer: &controlledStreamer{stream: s}, metrics: metrics.NewSSEMetrics()}
	w := &failingWriter{written: make(chan struct{})}
	request := httptest.NewRequest(http.MethodGet, "/sse/v2alpha/events/run", nil).WithContext(ctx)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		h.GetEvents(w, request, httprouter.Params{{Key: "identifier", Value: "run"}})
	}()
	select {
	case <-s.channelReady:
	case <-time.After(time.Second):
		t.Fatal("subscriber did not start")
	}

	sent := make(chan struct{})
	go func() {
		s.channel <- stream.Message{Data: []byte(`{"event":"message","data":{"message":"hello","name":"etos","@timestamp":"2026-08-31T10:00:00Z"}}`)}
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive the message")
	}
	select {
	case <-w.written:
	case <-time.After(time.Second):
		t.Fatal("writer was not called")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("handler continued after a failed write")
	}
	select {
	case <-s.closed:
	case <-time.After(time.Second):
		t.Fatal("stream consumer was not closed")
	}
}

// TestGetEventsShutdownWithSlowWriters verifies that application shutdown releases
// every subscription even while HTTP writers are blocked by slow clients.
func TestGetEventsShutdownWithSlowWriters(t *testing.T) {
	before := runtime.NumGoroutine()
	appCtx, shutdown := context.WithCancel(context.Background())
	const clients = 32
	streams := make([]*controlledStream, clients)
	writers := make([]*blockingWriter, clients)
	finished := make([]chan struct{}, clients)
	for i := range streams {
		s := &controlledStream{closed: make(chan struct{}), channelReady: make(chan struct{})}
		streams[i] = s
		w := &blockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
		writers[i] = w
		finished[i] = make(chan struct{})
		h := Handler{logger: logrus.NewEntry(logrus.New()), ctx: appCtx, streamer: &controlledStreamer{stream: s}, metrics: metrics.NewSSEMetrics()}
		go func(done chan struct{}) {
			defer close(done)
			// Shutdown aborts the response with http.ErrAbortHandler.
			defer func() { recover() }()
			h.GetEvents(w, httptest.NewRequest(http.MethodGet, "/sse/v2alpha/events/run", nil),
				httprouter.Params{{Key: "identifier", Value: "run"}})
		}(finished[i])
		select {
		case <-s.channelReady:
		case <-time.After(time.Second):
			t.Fatal("subscriber did not start")
		}
		s.channel <- stream.Message{Data: []byte(`{"event":"message","data":{"message":"hello","name":"etos","@timestamp":"2026-08-31T10:00:00Z"}}`)}
		select {
		case <-w.entered:
		case <-time.After(time.Second):
			t.Fatal("writer did not receive the event")
		}
	}
	for _, s := range streams {
		s.channel <- stream.Message{Data: []byte(`{"event":"message","data":{"message":"again","name":"etos","@timestamp":"2026-08-31T10:00:00Z"}}`)}
	}
	shutdown()
	for i, s := range streams {
		select {
		case <-s.closed:
		case <-time.After(time.Second):
			t.Errorf("subscriber %d did not close on shutdown", i)
		}
		close(writers[i].release)
		select {
		case <-finished[i]:
		case <-time.After(time.Second):
			t.Errorf("handler %d did not finish after writer unblocked", i)
		}
	}
	after := runtime.NumGoroutine()
	t.Logf("slow-writer churn: %d concurrent subscriptions; goroutines before=%d after=%d", clients, before, after)
	assert.LessOrEqual(t, after, before+2)
}

// getEvents runs GetEvents against a file stream containing lines until the timeout and returns
// the response body.
func getEvents(t *testing.T, testrunID string, lines string, lastEventID string) string {
	t.Helper()
	assert.NoError(t, os.WriteFile(testrunID, []byte(lines), 0644))
	t.Cleanup(func() { _ = os.Remove(testrunID) })
	ctx, done := context.WithTimeout(context.Background(), time.Second*1)
	defer done()

	log := logrus.WithFields(logrus.Fields{})
	streamer, err := stream.NewFileStreamer(10*time.Millisecond, log)
	assert.NoError(t, err)
	handler := Handler{logger: log, cfg: &cfg{}, ctx: context.Background(), streamer: streamer, metrics: metrics.NewSSEMetrics()}
	responseRecorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", fmt.Sprintf("/v2alpha/events/%s", testrunID), nil)
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	request = request.WithContext(ctx)
	ps := httprouter.Params{httprouter.Param{Key: "identifier", Value: testrunID}}
	handler.GetEvents(responseRecorder, request, ps)
	assert.Equal(t, http.StatusOK, responseRecorder.Code)
	return responseRecorder.Body.String()
}

const (
	invalidLine = `{"event":"message","data":"invalid"}`
	shutdownA   = `{"event":"shutdown","data":{"conclusion":"Successful","verdict":"Passed","description":"a"}}`
	shutdownB   = `{"event":"shutdown","data":{"conclusion":"Successful","verdict":"Passed","description":"b"}}`
)

// TestSSEGetEventsOffsetIDs tests that event IDs are the stream offset plus one and that
// dropped events do not shift the IDs of other events.
func TestSSEGetEventsOffsetIDs(t *testing.T) {
	body := getEvents(t, "test_sse_offset_ids", invalidLine+"\n"+shutdownA+"\n"+shutdownB+"\n", "")
	assert.NotContains(t, body, "invalid")
	assert.Contains(t, body, "id: 2\nevent: shutdown\ndata: {\"conclusion\":\"Successful\",\"description\":\"a\"")
	assert.Contains(t, body, "id: 3\nevent: shutdown\ndata: {\"conclusion\":\"Successful\",\"description\":\"b\"")
	assert.NotContains(t, body, "id: 1\n")
}

// TestSSEGetEventsResume tests that a client resuming with Last-Event-ID only receives the
// events after that ID.
func TestSSEGetEventsResume(t *testing.T) {
	body := getEvents(t, "test_sse_resume", invalidLine+"\n"+shutdownA+"\n"+shutdownB+"\n", "2")
	assert.NotContains(t, body, `"description":"a"`)
	assert.Contains(t, body, "id: 3\nevent: shutdown\ndata: {\"conclusion\":\"Successful\",\"description\":\"b\"")
}

// TestSSEGetEventsMalformedLastEventID tests that a Last-Event-ID that cannot be parsed makes
// the server stream from the start.
func TestSSEGetEventsMalformedLastEventID(t *testing.T) {
	for _, lastEventID := range []string{"not-a-number", "-5"} {
		t.Run(lastEventID, func(t *testing.T) {
			body := getEvents(t, "test_sse_malformed_last_event_id", shutdownA+"\n", lastEventID)
			assert.Contains(t, body, "id: 1\nevent: shutdown\n")
		})
	}
}

// fakeStream is a stream.Stream with a fixed first offset and position that never delivers
// any messages.
type fakeStream struct {
	// firstAfterConsume, if set, is the first offset once Consume has been called.
	firstAfterConsume int64
	consumeErr        error
	first             int64
	firstErr          error
	position          int64
	offset            int64
	consumed          bool
}

func (s *fakeStream) WithChannel(chan<- stream.Message) stream.Stream { return s }
func (s *fakeStream) WithOffset(offset int64) stream.Stream           { s.offset = offset; return s }
func (s *fakeStream) WithFilter([]string) stream.Stream               { return s }
func (s *fakeStream) Consume(context.Context) (<-chan error, error) {
	if s.consumeErr != nil {
		return nil, s.consumeErr
	}
	s.consumed = true
	if s.firstAfterConsume != 0 {
		s.first = s.firstAfterConsume
	}
	return make(chan error), nil
}
func (s *fakeStream) FirstOffset() (int64, error) { return s.first, s.firstErr }
func (s *fakeStream) Position() int64             { return s.position }
func (s *fakeStream) Close()                      {}

// fakeStreamer is a stream.Streamer returning a fakeStream.
type fakeStreamer struct {
	stream *fakeStream
}

func (s fakeStreamer) NewStream(context.Context, *logrus.Entry, string) (stream.Stream, error) {
	return s.stream, nil
}
func (s fakeStreamer) CreateStream(context.Context, *logrus.Entry, string) error { return nil }
func (s fakeStreamer) Close()                                                    {}

// getFakeEvents runs GetEvents against a fakeStream until the timeout and returns the body.
func getFakeEvents(t *testing.T, fake *fakeStream, lastEventID string, timeout time.Duration) string {
	t.Helper()
	ctx, done := context.WithTimeout(context.Background(), timeout)
	defer done()
	log := logrus.WithFields(logrus.Fields{})
	handler := Handler{logger: log, cfg: &cfg{}, ctx: context.Background(), streamer: fakeStreamer{fake}, metrics: metrics.NewSSEMetrics()}
	responseRecorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/v2alpha/events/fake", nil)
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	request = request.WithContext(ctx)
	ps := httprouter.Params{httprouter.Param{Key: "identifier", Value: "fake"}}
	handler.GetEvents(responseRecorder, request, ps)
	assert.Equal(t, http.StatusOK, responseRecorder.Code)
	return responseRecorder.Body.String()
}

// TestSSEGetEventsResumeExpired tests that resuming from an event that has been removed from
// the stream by retention is a non-retryable error.
func TestSSEGetEventsResumeExpired(t *testing.T) {
	fake := &fakeStream{first: 10}
	body := getFakeEvents(t, fake, "5", time.Second)
	assert.Equal(t, "event: error\ndata: {\"retry\":false,\"reason\":\""+reasonExpired+"\"}\n\n", body)
	assert.False(t, fake.consumed)
}

// TestSSEGetEventsResumeUnknown tests that resuming an empty stream is a non-retryable error.
func TestSSEGetEventsResumeUnknown(t *testing.T) {
	fake := &fakeStream{firstErr: stream.ErrEmptyStream}
	body := getFakeEvents(t, fake, "5", time.Second)
	assert.Equal(t, "event: error\ndata: {\"retry\":false,\"reason\":\""+reasonUnknown+"\"}\n\n", body)
	assert.False(t, fake.consumed)
}

// TestSSEGetEventsResumeFromFirstOffset tests that a client can resume from the first offset
// retained in the stream and that the stream is consumed from the last event ID.
func TestSSEGetEventsResumeFromFirstOffset(t *testing.T) {
	fake := &fakeStream{first: 10}
	body := getFakeEvents(t, fake, "10", 100*time.Millisecond)
	assert.NotContains(t, body, "error")
	assert.True(t, fake.consumed)
	assert.Equal(t, int64(10), fake.offset)
}

// TestSSEGetEventsPingIDs tests that pings carry the stream position as ID only when it is
// greater than the last event ID sent to the client.
func TestSSEGetEventsPingIDs(t *testing.T) {
	original := pingInterval
	pingInterval = 20 * time.Millisecond
	t.Cleanup(func() { pingInterval = original })

	t.Run("position ahead of client", func(t *testing.T) {
		body := getFakeEvents(t, &fakeStream{position: 50}, "", 70*time.Millisecond)
		assert.True(t, strings.HasPrefix(body, "id: 50\nevent: ping\ndata: \n\nevent: ping\ndata: \n\n"), body)
	})
	t.Run("position equal to client", func(t *testing.T) {
		body := getFakeEvents(t, &fakeStream{position: 50}, "50", 70*time.Millisecond)
		assert.True(t, strings.HasPrefix(body, "event: ping\ndata: \n\n"), body)
		assert.NotContains(t, body, "id:")
	})
}

// TestSSEGetEventsResumeExpiredAfterConsume verifies that events expiring between the resume
// check and the start of consumption result in a non-retryable error.
func TestSSEGetEventsResumeExpiredAfterConsume(t *testing.T) {
	fake := &fakeStream{first: 5, firstAfterConsume: 10}
	body := getFakeEvents(t, fake, "5", time.Second)
	assert.Equal(t, "event: error\ndata: {\"retry\":false,\"reason\":\""+reasonExpired+"\"}\n\n", body)
	assert.True(t, fake.consumed)
}

// TestSSEGetEventsConsumeError verifies that a failure to start consuming the stream results in a
// retryable error that does not expose the underlying error to the client.
func TestSSEGetEventsConsumeError(t *testing.T) {
	fake := &fakeStream{consumeErr: errors.New("broker internals")}
	body := getFakeEvents(t, fake, "", time.Second)
	assert.Equal(t, "event: error\ndata: {\"retry\":true,\"reason\":\"failed to consume the event stream\"}\n\n", body)
}
