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

// Package tracing provides optional tracing for the SSE service.
package tracing

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// Setup returns a tracer and bounded-by-context shutdown function without changing
// the global provider. A nil tracer disables tracing.
func Setup(ctx context.Context, serviceName, serviceVersion string) (trace.Tracer, func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if os.Getenv("OTEL_SDK_DISABLED") == "true" || os.Getenv("OTEL_TRACES_EXPORTER") == "none" ||
		(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "") {
		return nil, noop, nil
	}
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, noop, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName), semconv.ServiceVersion(serviceVersion)),
		resource.WithFromEnv(),
		resource.WithProcess(),
	)
	if err != nil {
		return nil, noop, errors.Join(fmt.Errorf("create OpenTelemetry resource: %w", err), exporter.Shutdown(ctx))
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	return provider.Tracer("github.com/eiffel-community/etos-api/sse"), provider.Shutdown, nil
}
