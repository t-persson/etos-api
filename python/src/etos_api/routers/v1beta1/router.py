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
from typing import Annotated

from etos_lib.kubernetes import Environment, Kubernetes
from fastapi import Depends, FastAPI, HTTPException
from kubernetes.dynamic.exceptions import NotFoundError
from opentelemetry import context as otel_context
from opentelemetry import trace
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
from .testrun import TestRun

ETOSV1BETA1 = FastAPI(
    title="ETOS",
    version="v1beta1",
    summary="API endpoints for ETOS v1 Beta",
    root_path_in_servers=False,
    dependencies=[Depends(context)],
)

API = f"/api/{ETOSV1BETA1.version}/testrun"
START_LABELS = {"endpoint": API, "operation": OPERATIONS.start_testrun.name}
# The key {suite_id} is supposed to indicate that this is a path parameter, but
# we don't want to set the actual value in the metrics label since that would create
# a high cardinality metric. Therefore we use the literal string "{suite_id}".
STOP_LABELS = {"endpoint": f"{API}/{{suite_id}}", "operation": OPERATIONS.stop_testrun.name}
SUBSUITE_LABELS = {
    "endpoint": f"{API}/{{sub_suite_id}}",
    "operation": OPERATIONS.get_subsuite.name,
}

count_invalid_submissions(ETOSV1BETA1, "/testrun")

TRACER = trace.get_tracer("etos_api.routers.testrun.router")
LOGGER = logging.getLogger(__name__)
logging.getLogger("pika").setLevel(logging.WARNING)
# pylint:disable=too-many-locals,too-many-statements


@ETOSV1BETA1.post("/testrun", tags=["etos"], response_model=StartTestrunResponse)
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
        return await _create_testrun(etos, span, otel_context.get_current())


@ETOSV1BETA1.delete("/testrun/{suite_id}", tags=["etos"], response_model=AbortTestrunResponse)
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
    with TRACER.start_as_current_span("abort-etos", context=ctx) as span:
        return await _abort(suite_id, span)


# The key {suite_id} is supposed to indicate that this is a path parameter, but
# we don't want to set the actual value in the metrics label since that would create
# a high cardinality metric. Therefore we use the literal string "{sub_suite_id}".
@ETOSV1BETA1.get("/testrun/{sub_suite_id}", tags=["etos"])
@COUNT_REQUESTS(SUBSUITE_LABELS, LOGGER)
async def get_subsuite(sub_suite_id: str) -> dict:
    """Get sub suite returns the sub suite definition for the ETOS test runner.

    :param sub_suite_id: The name of the Environment kubernetes resource.
    :return: JSON dictionary with the Environment spec. Formatted to TERCC format.
    """
    environment_client = Environment(Kubernetes(version="v1beta1"))
    try:
        environment_resource = environment_client.get(sub_suite_id)
        if not environment_resource:
            raise HTTPException(404, "Environment not found")
    except NotFoundError as error:
        raise HTTPException(404, "Environment not found") from error
    environment_spec = environment_resource.to_dict().get("spec", {})
    return environment_spec


@ETOSV1BETA1.get("/ping", tags=["etos"], status_code=204)
async def health_check():
    """Check the status of the API and verify the client version."""
    return Response(status_code=204)


async def _create_testrun(etos: StartTestrunRequest, span: Span, ctx: otel_context.Context) -> dict:
    """Create a testrun for ETOS to execute and record the submission outcome.

    :param etos: Testrun pydantic model.
    :param span: An opentelemetry span for tracing.
    :param ctx: OpenTelemetry context with extracted headers.
    :return: JSON dictionary with response.
    """
    with TestRunSubmission(ETOSV1BETA1.version) as submission:
        return await _submit_testrun(etos, span, ctx, submission)


async def _submit_testrun(
    etos: StartTestrunRequest,
    span: Span,
    ctx: otel_context.Context,
    submission: TestRunSubmission,
) -> dict:
    """Validate the request and create the testrun resource.

    :param etos: Testrun pydantic model.
    :param span: An opentelemetry span for tracing.
    :param ctx: OpenTelemetry context with extracted headers.
    :param submission: Submission metrics recorder.
    :return: JSON dictionary with response.
    """
    testrun = TestRun(span)

    try:
        test_suite = await testrun.download_suite(etos.test_suite_url)
    except Exception as error:
        raise SubmissionFailed(
            "suite_download_failed",
            status_code=400,
            detail=f"Failed to download test suite from {etos.test_suite_url}: {str(error)}",
        ) from error
    try:
        testrun_spec = await testrun.validate_suite(test_suite)
    except Exception as error:
        raise SubmissionFailed(
            "suite_invalid",
            status_code=400,
            detail=f"Failed to validate test suite: {str(error)}",
        ) from error

    datasets = etos.dataset
    if isinstance(datasets, list):
        if len(datasets) != len(testrun_spec.suites):
            raise SubmissionFailed(
                "suite_invalid",
                status_code=400,
                detail="If multiple datasets are provided, the number of datasets must correspond"
                " with number of test suites",
            )
    else:
        datasets = [datasets] * len(testrun_spec.suites)

    for suite in testrun_spec.suites:
        dataset = datasets.pop(0)
        if suite.dataset is None:
            suite.dataset = dict(dataset)
        else:
            suite.dataset.update(dataset)

    artifact = await testrun.wait_for_artifact(str(etos.artifact_id), etos.artifact_identity)
    testrun_name = await testrun.generate_name(testrun_spec.name)
    submission.stage("testrun_create_failed")
    await testrun.create(ctx, etos, testrun_name, artifact, testrun_spec)
    submission.stage("internal_error")

    return {
        "tercc": testrun.testrun_id,
        "artifact_id": artifact.artifact_id,
        "artifact_identity": artifact.identity,
        "event_repository": testrun.etos_library.debug.graphql_server,
    }


async def _abort(suite_id: str, span: Span) -> dict:
    """Abort a testrun by deleting the testrun resource."""
    testrun = TestRun(span)
    if not await testrun.delete(suite_id):
        raise HTTPException(status_code=404, detail="Suite ID not found.")
    return {"message": f"Abort triggered for suite id: {suite_id}."}
