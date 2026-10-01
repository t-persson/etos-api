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
"""ETOS testrun router."""

import logging
import os
from typing import Annotated
from uuid import uuid4

from etos_lib import ETOS
from etos_lib.kubernetes import Environment, Kubernetes, TestRun
from etos_lib.kubernetes.schemas.v1alpha1.testrun import Image, Metadata, Providers, Retention
from etos_lib.kubernetes.schemas.v1alpha1.testrun import TestRun as TestRunSchema
from etos_lib.kubernetes.schemas.v1alpha1.testrun import TestRunner, TestRunSpec
from fastapi import Depends, FastAPI, HTTPException
from kubernetes.dynamic.exceptions import NotFoundError
from opentelemetry import baggage as otel_baggage
from opentelemetry import context as otel_context
from opentelemetry import trace
from opentelemetry.propagate import inject
from opentelemetry.trace import Span
from starlette.responses import Response

from etos_api.library.metrics import (
    COUNT_REQUESTS,
    OPERATIONS,
    SubmissionFailed,
    TestRunSubmission,
    count_invalid_submissions,
)
from etos_api.library.opentelemetry import context

from .schemas import AbortTestrunResponse, StartTestrunRequest, StartTestrunResponse
from .utilities import (
    convert_to_rfc1123,
    download_suite,
    recipes_from_tests,
    validate_suite,
    wait_for_artifact_created,
)

ETOSV1ALPHA = FastAPI(
    title="ETOS",
    version="v1alpha",
    summary="API endpoints for ETOS v1 Alpha",
    root_path_in_servers=False,
    dependencies=[Depends(context)],
)

API = f"/api/{ETOSV1ALPHA.version}/testrun"
START_LABELS = {"endpoint": API, "operation": OPERATIONS.start_testrun.name}
# The key {suite_id} is supposed to indicate that this is a path parameter, but
# we don't want to set the actual value in the metrics label since that would create
# a high cardinality metric. Therefore we use the literal string "{suite_id}".
STOP_LABELS = {"endpoint": f"{API}/{{suite_id}}", "operation": OPERATIONS.stop_testrun.name}
SUBSUITE_LABELS = {
    "endpoint": f"{API}/{{suite_id}}",
    "operation": OPERATIONS.get_subsuite.name,
}
count_invalid_submissions(ETOSV1ALPHA, "/testrun")

TRACER = trace.get_tracer("etos_api.routers.testrun.router")
LOGGER = logging.getLogger(__name__)
logging.getLogger("pika").setLevel(logging.WARNING)
# pylint:disable=too-many-locals,too-many-statements


@ETOSV1ALPHA.post("/testrun", tags=["etos"], response_model=StartTestrunResponse)
@COUNT_REQUESTS(START_LABELS, LOGGER)
async def start_testrun(
    etos: StartTestrunRequest, ctx: Annotated[otel_context.Context, Depends(context)]
) -> dict:
    """Start ETOS testrun on post.

    :param etos: ETOS pydantic model.
    :type etos: :obj:`etos_api.routers.etos.schemas.StartTestrunRequest`
    :param ctx: OpenTelemetry context with extracted headers.
    :type ctx: :obj:`opentelemetry.context.Context`
    :return: JSON dictionary with response.
    :rtype: dict
    """
    with TRACER.start_as_current_span("start-etos", context=ctx) as span:
        with TestRunSubmission(ETOSV1ALPHA.version) as submission:
            return await _create_testrun(etos, span, otel_context.get_current(), submission)


@ETOSV1ALPHA.delete("/testrun/{suite_id}", tags=["etos"], response_model=AbortTestrunResponse)
@COUNT_REQUESTS(STOP_LABELS, LOGGER)
async def abort_testrun(
    suite_id: str, ctx: Annotated[otel_context.Context, Depends(context)]
) -> dict:
    """Abort ETOS testrun on delete.

    :param suite_id: ETOS suite id
    :type suite_id: str
    :param ctx: OpenTelemetry context with extracted headers.
    :type ctx: :obj:`opentelemetry.context.Context`
    :return: JSON dictionary with response.
    :rtype: dict
    """
    with TRACER.start_as_current_span("abort-etos", context=ctx):
        return await _abort(suite_id)


# The key {suite_id} is supposed to indicate that this is a path parameter, but
# we don't want to set the actual value in the metrics label since that would create
# a high cardinality metric. Therefore we use the literal string "{suite_id}".
@ETOSV1ALPHA.get("/testrun/{sub_suite_id}", tags=["etos"])
@COUNT_REQUESTS(SUBSUITE_LABELS, LOGGER)
async def get_subsuite(sub_suite_id: str) -> dict:
    """Get sub suite returns the sub suite definition for the ETOS test runner.

    :param sub_suite_id: The name of the Environment kubernetes resource.
    :return: JSON dictionary with the Environment spec. Formatted to TERCC format.
    """
    environment_client = Environment(Kubernetes())
    try:
        environment_resource = environment_client.get(sub_suite_id)
    except NotFoundError:
        environment_resource = None
    if not environment_resource:
        raise HTTPException(404, "Failed to get environment")
    environment_spec = environment_resource.to_dict().get("spec", {})
    recipes = await recipes_from_tests(environment_spec["recipes"])
    environment_spec["recipes"] = recipes
    return environment_spec


@ETOSV1ALPHA.get("/ping", tags=["etos"], status_code=204)
async def health_check():
    """Check the status of the API and verify the client version."""
    return Response(status_code=204)


async def _create_testrun(
    etos: StartTestrunRequest,
    span: Span,
    ctx: otel_context.Context,
    submission: TestRunSubmission,
) -> dict:
    """Create a testrun for ETOS to execute.

    :param etos: Testrun pydantic model.
    :param span: An opentelemetry span for tracing.
    :param ctx: OpenTelemetry context with extracted headers.
    :param submission: Submission metrics recorder.
    :return: JSON dictionary with response.
    """
    testrun_id = str(uuid4())
    LOGGER.identifier.set(testrun_id)
    span.set_attribute("etos.id", testrun_id)
    span.set_attribute("etos.version", ETOSV1ALPHA.version)

    LOGGER.info("Download test suite.")
    submission.stage("suite_download_failed")
    span.set_attribute("etos.test_suite.uri", etos.test_suite_url)
    test_suite = await download_suite(etos.test_suite_url)
    LOGGER.info("Test suite downloaded.")

    LOGGER.info("Validating test suite.")
    submission.stage("suite_invalid")
    await validate_suite(test_suite)
    LOGGER.info("Test suite validated.")
    submission.stage("internal_error")

    etos_library = ETOS("ETOS API", os.getenv("HOSTNAME", "localhost"), "ETOS API")

    LOGGER.info("Get artifact created %r", (etos.artifact_identity or str(etos.artifact_id)))
    try:
        artifact = await wait_for_artifact_created(
            etos_library, etos.artifact_identity, etos.artifact_id
        )
    except TimeoutError as error:
        LOGGER.warning("Timeout error while waiting for artifact.")
        raise SubmissionFailed(
            "artifact_lookup_timeout",
            status_code=504,
            detail=(
                f"Timeout waiting for artifact {etos.artifact_identity or etos.artifact_id}, "
                "retry in 30s"
            ),
            headers={"Retry-After": "30"},
        ) from error
    except Exception as exception:  # pylint:disable=broad-except
        LOGGER.critical(exception)
        raise SubmissionFailed(
            "event_repository_error",
            status_code=400,
            detail=f"Could not connect to GraphQL. {exception}",
        ) from exception
    if artifact is None:
        if etos.artifact_id is not None:
            detail = f"Artifact with ID '{etos.artifact_id}' not found in the Event Repository."
        else:
            detail = (
                f"Artifact with identity '{etos.artifact_identity}' not found"
                " in the Event Repository."
            )
        raise SubmissionFailed("artifact_not_found", status_code=400, detail=detail)
    LOGGER.info("Found artifact created %r", artifact)
    # There are assumptions here. Since "edges" list is already tested
    # and we know that the return from GraphQL must be 'node'.'meta'.'id'
    # if there are "edges", this is fine.
    # Same goes for 'data'.'identity'.
    artifact_id = artifact[0]["node"]["meta"]["id"]
    identity = artifact[0]["node"]["data"]["identity"]
    span.set_attribute("etos.artifact.id", artifact_id)
    span.set_attribute("etos.artifact.identity", identity)

    submission.stage("suite_invalid")
    try:
        # Since the TERCC that we use can have multiple names, it's quite difficult to get a
        # single name that describes the entire TERCC. However ETOS mostly only gets a single
        # test suite or gets a suite that has a similar name for all suites in the TERCC and
        # for this reason we get the name of the first suite and that should be okay.
        name = test_suite[0].get("name")
        if name is None:
            raise HTTPException(status_code=400, detail="There's no name field in TERCC")
        # Convert to kubernetes accepted name
        name = convert_to_rfc1123(name)
        # Truncate and Add a hyphen at the end, if possible since it makes the generated name
        # easier to read. This truncation does not need to be validated since the generateName we
        # use when creating a TestRun will truncate the string if necessary.
        if not name.endswith("-"):
            # 63 is the max length, 5 is the random characters added by generateName and
            # 1 is to be able to fit a hyphen at the end so we truncate to 57 to fit everything.
            name = f"{name[:57]}-"
    except (IndexError, TypeError, ValueError):
        name = f"testrun-{testrun_id}-"
        LOGGER.error("Could not get name from test suite, defaulting to %s", name)
    submission.stage("internal_error")

    retention = Retention(
        failure=os.getenv("TESTRUN_FAILURE_RETENTION"),
        success=os.getenv("TESTRUN_SUCCESS_RETENTION"),
    )

    ctx = otel_baggage.set_baggage("testrun_id", testrun_id, context=ctx)
    ctx = otel_baggage.set_baggage("artifact_id", artifact_id, context=ctx)
    ctx = otel_baggage.set_baggage(
        "etos_cluster", os.getenv("ETOS_CLUSTER", "Unknown"), context=ctx
    )
    carrier = {}
    # inject() creates a dict with context reference,
    # e. g. {'traceparent': '00-0be6c260d9cbe9772298eaf19cb90a5b-371353ee8fbd3ced-01'}
    inject(carrier, context=ctx)

    annotations = {}
    if carrier.get("traceparent"):
        annotations["etos.eiffel-community.github.io/traceparent"] = carrier["traceparent"]
    if carrier.get("baggage"):
        annotations["etos.eiffel-community.github.io/baggage"] = carrier["baggage"]

    kubernetes = Kubernetes()
    submission.stage("suite_invalid")
    testrun_spec = TestRunSchema(
        metadata=Metadata(
            generateName=name,
            namespace=kubernetes.namespace,
            labels={
                "etos.eiffel-community.github.io/id": testrun_id,
                "etos.eiffel-community.github.io/cluster": os.getenv("ETOS_CLUSTER", "Unknown"),
            },
            annotations=annotations,
        ),
        spec=TestRunSpec(
            cluster=os.getenv("ETOS_CLUSTER", "Unknown"),
            timeout=etos.timeout,
            deadline=etos.deadline,
            id=testrun_id,
            retention=retention,
            suiteRunner=Image(
                image=os.getenv(
                    "SUITE_RUNNER_IMAGE",
                    "registry.nordix.org/eiffel/etos-suite-runner:latest",
                ),
                imagePullPolicy=os.getenv("SUITE_RUNNER_IMAGE_PULL_POLICY", "IfNotPresent"),
            ),
            logListener=Image(
                image=os.getenv(
                    "LOG_LISTENER_IMAGE",
                    "registry.nordix.org/eiffel/etos-log-listener:latest",
                ),
                imagePullPolicy=os.getenv("LOG_LISTENER_IMAGE_PULL_POLICY", "IfNotPresent"),
            ),
            environmentProvider=Image(
                image=os.getenv(
                    "ENVIRONMENT_PROVIDER_IMAGE",
                    "registry.nordix.org/eiffel/etos-environment-provider:latest",
                ),
                imagePullPolicy=os.getenv("ENVIRONMENT_PROVIDER_IMAGE_PULL_POLICY", "IfNotPresent"),
            ),
            artifact=artifact_id,
            identity=identity,
            testRunner=TestRunner(version=os.getenv("ETR_VERSION", "Unknown")),
            providers=Providers(
                iut=etos.iut_provider,
                executionSpace=etos.execution_space_provider,
                logArea=etos.log_area_provider,
            ),
            suites=TestRunSpec.from_tercc(test_suite, etos.dataset),
            suiteSource=etos.test_suite_url,
        ),
    )

    submission.stage("testrun_create_failed")
    testrun_client = TestRun(kubernetes)
    if not testrun_client.create(testrun_spec):
        raise HTTPException(status_code=500, detail="Failed to create testrun")
    submission.stage("internal_error")

    LOGGER.info("ETOS triggered successfully.")
    return {
        "tercc": testrun_id,
        "artifact_id": artifact_id,
        "artifact_identity": identity,
        "event_repository": etos_library.debug.graphql_server,
    }


async def _abort(suite_id: str) -> dict:
    """Abort a testrun by deleting the testrun resource."""
    testrun_client = TestRun(Kubernetes())
    response = testrun_client.client.delete(
        type="TestRun",
        namespace=testrun_client.namespace,
        label_selector=f"etos.eiffel-community.github.io/id={suite_id}",
    )  # type: ignore
    if not response.items:
        raise HTTPException(status_code=404, detail="Suite ID not found.")
    return {"message": f"Abort triggered for suite id: {suite_id}."}
