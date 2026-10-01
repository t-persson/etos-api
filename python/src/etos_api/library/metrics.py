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

import os
import time
from enum import Enum
from functools import wraps
from importlib.metadata import PackageNotFoundError, version
from logging import Logger
from typing import Callable, Optional

from fastapi import FastAPI, HTTPException, Request
from fastapi.exception_handlers import request_validation_exception_handler
from fastapi.exceptions import RequestValidationError
from prometheus_client import Counter, Gauge, Histogram

OPERATIONS = Enum(
    "OPERATIONS",
    [
        "start_testrun",
        "get_subsuite",
        "stop_testrun",
    ],
)

# ETOS release version, set from the ETOS_VERSION environment variable by init_metrics.
ETOS_VERSION = "unknown"

REQUEST_TIME = Histogram(
    "http_request_duration_seconds",
    "Time spent processing request",
    ["endpoint", "operation", "etos_version"],
)
REQUESTS_TOTAL = Counter(
    "http_requests_total",
    "Total number of requests",
    ["endpoint", "operation", "status", "etos_version"],
)

BUILD_INFO = Gauge(
    "etos_api_build_info",
    "ETOS API build information. Always 1; the information is carried in the labels.",
    ["etos_version", "version"],
)

_COMMON_SUBMISSION_OUTCOMES = (
    "success",
    "request_invalid",
    "suite_download_failed",
    "suite_invalid",
    "artifact_not_found",
    "artifact_lookup_timeout",
    "event_repository_error",
)
# Outcomes of a TestRun submission per API version. Each failure outcome names the failed stage.
SUBMISSION_OUTCOMES = {
    "v0": _COMMON_SUBMISSION_OUTCOMES
    + ("environment_configuration_failed", "event_publish_failed", "internal_error"),
    "v1alpha": _COMMON_SUBMISSION_OUTCOMES + ("testrun_create_failed", "internal_error"),
    "v1beta1": _COMMON_SUBMISSION_OUTCOMES + ("testrun_create_failed", "internal_error"),
}
# Submissions rejected before the handler runs; no duration is observed for them.
UNTIMED_SUBMISSION_OUTCOMES = ("request_invalid",)
TESTRUN_SUBMISSIONS_TOTAL = Counter(
    "etos_api_testrun_submissions_total",
    "Total number of TestRun submissions handled by the ETOS API, by outcome.",
    ["api_version", "outcome", "etos_version"],
)
TESTRUN_SUBMISSION_DURATION = Histogram(
    "etos_api_testrun_submission_duration_seconds",
    "Time spent handling a TestRun submission, by outcome.",
    ["api_version", "outcome", "etos_version"],
    buckets=(0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 45, 60, 90, 120),
)

# Queries the ETOS API sends to the event repository while waiting for an artifact.
ARTIFACT_QUERIES = ("artifact_by_id", "artifact_by_identity")
# Outcome of a single event repository query attempt.
QUERY_ATTEMPT_OUTCOMES = ("found", "not_ready", "error")
# Final outcome of waiting for an artifact.
ARTIFACT_LOOKUP_OUTCOMES = ("found", "not_found", "error")
EVENT_REPOSITORY_QUERIES_TOTAL = Counter(
    "etos_api_event_repository_queries_total",
    "Total number of event repository query attempts made by the ETOS API, by outcome.",
    ["api_version", "query", "outcome", "etos_version"],
)
ARTIFACT_LOOKUP_DURATION = Histogram(
    "etos_api_artifact_lookup_duration_seconds",
    "Time spent polling the event repository for an artifact, by final outcome.",
    ["api_version", "query", "outcome", "etos_version"],
    buckets=(0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15, 20, 30, 45, 60),
)


def package_version() -> str:
    """Return the installed etos_api package version."""
    try:
        return version("etos_api")
    except PackageNotFoundError:
        return "unknown"


def init_metrics():
    """Set the ETOS version and build information and pre-initialize known label combinations.

    Pre-initializing the series makes a zero value distinguishable from a missing series.
    """
    global ETOS_VERSION  # pylint:disable=global-statement
    ETOS_VERSION = os.getenv("ETOS_VERSION") or "unknown"
    BUILD_INFO.clear()
    BUILD_INFO.labels(etos_version=ETOS_VERSION, version=package_version()).set(1)
    for api_version, outcomes in SUBMISSION_OUTCOMES.items():
        for outcome in outcomes:
            TESTRUN_SUBMISSIONS_TOTAL.labels(api_version, outcome, ETOS_VERSION)
            if outcome not in UNTIMED_SUBMISSION_OUTCOMES:
                TESTRUN_SUBMISSION_DURATION.labels(api_version, outcome, ETOS_VERSION)
        for query in ARTIFACT_QUERIES:
            for outcome in QUERY_ATTEMPT_OUTCOMES:
                EVENT_REPOSITORY_QUERIES_TOTAL.labels(api_version, query, outcome, ETOS_VERSION)
            for outcome in ARTIFACT_LOOKUP_OUTCOMES:
                ARTIFACT_LOOKUP_DURATION.labels(api_version, query, outcome, ETOS_VERSION)


init_metrics()


class SubmissionFailed(HTTPException):
    """HTTP exception for a failed TestRun submission, carrying a bounded metrics outcome."""

    def __init__(self, outcome: str, status_code: int, detail: str, headers=None):
        """Initialize with the metrics outcome and the HTTP response values."""
        super().__init__(status_code=status_code, detail=detail, headers=headers)
        self.outcome = outcome


class TestRunSubmission:
    """Context manager recording exactly one outcome and duration for a TestRun submission.

    A :obj:`SubmissionFailed` records its own outcome. Any other exception records the
    failure outcome of the current stage, set with :meth:`stage`. Cancellation is not recorded.
    """

    __test__ = False

    def __init__(self, api_version: str):
        """Initialize the submission for an API version."""
        self.api_version = api_version
        self.failure = "internal_error"
        self.start = 0.0

    def stage(self, failure: str):
        """Set the outcome to record if an unexpected exception escapes the current stage."""
        self.failure = failure

    def __enter__(self):
        """Start timing the submission."""
        self.start = time.monotonic()
        return self

    def __exit__(self, exc_type, exc, traceback):
        """Record the submission outcome and duration without suppressing exceptions."""
        if exc is None:
            outcome = "success"
        elif isinstance(exc, SubmissionFailed):
            outcome = exc.outcome
        elif isinstance(exc, Exception):
            outcome = self.failure
        else:
            return False
        if outcome not in SUBMISSION_OUTCOMES[self.api_version]:
            outcome = "internal_error"
        TESTRUN_SUBMISSIONS_TOTAL.labels(self.api_version, outcome, ETOS_VERSION).inc()
        TESTRUN_SUBMISSION_DURATION.labels(self.api_version, outcome, ETOS_VERSION).observe(
            time.monotonic() - self.start
        )
        return False


def count_invalid_submissions(app: FastAPI, path: str):
    """Record schema-invalid POST requests to a submission route as request_invalid.

    The response is FastAPI's default 422 validation response. The request is rejected before
    the handler runs, so no duration is observed.
    """

    async def handler(request: Request, exc: RequestValidationError):
        """Record the invalid submission and delegate to FastAPI's default handler."""
        route = request.scope.get("route")
        if request.method == "POST" and getattr(route, "path", None) == path:
            TESTRUN_SUBMISSIONS_TOTAL.labels(app.version, "request_invalid", ETOS_VERSION).inc()
        return await request_validation_exception_handler(request, exc)

    app.add_exception_handler(RequestValidationError, handler)


class ArtifactLookup:
    """Record event repository query attempts and the final outcome of an artifact lookup."""

    def __init__(self, api_version: str, artifact_id: Optional[object]):
        """Initialize the lookup for an API version and query type."""
        self.api_version = api_version
        self.query = "artifact_by_id" if artifact_id is not None else "artifact_by_identity"
        self.start = time.monotonic()

    def attempt(self, outcome: str, final: bool = False):
        """Record a query attempt and, when final, the lookup outcome."""
        EVENT_REPOSITORY_QUERIES_TOTAL.labels(
            self.api_version, self.query, outcome, ETOS_VERSION
        ).inc()
        if final:
            self.finish(outcome)

    def finish(self, outcome: str):
        """Record the final lookup outcome and duration."""
        ARTIFACT_LOOKUP_DURATION.labels(
            self.api_version, self.query, outcome, ETOS_VERSION
        ).observe(time.monotonic() - self.start)


# I like the idea of all operations in this file is upper-case.
def COUNT_REQUESTS(labels: dict, logger: Logger):  # pylint:disable=invalid-name
    """Count and time requests using the REQUESTS_TOTAL and REQUEST_TIME metrics.

    Apply it below the FastAPI route decorator so that the route registers the wrapped handler.
    """

    def decorator(func: Callable):
        def record(status: int, start: float):
            """Record the request status and duration."""
            REQUESTS_TOTAL.labels(**labels, status=status, etos_version=ETOS_VERSION).inc()
            REQUEST_TIME.labels(**labels, etos_version=ETOS_VERSION).observe(
                time.monotonic() - start
            )

        @wraps(func)
        async def wrapper(*args, **kwargs):
            start = time.monotonic()
            try:
                response = await func(*args, **kwargs)
            except HTTPException as http_exception:
                record(http_exception.status_code, start)
                raise
            except Exception:  # pylint:disable=bare-except
                logger.exception("Unhandled exception occurred, setting status to 500")
                record(500, start)
                raise
            record(200, start)
            return response

        return wrapper

    return decorator
