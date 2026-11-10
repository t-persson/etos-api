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
"""ETOS API metrics."""

from dataclasses import dataclass
from enum import Enum
from functools import wraps
from logging import Logger
from time import perf_counter
from typing import Callable

from fastapi import HTTPException
from opentelemetry import metrics
from opentelemetry.metrics import Counter, Histogram, Meter

from etos_api.library.cluster import cluster_identity

OPERATIONS = Enum(
    "OPERATIONS",
    [
        "start_testrun",
        "get_subsuite",
        "stop_testrun",
    ],
)

# Seconds. The OpenTelemetry default boundaries are meant for milliseconds.
DURATION_BUCKETS = [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10]


@dataclass(frozen=True)
class Instruments:
    """OpenTelemetry instruments published by the ETOS API."""

    request_duration: Histogram
    requests: Counter
    api_requests: Counter
    api_request_duration: Histogram
    suite_operations: Counter
    suite_operation_duration: Histogram


def create_instruments(meter: Meter) -> Instruments:
    """Create the ETOS API instruments on a meter."""
    return Instruments(
        request_duration=meter.create_histogram(
            "etos_api.http.request.duration",
            unit="s",
            description="Time spent processing request",
            explicit_bucket_boundaries_advisory=DURATION_BUCKETS,
        ),
        requests=meter.create_counter(
            "etos_api.http.requests",
            unit="{request}",
            description="Total number of requests",
        ),
        api_requests=meter.create_counter(
            "etos_api.requests",
            unit="{request}",
            description="Total decorated ETOS API operations by version, operation, status "
            "class, and outcome; excludes HTTP requests that do not reach a decorated handler",
        ),
        api_request_duration=meter.create_histogram(
            "etos_api.request.duration",
            unit="s",
            description="Duration of decorated ETOS API operations; excludes HTTP requests that "
            "do not reach a decorated handler",
            explicit_bucket_boundaries_advisory=DURATION_BUCKETS,
        ),
        suite_operations=meter.create_counter(
            "etos_api.suite.operations",
            unit="{operation}",
            description="Total suite download and validation operations",
        ),
        suite_operation_duration=meter.create_histogram(
            "etos_api.suite.operation.duration",
            unit="s",
            description="Suite download and validation duration",
            explicit_bucket_boundaries_advisory=DURATION_BUCKETS,
        ),
    )


# Created against the global meter provider. Until OpenTelemetry is set up the instruments are
# no-ops, and they start exporting as soon as the provider is installed.
INSTRUMENTS = create_instruments(metrics.get_meter("etos_api"))

SUITE_OPERATION_NAMES = ("suite_download", "suite_validation")
SUITE_OPERATION_OUTCOMES = ("success", "client_error", "server_error")
SUITE_API_VERSIONS = ("v1alpha", "v1beta1")
_HTTP_REQUEST_LABELS: list[dict[str, str]] = []


def initialize_counters() -> None:
    """Expose idle counters after meter-provider setup, without recording durations."""
    cluster = cluster_identity()
    for api_version in SUITE_API_VERSIONS:
        for operation in SUITE_OPERATION_NAMES:
            for outcome in SUITE_OPERATION_OUTCOMES:
                INSTRUMENTS.suite_operations.add(
                    0,
                    {
                        "etos_cluster": cluster,
                        "api_version": api_version,
                        "operation": operation,
                        "outcome": outcome,
                    },
                )
    for labels in _HTTP_REQUEST_LABELS:
        INSTRUMENTS.requests.add(0, {**labels, "status": "500", "etos_cluster": cluster})


def outcome_for_status(status_code: int) -> str:
    """Map an HTTP status to the bounded dashboard outcome vocabulary."""
    if 200 <= status_code < 300:
        return "success"
    if status_code == 404:
        return "not_found"
    if status_code == 422:
        return "validation_rejected"
    if status_code == 504:
        return "timed_out"
    if 400 <= status_code < 500:
        return "client_error"
    return "server_error"


def record_suite_operation(api_version: str, operation: str, outcome: str, duration: float) -> None:
    """Record one bounded suite admission operation observation."""
    if api_version not in SUITE_API_VERSIONS:
        raise ValueError(f"Unsupported API version: {api_version}")
    if operation not in SUITE_OPERATION_NAMES:
        raise ValueError(f"Unsupported suite operation: {operation}")
    if outcome not in SUITE_OPERATION_OUTCOMES:
        raise ValueError(f"Unsupported suite operation outcome: {outcome}")

    labels = {
        "etos_cluster": cluster_identity(),
        "api_version": api_version,
        "operation": operation,
        "outcome": outcome,
    }
    INSTRUMENTS.suite_operations.add(1, labels)
    INSTRUMENTS.suite_operation_duration.record(duration, labels)


# I like the idea of all operations in this file is upper-case.
def COUNT_REQUESTS(labels: dict, logger: Logger, api_version: str):  # pylint:disable=invalid-name
    """Count requests that reach a decorated operation handler.

    These operation metrics do not include FastAPI request-validation rejects or unmatched
    routes because those requests never reach the decorated handler. FastAPI auto-instrumentation
    handles generic HTTP instrumentation separately; this decorator emits operation metrics only.

    The decorator also records the request duration. A synchronous timing decorator must not be
    used on the async route handlers because it returns the un-awaited coroutine to FastAPI.
    """
    _HTTP_REQUEST_LABELS.append(dict(labels))

    def decorator(func: Callable):
        @wraps(func)
        async def wrapper(*args, **kwargs):
            started = perf_counter()
            status_code = 200

            def record(status_code: int):
                duration = perf_counter() - started
                cluster = cluster_identity()
                outcome = outcome_for_status(status_code)
                instruments = INSTRUMENTS
                instruments.request_duration.record(duration, {**labels, "etos_cluster": cluster})
                instruments.requests.add(
                    1, {**labels, "status": str(status_code), "etos_cluster": cluster}
                )
                instruments.api_requests.add(
                    1,
                    {
                        "etos_cluster": cluster,
                        "api_version": api_version,
                        "operation": labels["operation"],
                        "status_class": f"{status_code // 100}xx",
                        "outcome": outcome,
                    },
                )
                instruments.api_request_duration.record(
                    duration,
                    {
                        "etos_cluster": cluster,
                        "api_version": api_version,
                        "operation": labels["operation"],
                        "outcome": outcome,
                    },
                )

            try:
                return await func(*args, **kwargs)
            except HTTPException as http_exception:
                status_code = http_exception.status_code
                raise
            except Exception:  # pylint:disable=bare-except
                status_code = 500
                logger.exception("Unhandled exception occurred, setting status to 500")
                raise
            finally:
                record(status_code)

        return wrapper

    return decorator
