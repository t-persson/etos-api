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
"""ETOS testrun."""

import logging
import os
from typing import Optional
from uuid import uuid4

import aiohttp
from etos_lib import ETOS
from etos_lib.kubernetes import Kubernetes
from etos_lib.kubernetes import TestRun as TestRunClient
from etos_lib.kubernetes.schemas.v1beta1.testrun import Metadata, Providers, Retention, Suite
from etos_lib.kubernetes.schemas.v1beta1.testrun import TestRun as TestRunSchema
from etos_lib.kubernetes.schemas.v1beta1.testrun import TestRunSpec
from opentelemetry import baggage as otel_baggage
from opentelemetry import context as otel_context
from opentelemetry.propagate import inject
from opentelemetry.trace import Span
from pydantic import BaseModel
from yaml import safe_load

from etos_api.library.docker import Docker
from etos_api.library.metrics import SubmissionFailed

from .schemas import StartTestrunRequest
from .utilities import convert_to_rfc1123, wait_for_artifact_created


class MinimalSpec(BaseModel):
    """Minimal schema for validating the test suite.

    This is a subset of the TestRunSpec schema used for creating the testrun resource in Kubernetes.
    It is used to validate the test suite before creating the testrun resource, to ensure that all
    required fields are present and valid before proceeding with the testrun creation process.
    """

    name: str
    schemaVersion: str
    suites: list[Suite]


class Artifact(BaseModel):
    """Class representing an artifact in the Event Repository."""

    artifact_id: str
    identity: str


class TestRun:
    """Class representing a testrun in ETOS.

    Responsible for downloading and validating the test suite,
    waiting for the artifact to be created in the Event Repository, and
    creating the testrun resource in Kubernetes for ETOS to execute.
    """

    logger = logging.getLogger(__name__)

    def __init__(self, span: Span):
        """Initialize the TestRun class."""
        self.testrun_id = str(uuid4())
        self.span = span
        self.logger.identifier.set(self.testrun_id)
        self.etos_library = ETOS("ETOS API", os.getenv("HOSTNAME", "localhost"), "ETOS API")
        span.set_attribute("etos.id", self.testrun_id)
        span.set_attribute("etos.version", "v1beta1")

    async def download_suite(self, url: str) -> dict:
        """Download the test suite from the provided URL."""
        self.logger.info("Downloading test suite %r", url)
        self.span.set_attribute("etos.test_suite.uri", url)
        try:
            async with aiohttp.ClientSession() as session:
                async with session.get(url, timeout=60, raise_for_status=True) as response:
                    content = await response.read()
        except Exception as exception:  # pylint:disable=broad-except
            raise AssertionError(f"Unable to download suite from {url}") from exception
        self.logger.info("Test suite downloaded")
        return safe_load(content)

    async def validate_suite(self, test_suite: dict) -> MinimalSpec:
        """Validate the test suite against the MinimalSpec schema."""
        self.logger.info("Validating test suite")
        self.logger.debug(test_suite)
        testrun = MinimalSpec.model_validate(test_suite)
        await self.validate_test_runners(testrun)
        self.logger.info("Test suite validated")
        return testrun

    async def validate_test_runners(self, testrun: MinimalSpec):
        """Validate that all test runners in the test suite are available in the Docker registry."""
        docker = Docker()
        checked = set()
        for suite in testrun.suites:
            for execution in suite.testExecutions:
                test_runner = execution.environment.testRunner
                if test_runner in checked:
                    continue
                assert (
                    await docker.digest(test_runner) is not None
                ), f"Test runner {test_runner} not found"
                checked.add(test_runner)
        return testrun

    async def wait_for_artifact(
        self, artifact_id: Optional[str], identity: Optional[str]
    ) -> Artifact:
        """Wait for an artifact to be created in the Event Repository.

        The artifact can be identified by either its ID or its identity.
        """
        self.logger.info("Get artifact created %r", (identity or artifact_id))
        try:
            artifact = await wait_for_artifact_created(self.etos_library, identity, artifact_id)
        except TimeoutError as error:
            self.logger.warning("Timeout error while waiting for artifact.")
            raise SubmissionFailed(
                "artifact_lookup_timeout",
                status_code=504,
                detail=(f"Timeout waiting for artifact {identity or artifact_id}, retry in 30s"),
                headers={"Retry-After": "30"},
            ) from error
        except Exception as exception:  # pylint:disable=broad-except
            self.logger.critical(exception)
            raise SubmissionFailed(
                "event_repository_error",
                status_code=400,
                detail=f"Could not connect to GraphQL. {exception}",
            ) from exception
        if artifact is None:
            if artifact_id is not None:
                detail = f"Artifact with ID '{artifact_id}' not found in the Event Repository."
            else:
                detail = f"Artifact with identity '{identity}' not found in the Event Repository."
            raise SubmissionFailed("artifact_not_found", status_code=400, detail=detail)
        # There are assumptions here. Since "edges" list is already tested
        # and we know that the return from GraphQL must be 'node'.'meta'.'id'
        # if there are "edges", this is fine.
        # Same goes for 'data'.'identity'.
        artifact_id = artifact[0]["node"]["meta"]["id"]
        identity = artifact[0]["node"]["data"]["identity"]
        assert artifact_id is not None, "Artifact ID not found in GraphQL response"
        assert identity is not None, "Artifact identity not found in GraphQL response"
        self.span.set_attribute("etos.artifact.id", artifact_id)
        self.span.set_attribute("etos.artifact.identity", identity)

        self.logger.info("Found artifact created %r", artifact)
        return Artifact(artifact_id=artifact_id, identity=identity)

    async def generate_name(self, testrun_name: str) -> str:
        """Generate a name for the testrun resource in Kubernetes.

        The name is generated from the testrun name provided in the test suite.
        """
        # Convert to kubernetes accepted name
        name = convert_to_rfc1123(testrun_name)
        # Truncate and Add a hyphen at the end, if possible since it makes the generated name
        # easier to read. This truncation does not need to be validated since the generateName we
        # use when creating a TestRun will truncate the string if necessary.
        if not name.endswith("-"):
            # 63 is the max length, 5 is the random characters added by generateName and
            # 1 is to be able to fit a hyphen at the end so we truncate to 57 to fit everything.
            name = f"{name[:57]}-"
        return name

    async def create(
        self,
        ctx: otel_context.Context,
        etos: StartTestrunRequest,
        name: str,
        artifact: Artifact,
        minimal_spec: MinimalSpec,
    ):
        """Create a testrun resource in Kubernetes for ETOS to execute."""
        self.logger.info("Creating testrun with name: %r", name)
        self.logger.debug(artifact)
        self.logger.debug(minimal_spec)

        ctx = otel_baggage.set_baggage("testrun_id", self.testrun_id, context=ctx)
        ctx = otel_baggage.set_baggage("artifact_id", artifact.artifact_id, context=ctx)
        ctx = otel_baggage.set_baggage(
            "etos_cluster", os.getenv("ETOS_CLUSTER", "Unknown"), context=ctx
        )
        carrier: dict = {}
        # inject() creates a dict with context reference,
        # e. g. {'traceparent': '00-0be6c260d9cbe9772298eaf19cb90a5b-371353ee8fbd3ced-01'}
        inject(carrier, context=ctx)

        annotations = {}
        if carrier.get("traceparent"):
            annotations["etos.eiffel-community.github.io/traceparent"] = carrier["traceparent"]
        if carrier.get("baggage"):
            annotations["etos.eiffel-community.github.io/baggage"] = carrier["baggage"]

        kubernetes = Kubernetes(version="v1beta1")
        testrun = TestRunSchema(
            metadata=Metadata(
                generateName=name,
                namespace=kubernetes.namespace,
                labels={
                    "etos.eiffel-community.github.io/id": self.testrun_id,
                    "etos.eiffel-community.github.io/cluster": os.getenv("ETOS_CLUSTER", "Unknown"),
                },
                annotations=annotations,
            ),
            spec=TestRunSpec(
                schemaVersion=minimal_spec.schemaVersion,
                name=minimal_spec.name,
                suites=minimal_spec.suites,
                id=self.testrun_id,
                artifact=artifact.artifact_id,
                identity=artifact.identity,
                cluster=os.getenv("ETOS_CLUSTER"),
                providers=Providers(
                    iut=etos.iut_provider,
                    executionSpace=etos.execution_space_provider,
                    logArea=etos.log_area_provider,
                ),
                suiteSource=etos.test_suite_url,
                timeout=etos.timeout,
                deadline=etos.deadline,
                retention=Retention(
                    failure=os.getenv("TESTRUN_FAILURE_RETENTION"),
                    success=os.getenv("TESTRUN_SUCCESS_RETENTION"),
                ),
            ),
        )
        testrun_client = TestRunClient(kubernetes)
        if not testrun_client.create(testrun):
            raise SubmissionFailed(
                "testrun_create_failed", status_code=500, detail="Failed to create testrun"
            )
        self.logger.info("ETOS triggered successfully")

    async def delete(self, suite_id: str) -> bool:
        """Delete a testrun by suite_id. Returns True if a testrun was deleted, False otherwise."""
        kubernetes = Kubernetes(version="v1beta1")
        testrun_client = TestRunClient(kubernetes)
        response = testrun_client.client.delete(
            type="TestRun",
            namespace=testrun_client.namespace,
            field_selector=f"spec.id={suite_id}",
        )  # type: ignore
        return response and response.items and len(response.items) > 0
