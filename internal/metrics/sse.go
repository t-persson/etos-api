// Copyright Axis Communications AB.
//
// For a full list of individual contributors, please see the commit history.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const apiVersion = "v2alpha"

const (
	reconnectParsed     = "parsed"
	reconnectParseError = "parse_error"

	writeSuccess = "write_success"
	writeError   = "write_error"

	requestCancelled = "request_context_cancelled"
	schemaInvalid    = "schema_invalid"
	parseError       = "parse_error"
)

var eventTypes = []string{"artifact", "error", "message", "ping", "report", "shutdown", "status", "unknown"}

// SSEMetrics holds Prometheus metrics owned by the SSE service. A nil *SSEMetrics is valid:
// recording methods do nothing and Handler serves an empty registry.
type SSEMetrics struct {
	registry       *prometheus.Registry
	buildInfo      *prometheus.GaugeVec
	reconnects     *prometheus.CounterVec
	retries        *prometheus.CounterVec
	disconnects    *prometheus.CounterVec
	eventDelivery  *prometheus.CounterVec
	eventsDropped  *prometheus.CounterVec
	terminalResult *prometheus.CounterVec
	etosVersion    string
}

// NewSSEMetrics creates a registry with version-labeled, preinitialized SSE metrics.
func NewSSEMetrics(etosVersion string) *SSEMetrics {
	if etosVersion == "" {
		etosVersion = "unknown"
	}

	m := &SSEMetrics{
		registry:    prometheus.NewRegistry(),
		etosVersion: etosVersion,
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "etos_sse_build_info",
			Help: "Build information for the ETOS SSE service.",
		}, []string{"etos_version"}),
		reconnects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "etos_sse_reconnects_total",
			Help: "SSE stream requests carrying Last-Event-ID, partitioned by whether the ID parsed successfully.",
		}, []string{"api_version", "outcome", "etos_version"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "etos_sse_retries_total",
			Help: "Retryable stream-closed error events written to SSE clients.",
		}, []string{"api_version", "outcome", "etos_version"}),
		disconnects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "etos_sse_disconnects_total",
			Help: "SSE streams ended after request cancellation or a response write error.",
		}, []string{"api_version", "outcome", "etos_version"}),
		eventDelivery: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "etos_sse_event_delivery_total",
			Help: "SSE event frames written to the response, partitioned by bounded event type and write outcome.",
		}, []string{"api_version", "event", "outcome", "etos_version"}),
		eventsDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "etos_sse_events_dropped_total",
			Help: "Events dropped before delivery because parsing or protocol validation failed.",
		}, []string{"api_version", "event", "outcome", "etos_version"}),
		terminalResult: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "etos_sse_terminal_results_total",
			Help: "Terminal shutdown result events handled at the SSE response boundary.",
		}, []string{"api_version", "outcome", "etos_version"}),
	}

	m.registry.MustRegister(
		m.buildInfo,
		m.reconnects,
		m.retries,
		m.disconnects,
		m.eventDelivery,
		m.eventsDropped,
		m.terminalResult,
	)
	m.buildInfo.WithLabelValues(etosVersion).Set(1)
	m.initializeSeries()
	return m
}

// Handler returns the Prometheus exposition handler for the SSE registry.
func (m *SSEMetrics) Handler() http.Handler {
	if m == nil {
		return promhttp.HandlerFor(prometheus.NewRegistry(), promhttp.HandlerOpts{})
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Reconnect records a request that includes Last-Event-ID.
func (m *SSEMetrics) Reconnect(validLastEventID bool) {
	if m == nil {
		return
	}
	outcome := reconnectParsed
	if !validLastEventID {
		outcome = reconnectParseError
	}
	m.reconnects.WithLabelValues(apiVersion, outcome, m.etosVersion).Inc()
}

// Retry records the result of writing a retryable stream-closed event.
func (m *SSEMetrics) Retry(writeSucceeded bool) {
	if m == nil {
		return
	}
	m.retries.WithLabelValues(apiVersion, writeOutcome(writeSucceeded), m.etosVersion).Inc()
}

// Disconnect records why an active SSE response ended unexpectedly.
func (m *SSEMetrics) Disconnect(outcome string) {
	if m == nil {
		return
	}
	if outcome != requestCancelled && outcome != writeError {
		return
	}
	m.disconnects.WithLabelValues(apiVersion, outcome, m.etosVersion).Inc()
}

// EventDelivery records the write result for a bounded SSE event type.
func (m *SSEMetrics) EventDelivery(event string, writeSucceeded bool) {
	if m == nil {
		return
	}
	m.eventDelivery.WithLabelValues(apiVersion, normalizeEvent(event), writeOutcome(writeSucceeded), m.etosVersion).Inc()
}

// EventDropped records an event rejected before it reaches the response writer.
func (m *SSEMetrics) EventDropped(event, outcome string) {
	if m == nil {
		return
	}
	if outcome != schemaInvalid && outcome != parseError {
		return
	}
	m.eventsDropped.WithLabelValues(apiVersion, normalizeEvent(event), outcome, m.etosVersion).Inc()
}

// TerminalResult records a terminal result event's response write outcome.
func (m *SSEMetrics) TerminalResult(outcome string) {
	if m == nil {
		return
	}
	if outcome != writeSuccess && outcome != writeError {
		return
	}
	m.terminalResult.WithLabelValues(apiVersion, outcome, m.etosVersion).Inc()
}

// initializeSeries makes all known outcomes visible before they occur.
func (m *SSEMetrics) initializeSeries() {
	for _, outcome := range []string{reconnectParsed, reconnectParseError} {
		m.reconnects.WithLabelValues(apiVersion, outcome, m.etosVersion).Add(0)
	}
	for _, outcome := range []string{writeSuccess, writeError} {
		m.retries.WithLabelValues(apiVersion, outcome, m.etosVersion).Add(0)
		m.terminalResult.WithLabelValues(apiVersion, outcome, m.etosVersion).Add(0)
	}
	for _, outcome := range []string{requestCancelled, writeError} {
		m.disconnects.WithLabelValues(apiVersion, outcome, m.etosVersion).Add(0)
	}
	for _, event := range eventTypes {
		for _, outcome := range []string{writeSuccess, writeError} {
			m.eventDelivery.WithLabelValues(apiVersion, event, outcome, m.etosVersion).Add(0)
		}
		for _, outcome := range []string{parseError, schemaInvalid} {
			m.eventsDropped.WithLabelValues(apiVersion, event, outcome, m.etosVersion).Add(0)
		}
	}
}

// normalizeEvent maps input to the bounded event vocabulary used by metrics.
func normalizeEvent(event string) string {
	for _, known := range eventTypes {
		if event == known {
			return known
		}
	}
	return "unknown"
}

// writeOutcome returns the bounded label for a response write result.
func writeOutcome(writeSucceeded bool) string {
	if writeSucceeded {
		return writeSuccess
	}
	return writeError
}
