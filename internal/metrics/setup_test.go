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
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	metricsv1 "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/grpc"
)

// runtimeMetricsCollector receives OTLP metrics exported by the SSE meter provider.
type runtimeMetricsCollector struct {
	metricsv1.UnimplementedMetricsServiceServer
	requests chan *metricsv1.ExportMetricsServiceRequest
}

// Export records the metric batch received during provider shutdown.
func (c *runtimeMetricsCollector) Export(
	_ context.Context,
	request *metricsv1.ExportMetricsServiceRequest,
) (*metricsv1.ExportMetricsServiceResponse, error) {
	c.requests <- request
	return &metricsv1.ExportMetricsServiceResponse{}, nil
}

// TestSetupFlushesRuntimeMetrics verifies immediate shutdown exports runtime metrics with SSE service identity.
func TestSetupFlushesRuntimeMetrics(t *testing.T) {
	previous := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	collector := &runtimeMetricsCollector{requests: make(chan *metricsv1.ExportMetricsServiceRequest, 1)}
	server := grpc.NewServer()
	metricsv1.RegisterMetricsServiceServer(server, collector)
	t.Cleanup(server.Stop)
	go func() {
		if err := server.Serve(listener); err != nil {
			t.Errorf("serve OTLP metrics: %v", err)
		}
	}()

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment.name=test")
	t.Setenv("ETOS_NAMESPACE", "metrics-test")
	t.Setenv("ETOS_CLUSTER", "cluster-test")
	expectedCluster := clusterAttribute().Value.AsString()
	shutdown, err := Setup(context.Background(), "etos-sse", "abc1234")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, shutdown(ctx))

	select {
	case request := <-collector.requests:
		require.NotEmpty(t, request.ResourceMetrics)
		var names []string
		for _, resourceMetrics := range request.ResourceMetrics {
			attributes := make(map[string]string)
			for _, attr := range resourceMetrics.Resource.Attributes {
				attributes[attr.Key] = attr.Value.GetStringValue()
			}
			require.Equal(t, "etos-sse", attributes["service.name"])
			require.Equal(t, "abc1234", attributes["service.version"])
			require.Equal(t, "test", attributes["deployment.environment.name"])
			require.Equal(t, expectedCluster, attributes["etos_cluster"])
			for _, scope := range resourceMetrics.ScopeMetrics {
				for _, metric := range scope.Metrics {
					names = append(names, metric.Name)
				}
			}
		}
		require.Contains(t, names, "go.goroutine.count")
		require.Contains(t, names, "go.memory.gc.goal")
	default:
		t.Fatal("shutdown did not export runtime metrics")
	}
}

// TestSetupWithoutEndpointLeavesMetricsDisabled verifies runtime collection respects existing opt-in behavior.
func TestSetupWithoutEndpointLeavesMetricsDisabled(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	previous := otel.GetMeterProvider()

	shutdown, err := Setup(context.Background(), "etos-sse", "abc1234")
	require.NoError(t, err)
	require.NoError(t, shutdown(context.Background()))
	require.Same(t, previous, otel.GetMeterProvider())
}
