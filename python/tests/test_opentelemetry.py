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
"""Tests for ETOS API OpenTelemetry setup."""

import os
import subprocess
import sys
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from queue import Queue

import grpc
from fastapi import FastAPI
from fastapi.testclient import TestClient
from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor
from opentelemetry.proto.collector.metrics.v1 import (
    metrics_service_pb2,
    metrics_service_pb2_grpc,
)
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from etos_api.library.opentelemetry import OTEL_EXCLUDED_URLS


@dataclass
class RuntimeMetricsCollector(metrics_service_pb2_grpc.MetricsServiceServicer):
    """Receive real OTLP metric batches from isolated API startup."""

    requests: Queue[metrics_service_pb2.ExportMetricsServiceRequest] = field(default_factory=Queue)

    def Export(
        self,
        request: metrics_service_pb2.ExportMetricsServiceRequest,
        context: grpc.ServicerContext,
    ) -> metrics_service_pb2.ExportMetricsServiceResponse:
        """Capture a batch without forwarding telemetry.

        :param request: Exported metric batch.
        :param context: Incoming gRPC request context.
        :return: Successful empty OTLP response.
        """
        self.requests.put(request)
        # Protobuf creates this response class dynamically from its descriptor.
        return metrics_service_pb2.ExportMetricsServiceResponse()  # pylint: disable=no-member


def test_startup_exports_idle_counters_over_otlp() -> None:
    """Verify normal API startup exports attributed counters before its first request."""
    collector = RuntimeMetricsCollector()
    with ThreadPoolExecutor(max_workers=1) as executor:
        server = grpc.server(executor)
        metrics_service_pb2_grpc.add_MetricsServiceServicer_to_server(collector, server)
        port = server.add_insecure_port("127.0.0.1:0")
        endpoint = f"http://127.0.0.1:{port}"
        server.start()
        try:
            subprocess.run(
                [
                    sys.executable,
                    "-c",
                    "import etos_api; from opentelemetry import metrics; "
                    "metrics.get_meter_provider().shutdown(timeout_millis=5000)",
                ],
                env={
                    **os.environ,
                    "OTEL_EXPORTER_OTLP_ENDPOINT": endpoint,
                    "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": endpoint,
                    "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": endpoint,
                    "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": endpoint,
                    "OTEL_RESOURCE_ATTRIBUTES": "",
                    "OTEL_METRIC_EXPORT_INTERVAL": "3600000",
                    "ETOS_NAMESPACE": "metrics-test",
                    "ETOS_CLUSTER": "cluster-test",
                },
                check=True,
                capture_output=True,
                timeout=20,
            )
            request = collector.requests.get(timeout=5)
        finally:
            server.stop(0).wait(timeout=5)

    seen = set()
    for resource_metrics in request.resource_metrics:
        attributes = {
            entry.key: entry.value.string_value for entry in resource_metrics.resource.attributes
        }
        assert attributes["service.name"] == "etos-api"
        assert attributes["etos_cluster"] == "metrics-test/cluster-test"
        for scope in resource_metrics.scope_metrics:
            for metric in scope.metrics:
                if metric.name in ("etos_api.http.requests", "etos_api.suite.operations"):
                    seen.add(metric.name)
                    assert metric.sum.data_points
                    assert all(point.as_int == 0 for point in metric.sum.data_points)
                assert metric.name not in (
                    "etos_api.http.request.duration",
                    "etos_api.suite.operation.duration",
                )
    assert seen == {"etos_api.http.requests", "etos_api.suite.operations"}


def test_infrastructure_endpoints_are_not_traced():
    """Verify pings create no spans while API routes still do."""
    app = FastAPI()

    @app.get("/api/ping")
    async def ping():
        """Return a probe response."""
        return {}

    @app.get("/api/selftest")
    async def selftest():
        """Return a normal API response."""
        return {}

    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    FastAPIInstrumentor().instrument_app(
        app, tracer_provider=provider, excluded_urls=OTEL_EXCLUDED_URLS
    )
    try:
        client = TestClient(app)
        assert client.get("/api/ping").status_code == 200
        assert not exporter.get_finished_spans()

        assert client.get("/api/selftest").status_code == 200
        assert exporter.get_finished_spans()
    finally:
        FastAPIInstrumentor().uninstrument_app(app)
