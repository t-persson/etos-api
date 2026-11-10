# Copyright Axis Communications AB.
#
# For a full list of individual contributors, please see the commit history.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""ETOS opentelemetry helpers."""

import logging
from typing import Annotated

from fastapi import FastAPI, Header
from opentelemetry import context as otel_context
from opentelemetry import metrics, trace
from opentelemetry.baggage.propagation import W3CBaggagePropagator
from opentelemetry.exporter.otlp.proto.grpc.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor
from opentelemetry.instrumentation.system_metrics import SystemMetricsInstrumentor
from opentelemetry.propagate import extract, set_global_textmap
from opentelemetry.propagators.composite import CompositePropagator
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import (
    SERVICE_NAME,
    SERVICE_VERSION,
    OTELResourceDetector,
    ProcessResourceDetector,
    Resource,
    get_aggregated_resources,
)
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator
from pydantic import BaseModel

from etos_api.library.cluster import cluster_identity
from etos_api.library.metrics import initialize_counters

LOGGER = logging.getLogger(__name__)
# Health probes are periodic infrastructure requests, not user operations.
OTEL_EXCLUDED_URLS = ".*/ping"


def setup_opentelemetry(app: FastAPI, version: str) -> Resource:
    """Set up OpenTelemetry for ETOS API."""
    otel_resource = Resource.create(
        {
            SERVICE_NAME: "etos-api",
            SERVICE_VERSION: version,
            "etos_cluster": cluster_identity(),
        },
    )

    otel_resource = get_aggregated_resources(
        [OTELResourceDetector(), ProcessResourceDetector()],
    ).merge(otel_resource)

    provider = TracerProvider(resource=otel_resource)
    exporter = OTLPSpanExporter()
    processor = BatchSpanProcessor(exporter)
    provider.add_span_processor(processor)
    trace.set_tracer_provider(provider)
    setup_runtime_metrics(otel_resource)
    propagator = CompositePropagator(
        [
            TraceContextTextMapPropagator(),
            W3CBaggagePropagator(),
        ]
    )
    set_global_textmap(propagator)
    FastAPIInstrumentor().instrument_app(
        app, tracer_provider=provider, excluded_urls=OTEL_EXCLUDED_URLS
    )
    return otel_resource


def setup_runtime_metrics(resource: Resource) -> MeterProvider:
    """Export the default system and Python runtime metrics over OTLP."""
    reader = PeriodicExportingMetricReader(OTLPMetricExporter())
    meter_provider = MeterProvider(resource=resource, metric_readers=[reader])
    metrics.set_meter_provider(meter_provider)
    initialize_counters()
    SystemMetricsInstrumentor().instrument(meter_provider=meter_provider)
    return meter_provider


class OTELHeaders(BaseModel):
    """Headers model."""

    baggage: str | None = None
    traceparent: str | None = None


def context(headers: Annotated[OTELHeaders, Header()]) -> otel_context.Context:
    """Extract OpenTelemetry context from headers."""
    ctx = extract(headers.model_dump(), context=otel_context.get_current())
    LOGGER.debug("Extracted context from headers: %r", ctx)
    return ctx
