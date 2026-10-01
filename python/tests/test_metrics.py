# Copyright 2020 Axis Communications AB.
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
"""Tests for the ETOS API dashboard metrics."""

import asyncio
import os
from contextlib import ExitStack
from unittest import TestCase
from unittest.mock import AsyncMock, MagicMock, patch

from fastapi import HTTPException
from fastapi.testclient import TestClient
from prometheus_client import REGISTRY

from etos_api.library import metrics
from etos_api.main import APP
from etos_api.routers.v1beta1.testrun import Artifact

V1BETA1_TESTRUN = "etos_api.routers.v1beta1.testrun.TestRun"
V1BETA1_SUITE = {
    "name": "TestSuite",
    "schemaVersion": "v1beta1",
    "suites": [
        {
            "priority": 1,
            "testExecutions": [
                {
                    "id": "00000000-0000-0000-0000-000000000000",
                    "testCase": {"id": "test", "version": "main"},
                    "execution": {"checkout": ["true"], "command": "true"},
                    "environment": {
                        "environmentVariables": {},
                        "testRunner": "example.com/test-runner:1.0.0",
                    },
                }
            ],
        }
    ],
}
ARTIFACT = Artifact(artifact_id="11111111-1111-1111-1111-111111111111", identity="pkg:x/y")
EDGES = [
    {
        "node": {
            "meta": {"id": "11111111-1111-1111-1111-111111111111"},
            "data": {"identity": "pkg:x/y"},
        }
    }
]
FOUND = {"artifactCreated": {"edges": EDGES}}
START_PATHS = {
    "v0": "/api/v0/etos",
    "v1alpha": "/api/v1alpha/testrun",
    "v1beta1": "/api/v1beta1/testrun",
}
START_BODY = {"artifact_identity": "pkg:x/y", "test_suite_url": "http://localhost/suite.json"}


def _sample(name: str, **labels) -> float:
    """Return the current value of a metric sample, treating a missing sample as zero."""
    return REGISTRY.get_sample_value(name, {**labels, "etos_version": metrics.ETOS_VERSION}) or 0.0


def _submissions(api_version: str, outcome: str) -> float:
    """Return the submission counter value for an API version and outcome."""
    return _sample("etos_api_testrun_submissions_total", api_version=api_version, outcome=outcome)


def _submission_durations(api_version: str, outcome: str) -> float:
    """Return the submission duration observation count for an API version and outcome."""
    return _sample(
        "etos_api_testrun_submission_duration_seconds_count",
        api_version=api_version,
        outcome=outcome,
    )


def _queries(api_version: str, query: str, outcome: str) -> float:
    """Return the event repository query counter value."""
    return _sample(
        "etos_api_event_repository_queries_total",
        api_version=api_version,
        query=query,
        outcome=outcome,
    )


def _lookups(api_version: str, query: str, outcome: str) -> float:
    """Return the artifact lookup observation count."""
    return _sample(
        "etos_api_artifact_lookup_duration_seconds_count",
        api_version=api_version,
        query=query,
        outcome=outcome,
    )


class SubmissionMetricsMixin:  # pylint: disable=too-few-public-methods
    """Shared assertions for TestRun submission metrics."""

    api_version = ""

    def _post(self, body=None) -> int:
        """Post a start request for the API version and return the status code."""
        client = TestClient(APP, raise_server_exceptions=False)
        response = client.post(
            START_PATHS[self.api_version], json=START_BODY if body is None else body
        )
        return response.status_code

    def _assert_outcome(self, outcome: str, status: int, body=None):
        """Assert that a submission returns a status and records exactly one outcome."""
        outcomes = metrics.SUBMISSION_OUTCOMES[self.api_version]
        before = {name: _submissions(self.api_version, name) for name in outcomes}
        durations = _submission_durations(self.api_version, outcome)
        self.assertEqual(self._post(body), status)  # pylint:disable=no-member
        for name in outcomes:
            expected = before[name] + (1 if name == outcome else 0)
            self.assertEqual(  # pylint:disable=no-member
                _submissions(self.api_version, name), expected, name
            )
        timed = 0 if outcome in metrics.UNTIMED_SUBMISSION_OUTCOMES else 1
        self.assertEqual(  # pylint:disable=no-member
            _submission_durations(self.api_version, outcome), durations + timed
        )

    def test_request_invalid(self):
        """Test that a schema-invalid submission is recorded as request_invalid.

        Approval criteria:
            - A schema-invalid submission shall return FastAPI's default 422 response.
            - It shall increment only the request_invalid outcome and observe no duration.
        """
        self._assert_outcome("request_invalid", 422, body={})


class TestV1Beta1SubmissionMetrics(SubmissionMetricsMixin, TestCase):
    """Test v1beta1 TestRun submission outcome metrics."""

    api_version = "v1beta1"

    def _patched(self, outcome: str, status: int, **overrides):
        """Assert an outcome with the given TestRun method patches."""
        patches = {
            "download_suite": AsyncMock(return_value=V1BETA1_SUITE),
            "validate_test_runners": AsyncMock(),
            "wait_for_artifact": AsyncMock(return_value=ARTIFACT),
            "create": AsyncMock(),
        }
        body = overrides.pop("body", None)
        patches.update(overrides)
        with ExitStack() as stack:
            for name, mock in patches.items():
                if mock is not None:
                    stack.enter_context(patch(f"{V1BETA1_TESTRUN}.{name}", mock))
            self._assert_outcome(outcome, status, body)

    def test_success(self):
        """Test that a successful submission is recorded as success.

        Approval criteria:
            - A successful submission shall increment only the success outcome with a duration.
        """
        self._patched("success", 200)

    def test_suite_failures(self):
        """Test that suite download, validation and dataset failures record their stage.

        Approval criteria:
            - A download failure shall return 400 and record suite_download_failed.
            - A validation failure shall return 400 and record suite_invalid.
            - A dataset count mismatch shall return 400 and record suite_invalid.
        """
        self._patched(
            "suite_download_failed", 400, download_suite=AsyncMock(side_effect=AssertionError)
        )
        self._patched(
            "suite_invalid", 400, validate_test_runners=AsyncMock(side_effect=AssertionError)
        )
        self._patched("suite_invalid", 400, body={**START_BODY, "dataset": [{}, {}]})

    def test_artifact_failures(self):
        """Test that artifact lookup failures keep their status and record their outcome.

        Approval criteria:
            - Not found shall return 400, timeout 504 and event repository errors 400.
        """
        for outcome, status, side_effect in (
            ("artifact_not_found", 400, None),
            ("artifact_lookup_timeout", 504, TimeoutError()),
            ("event_repository_error", 400, ConnectionError("down")),
        ):
            with self.subTest(outcome=outcome):
                with patch(
                    "etos_api.routers.v1beta1.testrun.wait_for_artifact_created",
                    AsyncMock(return_value=None, side_effect=side_effect),
                ):
                    self._patched(outcome, status, wait_for_artifact=None)

    def test_testrun_create_failed(self):
        """Test that TestRun creation failures are recorded as testrun_create_failed.

        Approval criteria:
            - A rejected create shall return 500 and record testrun_create_failed.
            - An unexpected create exception shall return 500 and record testrun_create_failed.
        """
        rejected = metrics.SubmissionFailed(
            "testrun_create_failed", status_code=500, detail="failed"
        )
        self._patched("testrun_create_failed", 500, create=AsyncMock(side_effect=rejected))
        self._patched("testrun_create_failed", 500, create=AsyncMock(side_effect=RuntimeError()))

    def test_internal_error(self):
        """Test that an unexpected exception outside a known stage is recorded as internal_error.

        Approval criteria:
            - An unexpected exception after artifact lookup shall record internal_error.
        """
        with patch(f"{V1BETA1_TESTRUN}.generate_name", AsyncMock(side_effect=RuntimeError())):
            self._patched("internal_error", 500)


class TestV1AlphaSubmissionMetrics(SubmissionMetricsMixin, TestCase):
    """Test v1alpha TestRun submission outcome metrics."""

    api_version = "v1alpha"
    router = "etos_api.routers.v1alpha.router"

    def _patched(self, outcome: str, status: int, **overrides):
        """Assert an outcome with the given v1alpha router patches."""
        testrun_client = MagicMock()
        testrun_client.create.return_value = overrides.pop("created", True)
        kubernetes = MagicMock()
        kubernetes.namespace = "etos"
        patches = {
            "download_suite": AsyncMock(return_value=[{"name": "suite"}]),
            "validate_suite": AsyncMock(),
            "wait_for_artifact_created": AsyncMock(return_value=EDGES),
            "ETOS": MagicMock(),
            "Kubernetes": MagicMock(return_value=kubernetes),
            "TestRunSpec": MagicMock(),
            "TestRunSchema": MagicMock(),
            "TestRun": MagicMock(return_value=testrun_client),
        }
        patches["ETOS"].return_value.debug.graphql_server = "http://localhost/graphql"
        patches.update(overrides)
        with (
            patch(f"{self.router}.download_suite", patches["download_suite"]),
            patch(f"{self.router}.validate_suite", patches["validate_suite"]),
            patch(f"{self.router}.wait_for_artifact_created", patches["wait_for_artifact_created"]),
            patch(f"{self.router}.ETOS", patches["ETOS"]),
            patch(f"{self.router}.Kubernetes", patches["Kubernetes"]),
            patch(f"{self.router}.TestRunSpec", patches["TestRunSpec"]),
            patch(f"{self.router}.TestRunSchema", patches["TestRunSchema"]),
            patch(f"{self.router}.TestRun", patches["TestRun"]),
        ):
            self._assert_outcome(outcome, status)

    def test_outcomes(self):
        """Test that each v1alpha submission stage records its outcome and keeps its status.

        Approval criteria:
            - Success shall return 200 and record success.
            - Download, validation, artifact and create failures shall record their stage.
        """
        cases = (
            ("success", 200, {}),
            (
                "suite_download_failed",
                500,
                {"download_suite": AsyncMock(side_effect=AssertionError)},
            ),
            ("suite_invalid", 400, {"validate_suite": AsyncMock(side_effect=HTTPException(400))}),
            (
                "artifact_not_found",
                400,
                {"wait_for_artifact_created": AsyncMock(return_value=None)},
            ),
            (
                "artifact_lookup_timeout",
                504,
                {"wait_for_artifact_created": AsyncMock(side_effect=TimeoutError)},
            ),
            (
                "event_repository_error",
                400,
                {"wait_for_artifact_created": AsyncMock(side_effect=ConnectionError)},
            ),
            ("testrun_create_failed", 500, {"created": False}),
        )
        for outcome, status, overrides in cases:
            with self.subTest(outcome=outcome):
                self._patched(outcome, status, **overrides)


class TestV0SubmissionMetrics(SubmissionMetricsMixin, TestCase):
    """Test v0 TestRun submission outcome metrics."""

    api_version = "v0"
    router = "etos_api.routers.v0.router"

    def _patched(self, outcome: str, status: int, **overrides):
        """Assert an outcome with the given v0 router patches."""
        etos = MagicMock()
        etos.debug.graphql_server = "http://localhost/graphql"
        etos.events.send.return_value.meta.event_id = "00000000-0000-0000-0000-000000000000"
        patches = {
            "validate_suite": AsyncMock(),
            "wait_for_artifact_created": AsyncMock(return_value=EDGES),
            "configure_testrun": AsyncMock(),
            "ETOS": MagicMock(return_value=etos),
        }
        patches.update(overrides)
        with (
            patch(f"{self.router}.validate_suite", patches["validate_suite"]),
            patch(f"{self.router}.wait_for_artifact_created", patches["wait_for_artifact_created"]),
            patch(f"{self.router}.configure_testrun", patches["configure_testrun"]),
            patch(f"{self.router}.ETOS", patches["ETOS"]),
        ):
            self._assert_outcome(outcome, status)

    def test_outcomes(self):
        """Test that each v0 submission stage records its outcome and keeps its status.

        Approval criteria:
            - Success shall return 200 and record success.
            - Artifact, environment configuration and publish failures shall record their stage.
        """
        failing_etos = MagicMock()
        failing_etos.events.send.side_effect = ConnectionError
        cases = (
            ("success", 200, {}),
            (
                "artifact_not_found",
                400,
                {"wait_for_artifact_created": AsyncMock(return_value=None)},
            ),
            (
                "environment_configuration_failed",
                400,
                {"configure_testrun": AsyncMock(side_effect=AssertionError)},
            ),
            ("event_publish_failed", 500, {"ETOS": MagicMock(return_value=failing_etos)}),
        )
        for outcome, status, overrides in cases:
            with self.subTest(outcome=outcome):
                self._patched(outcome, status, **overrides)

    def test_suite_failures(self):
        """Test that v0 suite download and validation failures are distinguished.

        Approval criteria:
            - Both shall return 400 with the unchanged validation failure detail.
            - A download failure shall record suite_download_failed.
            - A validation failure shall record suite_invalid.
        """
        utilities = "etos_api.routers.v0.utilities"
        with patch(f"{utilities}.download_suite", AsyncMock(side_effect=AssertionError("x"))):
            self._assert_outcome("suite_download_failed", 400)
        with (
            patch(f"{utilities}.download_suite", AsyncMock(return_value=[])),
            patch(f"{utilities}.SuiteValidator.validate", AsyncMock(side_effect=AssertionError)),
        ):
            self._assert_outcome("suite_invalid", 400)


class TestArtifactLookupMetrics(TestCase):
    """Test event repository polling metrics for every API version."""

    def _wait(self, api_version, responses, artifact_id=None, identity="pkg:x/y", timeout=30):
        """Run an API version's wait_for_artifact_created against a fake query handler."""
        module = f"etos_api.routers.{api_version}.utilities"
        handler = MagicMock()
        handler.execute = AsyncMock(side_effect=responses)
        with (
            patch(f"{module}.GraphqlQueryHandler", return_value=handler),
            patch(f"{module}.asyncio.sleep", AsyncMock()),
        ):
            utilities = __import__(module, fromlist=["wait_for_artifact_created"])
            return asyncio.run(
                utilities.wait_for_artifact_created(
                    MagicMock(), identity, artifact_id, timeout=timeout
                )
            )

    def test_found_after_retry(self):
        """Test that retries and a final found are recorded.

        Approval criteria:
            - Each not-ready attempt shall be counted as not_ready.
            - The final attempt shall be counted as found with one found lookup observation.
        """
        query = "artifact_by_identity"
        for api_version in metrics.SUBMISSION_OUTCOMES:
            with self.subTest(api_version=api_version):
                not_ready = _queries(api_version, query, "not_ready")
                found = _queries(api_version, query, "found")
                lookups = _lookups(api_version, query, "found")
                result = self._wait(api_version, [None, {"artifactCreated": {"edges": []}}, FOUND])
                self.assertEqual(result, EDGES)
                self.assertEqual(_queries(api_version, query, "not_ready"), not_ready + 2)
                self.assertEqual(_queries(api_version, query, "found"), found + 1)
                self.assertEqual(_lookups(api_version, query, "found"), lookups + 1)

    def test_not_found(self):
        """Test that an exhausted wait is recorded as not_found.

        Approval criteria:
            - An exhausted wait shall record a not_found lookup observation.
        """
        query = "artifact_by_id"
        for api_version in metrics.SUBMISSION_OUTCOMES:
            with self.subTest(api_version=api_version):
                lookups = _lookups(api_version, query, "not_found")
                self.assertIsNone(
                    self._wait(api_version, [], artifact_id="id", identity=None, timeout=0)
                )
                self.assertEqual(_lookups(api_version, query, "not_found"), lookups + 1)

    def test_error(self):
        """Test that an event repository error is recorded and re-raised.

        Approval criteria:
            - A query exception shall be re-raised unchanged.
            - It shall be counted as an error attempt and an error lookup.
        """
        query = "artifact_by_identity"
        for api_version in metrics.SUBMISSION_OUTCOMES:
            with self.subTest(api_version=api_version):
                errors = _queries(api_version, query, "error")
                lookups = _lookups(api_version, query, "error")
                with self.assertRaises(ConnectionError):
                    self._wait(api_version, ConnectionError("down"))
                self.assertEqual(_queries(api_version, query, "error"), errors + 1)
                self.assertEqual(_lookups(api_version, query, "error"), lookups + 1)


class TestRequestMetrics(TestCase):
    """Test the http_* request metrics."""

    def test_requests_recorded(self):
        """Test that request count and duration are recorded with status and ETOS version.

        Approval criteria:
            - A request to an instrumented endpoint shall increment http_requests_total with
              its status and observe http_request_duration_seconds.
        """
        labels = {"endpoint": "/api/v1beta1/testrun/{suite_id}", "operation": "stop_testrun"}
        count = _sample("http_requests_total", **labels, status="404")
        durations = _sample("http_request_duration_seconds_count", **labels)
        with patch("etos_api.routers.v1beta1.router.TestRun.delete", AsyncMock(return_value=False)):
            response = TestClient(APP).delete("/api/v1beta1/testrun/unknown")
        self.assertEqual(response.status_code, 404)
        self.assertEqual(_sample("http_requests_total", **labels, status="404"), count + 1)
        self.assertEqual(_sample("http_request_duration_seconds_count", **labels), durations + 1)


class TestBuildInfoAndInitialization(TestCase):
    """Test build information, the ETOS version label and label pre-initialization."""

    def tearDown(self):
        """Restore the metrics initialization for the test environment."""
        metrics.init_metrics()

    def test_etos_version(self):
        """Test that the ETOS version is set from ETOS_VERSION on build info and series.

        Approval criteria:
            - etos_api_build_info shall be 1 with etos_version from ETOS_VERSION and the
              etos_api package version.
            - Pre-initialized series shall carry the same etos_version.
        """
        with patch.dict(os.environ, {"ETOS_VERSION": "v9.9.9"}):
            metrics.init_metrics()
        self.assertEqual(metrics.ETOS_VERSION, "v9.9.9")
        self.assertEqual(
            REGISTRY.get_sample_value(
                "etos_api_build_info",
                {"etos_version": "v9.9.9", "version": metrics.package_version()},
            ),
            1,
        )
        self.assertEqual(_submissions("v1beta1", "success"), 0)

    def test_etos_version_unknown(self):
        """Test that a missing ETOS_VERSION is reported as unknown.

        Approval criteria:
            - etos_version shall be "unknown" when ETOS_VERSION is unset or empty.
        """
        with patch.dict(os.environ, {"ETOS_VERSION": ""}):
            metrics.init_metrics()
        self.assertEqual(
            REGISTRY.get_sample_value(
                "etos_api_build_info",
                {"etos_version": "unknown", "version": metrics.package_version()},
            ),
            1,
        )

    def test_pre_initialized_and_bounded(self):
        """Test that every known series is exported and no unbounded label value appears.

        Approval criteria:
            - Every known outcome and query combination shall be exported for each version.
            - No exported label value shall be outside the documented vocabularies.
        """
        for api_version, outcomes in metrics.SUBMISSION_OUTCOMES.items():
            for outcome in outcomes:
                self.assertIsNotNone(
                    REGISTRY.get_sample_value(
                        "etos_api_testrun_submissions_total",
                        {
                            "api_version": api_version,
                            "outcome": outcome,
                            "etos_version": metrics.ETOS_VERSION,
                        },
                    )
                )
            for query in metrics.ARTIFACT_QUERIES:
                for outcome in metrics.QUERY_ATTEMPT_OUTCOMES:
                    self.assertIsNotNone(
                        _sample_or_none(api_version, query, outcome), (api_version, query, outcome)
                    )
        allowed = {
            "api_version": set(metrics.SUBMISSION_OUTCOMES),
            "query": set(metrics.ARTIFACT_QUERIES),
            "outcome": {o for outcomes in metrics.SUBMISSION_OUTCOMES.values() for o in outcomes}
            | set(metrics.QUERY_ATTEMPT_OUTCOMES)
            | set(metrics.ARTIFACT_LOOKUP_OUTCOMES),
        }
        names = (
            "etos_api_testrun_submissions",
            "etos_api_testrun_submission_duration_seconds",
            "etos_api_event_repository_queries",
            "etos_api_artifact_lookup_duration_seconds",
        )
        for metric in REGISTRY.collect():
            if metric.name not in names:
                continue
            for sample in metric.samples:
                for label, values in allowed.items():
                    if label in sample.labels:
                        self.assertIn(sample.labels[label], values, sample)
                if "outcome" in sample.labels and "query" not in sample.labels:
                    self.assertIn(
                        sample.labels["outcome"],
                        metrics.SUBMISSION_OUTCOMES[sample.labels["api_version"]],
                        sample,
                    )


def _sample_or_none(api_version: str, query: str, outcome: str):
    """Return the event repository query sample value, or None if it is not exported."""
    return REGISTRY.get_sample_value(
        "etos_api_event_repository_queries_total",
        {
            "api_version": api_version,
            "query": query,
            "outcome": outcome,
            "etos_version": metrics.ETOS_VERSION,
        },
    )
