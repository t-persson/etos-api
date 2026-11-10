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
	"errors"
	"fmt"
	"os"

	goruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// Setup installs a global OpenTelemetry meter provider with Go runtime metrics exporting over OTLP/gRPC.
//
// Nothing is exported unless OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
// is set; without either endpoint the returned shutdown function is a no-op. The exporter and resource are
// further configured through the standard OTEL_* environment variables.
func Setup(ctx context.Context, serviceName, serviceVersion string) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") == "" {
		return noop, nil
	}
	exporter, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		return noop, fmt.Errorf("create OTLP metric exporter: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(serviceVersion),
			clusterAttribute(),
		),
		resource.WithFromEnv(),
		resource.WithProcess(),
	)
	if err != nil {
		return noop, errors.Join(fmt.Errorf("create OpenTelemetry resource: %w", err), exporter.Shutdown(ctx))
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	)
	if err := goruntime.Start(goruntime.WithMeterProvider(provider)); err != nil {
		return noop, errors.Join(fmt.Errorf("start Go runtime metrics: %w", err), provider.Shutdown(ctx))
	}
	otel.SetMeterProvider(provider)
	return provider.Shutdown, nil
}
