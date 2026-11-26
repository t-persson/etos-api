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
"""ETOS API v1beta1 router tests."""

import logging
import sys
from unittest import TestCase
from unittest.mock import AsyncMock, Mock, patch
from uuid import uuid4

from fastapi.testclient import TestClient
from kubernetes.client.exceptions import ApiException
from kubernetes.dynamic.exceptions import ConflictError, NotFoundError

from etos_api.main import APP
from etos_api.routers.v1beta1.schemas import StartTestrunRequest, StartTestrunResponse
from etos_api.routers.v1beta1.testrun import Artifact, IdempotentStart

logging.basicConfig(level=logging.DEBUG, stream=sys.stdout)


def _test_suite(datasets: list) -> dict:
    """Create a v1beta1 test suite with one suite per dataset (None meaning no dataset key)."""
    suites = []
    for index, dataset in enumerate(datasets):
        suite = {
            "priority": 1,
            "testExecutions": [
                {
                    "id": f"00000000-0000-0000-0000-00000000000{index}",
                    "testCase": {"id": f"test_{index}", "version": "main"},
                    "execution": {
                        "checkout": ["git clone https://example.com/tests.git"],
                        "command": "pytest",
                    },
                    "environment": {
                        "environmentVariables": {},
                        "testRunner": "example.com/test-runner:latest",
                    },
                }
            ],
        }
        if dataset is not None:
            suite["dataset"] = dataset
        suites.append(suite)
    return {"name": "TestSuite", "schemaVersion": "v1beta1", "suites": suites}


class TestV1Beta1Router(TestCase):
    """Test the v1beta1 router in etos-api."""

    client = TestClient(APP)

    def _start(self, test_suite: dict, dataset) -> list:
        """Start a testrun and return the suites passed to TestRun.create."""
        artifact = Artifact(artifact_id="11111111-1111-1111-1111-111111111111", identity="pkg:x/y")
        with (
            patch(
                "etos_api.routers.v1beta1.testrun.TestRun.download_suite",
                AsyncMock(return_value=test_suite),
            ),
            patch("etos_api.routers.v1beta1.testrun.TestRun.validate_test_runners", AsyncMock()),
            patch(
                "etos_api.routers.v1beta1.testrun.TestRun.wait_for_artifact",
                AsyncMock(return_value=artifact),
            ),
            patch(
                "etos_api.routers.v1beta1.testrun.TestRun.create",
                AsyncMock(
                    return_value=StartTestrunResponse(
                        tercc=uuid4(),
                        artifact_id=artifact.artifact_id,
                        artifact_identity=artifact.identity,
                        event_repository="http://localhost",
                    )
                ),
            ) as create_mock,
        ):
            response = self.client.post(
                "/api/v1beta1/testrun",
                json={
                    "artifact_identity": "pkg:x/y",
                    "test_suite_url": "http://localhost/my_test.json",
                    "dataset": dataset,
                },
            )
        self.assertEqual(response.status_code, 200, response.text)
        create_mock.assert_awaited_once()
        return create_mock.await_args.args[-1].suites

    def test_start_testrun_suite_without_dataset(self):
        """Test that a request dataset is applied to a suite that has no dataset.

        Approval criteria:
            - POST requests to v1beta1 testrun shall return 200.
            - The request dataset shall be set on the suite.

        Test steps::
            1. Send a start request for a test suite without a dataset.
            2. Verify that the request dataset is set on the suite.
        """
        suites = self._start(_test_suite([None]), {"key": "value"})
        self.assertDictEqual(suites[0].dataset, {"key": "value"})

    def test_start_testrun_creation_failure_returns_server_error(self):
        """Verify unsuccessful Kubernetes creation returns 500 instead of API success."""
        artifact = Artifact(artifact_id="11111111-1111-1111-1111-111111111111", identity="pkg:x/y")
        with (
            patch(
                "etos_api.routers.v1beta1.testrun.TestRun.download_suite",
                AsyncMock(return_value=_test_suite([None])),
            ),
            patch("etos_api.routers.v1beta1.testrun.TestRun.validate_test_runners", AsyncMock()),
            patch(
                "etos_api.routers.v1beta1.testrun.TestRun.wait_for_artifact",
                AsyncMock(return_value=artifact),
            ),
            patch("etos_api.routers.v1beta1.testrun.Kubernetes") as kubernetes,
            patch("etos_api.routers.v1beta1.testrun.TestRunClient") as client,
        ):
            kubernetes.return_value.namespace = "etos-test"
            client.return_value.create.return_value = False
            response = self.client.post(
                "/api/v1beta1/testrun",
                json={
                    "artifact_identity": "pkg:x/y",
                    "test_suite_url": "http://localhost/my_test.json",
                },
            )
        self.assertEqual(response.status_code, 500, response.text)
        self.assertEqual(response.json()["detail"], "Failed to create testrun")
        client.return_value.create.assert_called_once()
        created = client.return_value.create.call_args.args[0]
        self.assertIsNone(created.metadata.name)
        self.assertEqual(created.metadata.generateName, "testsuite-")

    def test_start_testrun_suite_with_dataset(self):
        """Test that a request dataset is merged into a suite dataset.

        Approval criteria:
            - POST requests to v1beta1 testrun shall return 200.
            - The request dataset shall be merged into the suite dataset.

        Test steps::
            1. Send a start request for a test suite with a dataset.
            2. Verify that the request dataset is merged into the suite dataset.
        """
        suites = self._start(_test_suite([{"a": 1, "key": "old"}]), {"key": "value"})
        self.assertDictEqual(suites[0].dataset, {"a": 1, "key": "value"})

    def test_start_testrun_suites_without_dataset_do_not_share(self):
        """Test that suites without a dataset do not share the same dataset object.

        Approval criteria:
            - POST requests to v1beta1 testrun shall return 200.
            - Each suite shall get its own copy of the request dataset.

        Test steps::
            1. Send a start request for a test suite with two suites without a dataset.
            2. Verify that each suite has the dataset and that they are separate objects.
        """
        suites = self._start(_test_suite([None, None]), {"key": "value"})
        self.assertDictEqual(suites[0].dataset, {"key": "value"})
        self.assertDictEqual(suites[1].dataset, {"key": "value"})
        self.assertIsNot(suites[0].dataset, suites[1].dataset)

    def test_keyed_start_replays_the_stored_run(self) -> None:
        """Recover the same run after response loss or a concurrent create conflict."""
        body = {"artifact_identity": "pkg:x/y", "test_suite_url": "http://localhost/suite.yaml"}
        path = "/api/v1beta1/testrun"
        headers = {"Idempotency-Key": str(uuid4())}
        artifact = Artifact(artifact_id=str(uuid4()), identity="pkg:x/y")
        with (
            patch(
                "etos_api.routers.v1beta1.testrun.TestRun.download_suite",
                AsyncMock(side_effect=lambda _: _test_suite([None])),
            ) as download,
            patch("etos_api.routers.v1beta1.testrun.TestRun.validate_test_runners", AsyncMock()),
            patch(
                "etos_api.routers.v1beta1.testrun.TestRun.wait_for_artifact",
                AsyncMock(return_value=artifact),
            ),
            patch("etos_api.routers.v1beta1.testrun.Kubernetes") as kubernetes,
            patch("etos_api.routers.v1beta1.testrun.TestRunClient") as client_class,
        ):
            kubernetes.return_value.namespace = "etos-test"
            client = client_class.return_value
            missing = NotFoundError(ApiException(status=404))
            client.get.side_effect = missing
            first = self.client.post(path, headers=headers, json=body)
            self.assertEqual(first.status_code, 200, first.text)
            self.assertNotEqual(first.json()["tercc"], headers["Idempotency-Key"])
            created = client.create.call_args.args[0]
            self.assertIsNone(created.metadata.generateName)
            self.assertRegex(created.metadata.name, r"^testsuite-[0-9a-f]{32}$")
            client.get.assert_called_once_with(created.metadata.name)
            stored = Mock()
            stored.to_dict.return_value = created.model_dump()

            def get_stored(name: str) -> Mock:
                if name == created.metadata.name:
                    return stored
                raise NotFoundError(ApiException(status=404))

            client.get.side_effect = get_stored
            self.assertEqual(
                self.client.post(path, headers=headers, json=body).json(), first.json()
            )
            client.get.assert_called_with(created.metadata.name)
            self.assertEqual(
                self.client.post(
                    path, headers=headers, json={**body, "dataset": {"x": 1}}
                ).status_code,
                409,
            )
            self.assertEqual(download.await_count, 3)
            client.create.assert_called_once()

            client.get.side_effect = [missing, stored]
            client.create.side_effect = ConflictError(ApiException(status=409))
            self.assertEqual(
                self.client.post(path, headers=headers, json=body).json(), first.json()
            )

            client.get.side_effect = missing
            client.create.side_effect = None
            other = self.client.post(path, headers={"Idempotency-Key": str(uuid4())}, json=body)
            self.assertEqual(other.status_code, 200, other.text)
            self.assertNotEqual(other.json()["tercc"], first.json()["tercc"])
            self.assertNotEqual(
                client.create.call_args.args[0].metadata.name, created.metadata.name
            )

    def test_keyed_name_handles_long_and_non_dns_suite_names(self) -> None:
        """Keep keyed names valid and deterministic for unusual suite names."""
        request = StartTestrunRequest(
            artifact_identity="pkg:x/y", test_suite_url="http://localhost/suite.yaml"
        )
        start = IdempotentStart.from_request(uuid4(), request)
        name = start.name_for_suite("A" * 29 + " /" + "B" * 40)
        self.assertRegex(name, r"^a{29}-[0-9a-f]{32}$")
        self.assertEqual(start.name_for_suite("!!!"), "testrun-" + name.rsplit("-", 1)[1])
