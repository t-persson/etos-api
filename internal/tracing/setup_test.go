// Copyright Axis Communications AB.
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
package tracing

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracev1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

const testParent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"

type traceCollector struct {
	tracev1.UnimplementedTraceServiceServer
	requests chan *tracev1.ExportTraceServiceRequest
}

// Export captures an OTLP trace batch for assertions after provider shutdown.
func (c *traceCollector) Export(_ context.Context, request *tracev1.ExportTraceServiceRequest) (*tracev1.ExportTraceServiceResponse, error) {
	c.requests <- request
	return &tracev1.ExportTraceServiceResponse{}, nil
}

// TestSetupExportsSpanLinks verifies that shutdown exports linked spans and service
// metadata through either OTLP endpoint setting without replacing the global provider.
func TestSetupExportsSpanLinks(t *testing.T) {
	for _, endpoint := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		t.Run(endpoint, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			collector := &traceCollector{requests: make(chan *tracev1.ExportTraceServiceRequest, 1)}
			server := grpc.NewServer()
			tracev1.RegisterTraceServiceServer(server, collector)
			t.Cleanup(server.Stop)
			go func() {
				if err := server.Serve(listener); err != nil {
					t.Errorf("serve OTLP traces: %v", err)
				}
			}()
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
			t.Setenv(endpoint, "http://"+listener.Addr().String())
			t.Setenv("OTEL_SDK_DISABLED", "false")
			t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
			t.Setenv("OTEL_SERVICE_NAME", "etos-sse")
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment.name=test")
			t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
			previous := otel.GetTracerProvider()
			tracer, shutdown, err := Setup(context.Background(), "etos-sse", "revision")
			require.NoError(t, err)
			require.NotNil(t, tracer)
			assert.Same(t, previous, otel.GetTracerProvider())
			linkCtx := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": testParent})
			link := trace.SpanContextFromContext(linkCtx)
			_, span := tracer.Start(context.Background(), "connection", trace.WithLinks(trace.Link{SpanContext: link}))
			span.End()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, shutdown(ctx))
			select {
			case request := <-collector.requests:
				require.Len(t, request.ResourceSpans, 1)
				resource := request.ResourceSpans[0]
				attrs := map[string]string{}
				for _, attr := range resource.Resource.Attributes {
					attrs[attr.Key] = attr.Value.GetStringValue()
				}
				assert.Equal(t, "etos-sse", attrs["service.name"])
				assert.Equal(t, "revision", attrs["service.version"])
				assert.Equal(t, "test", attrs["deployment.environment.name"])
				require.Len(t, resource.ScopeSpans, 1)
				require.Len(t, resource.ScopeSpans[0].Spans, 1)
				exported := resource.ScopeSpans[0].Spans[0]
				require.Len(t, exported.Links, 1)
				traceID, spanID := link.TraceID(), link.SpanID()
				assert.Equal(t, traceID[:], exported.Links[0].TraceId)
				assert.Equal(t, spanID[:], exported.Links[0].SpanId)
				assert.NotEqual(t, exported.TraceId, exported.Links[0].TraceId)
				assert.Empty(t, exported.ParentSpanId)
			default:
				t.Fatal("shutdown did not export the span")
			}
		})
	}
}

// TestSetupDisabled verifies that absent endpoints and explicit disable settings
// return a nil tracer and a safe shutdown function.
func TestSetupDisabled(t *testing.T) {
	for _, reason := range []string{"no endpoint", "SDK disabled", "exporter disabled"} {
		t.Run(reason, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
			t.Setenv("OTEL_SDK_DISABLED", "false")
			t.Setenv("OTEL_TRACES_EXPORTER", "")
			if reason != "no endpoint" {
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
			}
			if reason == "SDK disabled" {
				t.Setenv("OTEL_SDK_DISABLED", "true")
			}
			if reason == "exporter disabled" {
				t.Setenv("OTEL_TRACES_EXPORTER", "none")
			}
			tracer, shutdown, err := Setup(context.Background(), "etos-sse", "revision")
			require.NoError(t, err)
			assert.Nil(t, tracer)
			require.NoError(t, shutdown(context.Background()))
		})
	}
}
