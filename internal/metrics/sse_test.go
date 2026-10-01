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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSSEMetricsPreinitializeVersionedSeries verifies zero-valued series and ETOS version labels.
func TestSSEMetricsPreinitializeVersionedSeries(t *testing.T) {
	metrics := NewSSEMetrics("9.1.0")
	body := scrape(t, metrics)

	require.Contains(t, body, `etos_sse_build_info{etos_version="9.1.0"} 1`)
	require.Contains(t, body, `etos_sse_reconnects_total{api_version="v2alpha",etos_version="9.1.0",outcome="parsed"} 0`)
	require.Contains(t, body, `etos_sse_terminal_results_total{api_version="v2alpha",etos_version="9.1.0",outcome="write_success"} 0`)
	require.Contains(t, body, `etos_sse_event_delivery_total{api_version="v2alpha",etos_version="9.1.0",event="shutdown",outcome="write_error"} 0`)
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "etos_sse_") {
			require.Contains(t, line, `etos_version="9.1.0"`)
		}
	}
}

// TestSSEMetricsBoundEventLabels verifies event input cannot create unbounded metric labels.
func TestSSEMetricsBoundEventLabels(t *testing.T) {
	metrics := NewSSEMetrics("9.1.0")
	metrics.EventDelivery("testrun-123/event/secret?url=https://private.invalid", true)
	metrics.EventDropped("testrun-123/event/secret?url=https://private.invalid", schemaInvalid)

	body := scrape(t, metrics)
	require.Contains(t, body, `event="unknown",outcome="write_success"`)
	require.Contains(t, body, `event="unknown",outcome="schema_invalid"`)
	require.NotContains(t, body, "testrun-123")
	require.NotContains(t, body, "private.invalid")
}

// scrape returns the Prometheus exposition for the supplied metrics registry.
func scrape(t *testing.T, metrics *SSEMetrics) string {
	t.Helper()
	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	require.Equal(t, 200, response.Code)
	return response.Body.String()
}

// TestSSEMetricsNilIsNoOp checks that a nil SSEMetrics never panics a stream handler.
func TestSSEMetricsNilIsNoOp(t *testing.T) {
	var m *SSEMetrics
	require.NotPanics(t, func() {
		m.Reconnect(true)
		m.Retry(true)
		m.Disconnect(writeError)
		m.EventDelivery("shutdown", true)
		m.EventDropped("message", schemaInvalid)
		m.TerminalResult(writeSuccess)
	})
	recorder := httptest.NewRecorder()
	m.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	require.Equal(t, 200, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "etos_sse_")
}
