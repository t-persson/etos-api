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
package sse

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// WithTracing enables optional connection tracing.
func (a *Application) WithTracing(tracer trace.Tracer) *Application {
	a.tracer = tracer
	return a
}

// startSpan links the connection span to the HTTP trace context and propagates baggage.
func (h Handler) startSpan(r *http.Request, identifier string) (*http.Request, trace.Span) {
	if h.tracer == nil {
		return r, noop.Span{}
	}
	start := time.Now()
	traceContext := propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(r.Header))
	link := trace.SpanContextFromContext(traceContext)
	ctx := propagation.Baggage{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	// The incoming trace is a link, not the parent of the SSE connection span.
	ctx = trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithTimestamp(start),
		trace.WithAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("http.route", "/sse/v2alpha/events/:identifier"),
			attribute.String("etos.id", identifier),
			attribute.Bool("etos.sse.reconnect", r.Header.Get("Last-Event-ID") != ""),
		),
	}
	if link.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: link}))
	}
	ctx, span := h.tracer.Start(ctx, "GET /sse/v2alpha/events/:identifier", opts...)
	return r.WithContext(ctx), span
}

// traceFailure marks an active connection span with a bounded error category,
// ignoring failures after cancellation and when tracing is disabled.
func (h Handler) traceFailure(ctx context.Context, category string) {
	if h.tracer == nil || ctx.Err() != nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("error.type", category))
	span.SetStatus(codes.Error, category)
}
