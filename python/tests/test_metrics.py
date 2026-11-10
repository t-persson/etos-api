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
"""ETOS API request and operation metrics tests."""

import asyncio
import inspect
import logging
from unittest.mock import MagicMock, patch

import pytest
from fastapi import Body, FastAPI
from fastapi.routing import APIRoute
from fastapi.testclient import TestClient
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader

from etos_api.library import metrics as etos_metrics
from etos_api.library.metrics import (
    COUNT_REQUESTS,
    OPERATIONS,
    outcome_for_status,
    record_suite_operation,
)
from etos_api.main import APP

LABELS = {"endpoint": "/api/v1beta1/testrun", "operation": "start_testrun"}
SUBSUITE_LABELS = {
    "endpoint": "/api/v1beta1/testrun/{sub_suite_id}",
    "operation": "get_subsuite",
}


class MetricsReader:
    """Read back the data points recorded by the ETOS API instruments."""

    def __init__(self, reader: InMemoryMetricReader):
        """Set up the reader."""
        self.reader = reader

    def points(self, name: str) -> list:
        """Get all data points for a metric."""
        data = self.reader.get_metrics_data()
        if data is None:
            return []
        return [
            point
            for resource_metrics in data.resource_metrics
            for scope_metrics in resource_metrics.scope_metrics
            for metric in scope_metrics.metrics
            if metric.name == name
            for point in metric.data.data_points
        ]

    def count(self, name: str, **attributes) -> float:
        """Sum a counter, or count histogram observations, matching the attributes."""
        return sum(
            point.value if hasattr(point, "value") else point.count
            for point in self.points(name)
            if attributes.items() <= dict(point.attributes).items()
        )

    def histogram_sum(self, name: str, **attributes) -> float:
        """Sum the recorded values of a histogram matching the attributes."""
        return sum(
            point.sum
            for point in self.points(name)
            if attributes.items() <= dict(point.attributes).items()
        )


@pytest.fixture(name="otel")
def fixture_otel(monkeypatch):
    """Publish the ETOS API instruments to an in-memory OpenTelemetry reader."""
    reader = InMemoryMetricReader()
    provider = MeterProvider(metric_readers=[reader])
    monkeypatch.setattr(
        etos_metrics, "INSTRUMENTS", etos_metrics.create_instruments(provider.get_meter("test"))
    )
    monkeypatch.setattr(etos_metrics, "cluster_identity", lambda: "unassigned")
    yield MetricsReader(reader)
    provider.shutdown()


@pytest.fixture(name="client")
def fixture_client():
    """Test client for the ETOS API."""
    return TestClient(APP, raise_server_exceptions=False)


def test_idle_counters_export_zeros_without_duration_observations(otel: MetricsReader) -> None:
    """Verify startup exposes real zero counters before any requests or suite operations.

    :param otel: SDK-backed metric reader.
    """
    etos_metrics.initialize_counters()

    suite_points = otel.points("etos_api.suite.operations")
    assert len(suite_points) == 12
    for point in suite_points:
        assert point.value == 0
        assert point.attributes["etos_cluster"] == "unassigned"
        assert point.attributes["api_version"] in ("v1alpha", "v1beta1")
        assert point.attributes["operation"] in etos_metrics.SUITE_OPERATION_NAMES
        assert point.attributes["outcome"] in etos_metrics.SUITE_OPERATION_OUTCOMES

    http_points = otel.points("etos_api.http.requests")
    assert http_points
    for point in http_points:
        assert point.value == 0
        assert point.attributes["status"] == "500"
        assert point.attributes["etos_cluster"] == "unassigned"
        assert set(point.attributes) == {"endpoint", "operation", "status", "etos_cluster"}
        assert point.attributes["operation"] in OPERATIONS.__members__
    assert not otel.points("etos_api.suite.operation.duration")
    assert not otel.points("etos_api.http.request.duration")
    assert not otel.points("etos_api.requests")


def test_counter_initialization_preserves_first_failure(
    otel: MetricsReader, client: TestClient
) -> None:
    """Verify reinitializing counters cannot reset or invent failures.

    :param otel: SDK-backed metric reader.
    :param client: Local API test client.
    """
    etos_metrics.initialize_counters()
    assert get_subsuite(client, MagicMock(side_effect=RuntimeError("boom"))) == 500
    record_suite_operation("v1beta1", "suite_download", "server_error", 0.1)
    etos_metrics.initialize_counters()

    assert otel.count("etos_api.http.requests", **SUBSUITE_LABELS, status="500") == 1
    assert (
        otel.count(
            "etos_api.suite.operations",
            api_version="v1beta1",
            operation="suite_download",
            outcome="server_error",
        )
        == 1
    )
    assert otel.count("etos_api.http.request.duration", **SUBSUITE_LABELS) == 1
    assert otel.count("etos_api.suite.operation.duration") == 1


def get_subsuite(client: TestClient, get_mock: MagicMock) -> int:
    """Request a v1beta1 sub suite with a mocked Environment client and return the status."""
    with (
        patch("etos_api.routers.v1beta1.router.Kubernetes"),
        patch("etos_api.routers.v1beta1.router.Environment") as environment_mock,
    ):
        environment_mock.return_value.get = get_mock
        response = client.get("/api/v1beta1/testrun/my-environment")
    return response.status_code


def test_successful_request_is_counted_and_timed(otel, client):
    """Verify a successful route request is published as a request and a duration.

    Approval criteria:
        - etos_api.http.requests with status 200 shall increase by one.
        - etos_api.http.request.duration shall get one observation.
        - etos_api.requests shall record a versioned success.
    """
    resource = MagicMock()
    resource.to_dict.return_value = {"spec": {"name": "suite"}}

    assert get_subsuite(client, MagicMock(return_value=resource)) == 200

    assert otel.count("etos_api.http.requests", **SUBSUITE_LABELS, status="200") == 1
    assert otel.count("etos_api.http.request.duration", **SUBSUITE_LABELS) == 1
    assert (
        otel.count(
            "etos_api.requests",
            api_version="v1beta1",
            operation="get_subsuite",
            status_class="2xx",
            outcome="success",
            etos_cluster="unassigned",
        )
        == 1
    )
    assert otel.count("etos_api.request.duration", operation="get_subsuite") == 1


def test_http_exception_is_counted_with_its_status(otel, client):
    """Verify a route raising an HTTPException publishes its status and outcome."""
    assert get_subsuite(client, MagicMock(return_value=None)) == 404

    assert otel.count("etos_api.http.requests", **SUBSUITE_LABELS, status="404") == 1
    assert otel.count("etos_api.requests", operation="get_subsuite", outcome="not_found") == 1
    assert otel.count("etos_api.request.duration", outcome="not_found") == 1


def test_unhandled_exception_is_counted_as_500(otel, client):
    """Verify an unhandled exception is published as a 500 server error."""
    assert get_subsuite(client, MagicMock(side_effect=RuntimeError("boom"))) == 500

    assert otel.count("etos_api.http.requests", **SUBSUITE_LABELS, status="500") == 1
    assert otel.count("etos_api.requests", status_class="5xx", outcome="server_error") == 1


def test_v0_and_v1alpha_routes_are_instrumented(otel, client):
    """Verify v0 and v1alpha routes publish request metrics."""
    v0_labels = {"endpoint": "/api/v0/etos/{suite_id}", "operation": "stop_testrun"}
    v1alpha_labels = {
        "endpoint": "/api/v1alpha/testrun/{suite_id}",
        "operation": "get_subsuite",
    }
    with patch("etos_api.routers.v0.router._abort", side_effect=RuntimeError("boom")):
        assert client.delete("/api/v0/etos/my-suite").status_code == 500
    assert otel.count("etos_api.http.requests", **v0_labels, status="500") == 1
    with (
        patch("etos_api.routers.v1alpha.router.Kubernetes"),
        patch("etos_api.routers.v1alpha.router.Environment") as environment_mock,
    ):
        environment_mock.return_value.get.return_value = None
        assert client.get("/api/v1alpha/testrun/my-env").status_code == 404
    assert otel.count("etos_api.http.requests", **v1alpha_labels, status="404") == 1


def test_metrics_endpoint_is_gone(client):
    """Verify the API no longer serves a Prometheus scrape endpoint."""
    assert client.get("/metrics/").status_code == 404


def test_count_requests_measures_awaited_duration(otel):
    """Verify COUNT_REQUESTS records the time spent awaiting the coroutine."""
    labels = {"endpoint": "/test/time_requests", "operation": "get_subsuite"}

    @COUNT_REQUESTS(labels, logging.getLogger(__name__), "v1beta1")
    async def sleeper():
        await asyncio.sleep(0.05)
        return "done"

    assert asyncio.run(sleeper()) == "done"
    assert otel.histogram_sum("etos_api.http.request.duration", **labels) >= 0.05


def test_status_outcomes_are_bounded():
    """Verify HTTP statuses map to the bounded operation outcome vocabulary."""
    assert outcome_for_status(200) == "success"
    assert outcome_for_status(404) == "not_found"
    assert outcome_for_status(422) == "validation_rejected"
    assert outcome_for_status(504) == "timed_out"
    assert outcome_for_status(503) == "server_error"


def test_suite_operation_records_bounded_labels_and_duration(otel):
    """Verify suite observations publish only bounded attributes and the elapsed duration."""
    record_suite_operation("v1beta1", "suite_download", "client_error", 1.25)

    expected = {
        "etos_cluster": "unassigned",
        "api_version": "v1beta1",
        "operation": "suite_download",
        "outcome": "client_error",
    }
    (counter,) = otel.points("etos_api.suite.operations")
    (histogram,) = otel.points("etos_api.suite.operation.duration")
    assert dict(counter.attributes) == expected
    assert counter.value == 1
    assert dict(histogram.attributes) == expected
    assert histogram.sum == 1.25


def test_suite_operation_rejects_unbounded_label_values(otel):
    """Verify unsupported suite dimensions are rejected before metric emission."""
    for values in (
        ("v1", "suite_download", "success"),
        ("v1alpha", "download", "success"),
        ("v1alpha", "suite_validation", "failure"),
    ):
        with pytest.raises(ValueError):
            record_suite_operation(*values, duration=0.1)
    assert not otel.points("etos_api.suite.operations")


@pytest.mark.asyncio
async def test_count_requests_logs_unhandled_exception_and_reraises(otel):
    """Verify unexpected errors are logged, re-raised, and published as server errors."""
    logger = MagicMock()

    @COUNT_REQUESTS(LABELS, logger, "v1beta1")
    async def operation():
        raise RuntimeError("unexpected")

    with pytest.raises(RuntimeError, match="unexpected"):
        await operation()

    logger.exception.assert_called_once_with("Unhandled exception occurred, setting status to 500")
    assert otel.count("etos_api.requests", outcome="server_error", status_class="5xx") == 1


@pytest.mark.asyncio
async def test_count_requests_only_counts_requests_reaching_the_handler(otel):
    """Verify framework rejects and unmatched routes are not counted as operations."""
    app = FastAPI()

    @app.post("/operation")
    @COUNT_REQUESTS(LABELS, MagicMock(), "v1beta1")
    async def operation(value: int = Body(embed=True)):
        return {"value": value}

    client = TestClient(app)
    assert client.post("/operation", json={"value": 7}).status_code == 200
    assert client.post("/operation", json={"value": "not an integer"}).status_code == 422
    assert client.post("/unmatched", json={"value": 7}).status_code == 404

    assert otel.count("etos_api.requests") == 1
    assert otel.count("etos_api.http.requests", status="200") == 1


def test_mounted_api_route_handlers_are_coroutine_functions():
    """Verify metric decorators keep every mounted API handler awaitable by FastAPI.

    A synchronous wrapper makes FastAPI run the handler in a threadpool and return the
    un-awaited coroutine as the response.
    """
    endpoints = [
        route.endpoint
        for mount in APP.routes
        for route in getattr(getattr(mount, "app", None), "routes", [])
        if isinstance(route, APIRoute)
    ]

    assert endpoints
    for endpoint in endpoints:
        assert inspect.iscoroutinefunction(endpoint), endpoint.__qualname__
