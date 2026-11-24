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
package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/julienschmidt/httprouter"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// points collects the data points of one metric as attribute sets.
func points(t *testing.T, reader *sdkmetric.ManualReader, name string) []metricdata.DataPoint[int64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				return data.DataPoints
			}
		}
	}
	return nil
}

// histogramPoints collects the data points of one histogram metric.
func histogramPoints(t *testing.T, reader *sdkmetric.ManualReader, name string) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if data, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Name == name {
				return data.DataPoints
			}
		}
	}
	return nil
}

// value returns the value of the single point matching the attributes, or -1 when absent.
func value(pts []metricdata.DataPoint[int64], attrs ...attribute.KeyValue) int64 {
	want := attribute.NewSet(attrs...)
	for _, p := range pts {
		if p.Attributes.Equals(&want) {
			return p.Value
		}
	}
	return -1
}

func newReader() (*sdkmetric.ManualReader, *sdkmetric.MeterProvider) {
	reader := sdkmetric.NewManualReader()
	return reader, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
}

// TestHTTPMetricsUsesRouteTemplates verifies request identifiers stay out of metric attributes.
func TestHTTPMetricsUsesRouteTemplates(t *testing.T) {
	reader, provider := newReader()
	m := NewHTTPMetricsWithMeter("test", provider.Meter("test"))
	router := httprouter.New()
	router.GET("/items/:id", m.Wrap("/items/:id", http.MethodGet, func(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
		w.WriteHeader(http.StatusNoContent)
	}))

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/items/secret-id", nil))
	require.Equal(t, http.StatusNoContent, recorder.Code)

	pts := points(t, reader, "test.service.requests")
	require.Len(t, pts, 1)
	route, ok := pts[0].Attributes.Value("route")
	require.True(t, ok)
	require.Equal(t, "/items/:id", route.AsString())
	for _, kv := range pts[0].Attributes.ToSlice() {
		require.NotContains(t, kv.Value.String(), "secret-id")
	}
}

// TestSSEMetricsBoundsEventClassAndTracksConnections verifies bounded attributes and connection counts.
func TestSSEMetricsBoundsEventClassAndTracksConnections(t *testing.T) {
	t.Setenv("ETOS_NAMESPACE", "")
	t.Setenv("ETOS_CLUSTER", "")
	reader, provider := newReader()
	m := NewSSEMetricsWithMeter(provider.Meter("test"))
	m.ConnectionOpened()
	m.EventDelivered("unexpected-user-event")
	m.EventDelivered("ping")
	m.ReconnectRequested()
	m.StreamError("sensitive-backend-error")
	m.ConnectionClosedWithReason("client_disconnect")

	cluster := attribute.String("etos_cluster", "unassigned")
	require.EqualValues(t, 0, value(points(t, reader, "etos_sse.connections.active"), cluster))
	require.EqualValues(t, 1, value(points(t, reader, "etos_sse.connections"), cluster))
	events := points(t, reader, "etos_sse.events.delivered")
	require.EqualValues(t, 1, value(events, cluster, attribute.String("event_class", "data")))
	require.EqualValues(t, 1, value(events, cluster, attribute.String("event_class", "ping")))
	require.EqualValues(t, 1, value(points(t, reader, "etos_sse.disconnects"), cluster, attribute.String("reason", "client_disconnect")))
	require.EqualValues(t, 1, value(points(t, reader, "etos_sse.errors"), cluster, attribute.String("category", "unknown")))
	require.EqualValues(t, 1, value(points(t, reader, "etos_sse.reconnects"), cluster))
}

// TestSSEConnectionsCloseOnceWithoutAffectingOtherClients verifies independent client accounting.
func TestSSEConnectionsCloseOnceWithoutAffectingOtherClients(t *testing.T) {
	t.Setenv("ETOS_CLUSTER", "")
	reader, provider := newReader()
	m := NewSSEMetricsWithMeter(provider.Meter("test"))
	cluster := attribute.String("etos_cluster", "unassigned")
	m.ConnectionOpened()
	m.ConnectionOpened()
	m.ConnectionClosedWithReason("client_disconnect")
	require.EqualValues(t, 1, value(points(t, reader, "etos_sse.connections.active"), cluster))
	require.EqualValues(t, 1, value(points(t, reader, "etos_sse.disconnects"), cluster, attribute.String("reason", "client_disconnect")))
	m.ConnectionClosedWithReason("server_shutdown")
	require.EqualValues(t, 0, value(points(t, reader, "etos_sse.connections.active"), cluster))
}

// TestHTTPHistogramAggregatesStatusClassesWithoutDuplicateSeries verifies shared latency attributes aggregate.
func TestHTTPHistogramAggregatesStatusClassesWithoutDuplicateSeries(t *testing.T) {
	t.Setenv("ETOS_CLUSTER", "")
	reader, provider := newReader()
	m := NewHTTPMetricsWithMeter("test", provider.Meter("test"))
	m.recordRequest(context.Background(), "/items/:id", "GET", "4xx", "client_error", 0.01)
	m.recordRequest(context.Background(), "/items/:id", "GET", "5xx", "client_error", 0.02)
	pts := histogramPoints(t, reader, "test.service.request.duration")
	require.Len(t, pts, 1)
	require.EqualValues(t, 2, pts[0].Count)
}

// TestSSECollectorsUseExplicitCluster verifies all SSE instruments use the configured namespace-qualified cluster identity.
func TestSSECollectorsUseExplicitCluster(t *testing.T) {
	for _, cluster := range []string{"first", "second"} {
		t.Run(cluster, func(t *testing.T) {
			t.Setenv("ETOS_NAMESPACE", "namespace")
			t.Setenv("ETOS_CLUSTER", cluster)
			reader, provider := newReader()
			m := NewSSEMetricsWithMeter(provider.Meter("test"))
			m.ConnectionOpened()
			m.EventDelivered("ping")
			m.ReconnectRequested()
			m.StreamError("backend_failure")
			m.ConnectionClosedWithReason("client_disconnect")
			m.recordRequest(context.Background(), "/events", "GET", "2xx", "success", 0.01)

			var rm metricdata.ResourceMetrics
			require.NoError(t, reader.Collect(context.Background(), &rm))
			seen := 0
			check := func(set attribute.Set, name string) {
				v, ok := set.Value("etos_cluster")
				require.True(t, ok, name)
				require.Equal(t, "namespace/"+cluster, v.AsString(), name)
				seen++
			}
			for _, scope := range rm.ScopeMetrics {
				for _, mt := range scope.Metrics {
					switch data := mt.Data.(type) {
					case metricdata.Sum[int64]:
						for _, p := range data.DataPoints {
							check(p.Attributes, mt.Name)
						}
					case metricdata.Histogram[float64]:
						for _, p := range data.DataPoints {
							check(p.Attributes, mt.Name)
						}
					}
				}
			}
			require.Equal(t, 8, seen)
		})
	}
}

// TestResponseRecorderPreservesFirstAndImplicitStatus verifies implicit success cannot be overwritten.
func TestResponseRecorderPreservesFirstAndImplicitStatus(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &responseRecorder{ResponseWriter: recorder, status: http.StatusOK}
	_, err := writer.Write([]byte("body"))
	require.NoError(t, err)
	writer.WriteHeader(http.StatusInternalServerError)
	require.Equal(t, http.StatusOK, writer.status)
	require.Equal(t, http.StatusOK, recorder.Code)
}
