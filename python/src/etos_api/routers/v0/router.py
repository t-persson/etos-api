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
"""ETOS API router."""

import logging
import os
from typing import Annotated
from uuid import uuid4

from eiffellib.events import EiffelTestExecutionRecipeCollectionCreatedEvent
from etos_lib import ETOS
from etos_lib.kubernetes import Kubernetes
from fastapi import Depends, FastAPI, HTTPException
from kubernetes import client
from opentelemetry import baggage as otel_baggage
from opentelemetry import context as otel_context
from opentelemetry import trace
from opentelemetry.trace import Span
from starlette.responses import RedirectResponse, Response

from etos_api.library.environment import Configuration, configure_testrun
from etos_api.library.metrics import (
    COUNT_REQUESTS,
    OPERATIONS,
    SubmissionFailed,
    TestRunSubmission,
    count_invalid_submissions,
)
from etos_api.library.opentelemetry import context
from etos_api.library.utilities import sync_to_async

from .schemas import AbortEtosResponse, StartEtosRequest, StartEtosResponse
from .utilities import validate_suite, wait_for_artifact_created

ETOSV0 = FastAPI(
    title="ETOS",
    version="v0",
    summary="API endpoints for ETOS v0 - I.e. the version before versions",
    root_path_in_servers=False,
    dependencies=[Depends(context)],
)

API = f"/api/{ETOSV0.version}/etos"
START_LABELS = {"endpoint": API, "operation": OPERATIONS.start_testrun.name}
# The key {suite_id} is supposed to indicate that this is a path parameter, but
# we don't want to set the actual value in the metrics label since that would create
# a high cardinality metric. Therefore we use the literal string "{suite_id}".
STOP_LABELS = {"endpoint": f"{API}/{{suite_id}}", "operation": OPERATIONS.stop_testrun.name}
count_invalid_submissions(ETOSV0, "/etos")

TRACER = trace.get_tracer("etos_api.routers.etos.router")
LOGGER = logging.getLogger(__name__)
logging.getLogger("pika").setLevel(logging.WARNING)
# pylint:disable=too-many-locals,too-many-statements


@ETOSV0.post("/etos", tags=["etos"], response_model=StartEtosResponse)
@COUNT_REQUESTS(START_LABELS, LOGGER)
async def start_etos(
    etos: StartEtosRequest,
    ctx: Annotated[otel_context.Context, Depends(context)],
) -> dict:
    """Start ETOS execution on post.

    :param etos: ETOS pydantic model.
    :type etos: :obj:`etos_api.routers.etos.schemas.StartEtosRequest`
    :param ctx: OpenTelemetry context with extracted headers.
    :type ctx: :obj:`opentelemetry.context.Context`
    :return: JSON dictionary with response.
    :rtype: dict
    """
    with TRACER.start_as_current_span("start-etos", context=ctx) as span:
        with TestRunSubmission(ETOSV0.version) as submission:
            return await _start(etos, span, otel_context.get_current(), submission)


@ETOSV0.delete("/etos/{suite_id}", tags=["etos"], response_model=AbortEtosResponse)
@COUNT_REQUESTS(STOP_LABELS, LOGGER)
async def abort_etos(suite_id: str, ctx: Annotated[otel_context.Context, Depends(context)]) -> dict:
    """Abort ETOS execution on delete.

    :param suite_id: ETOS suite id
    :type suite_id: str
    :param ctx: OpenTelemetry context with extracted headers.
    :type ctx: :obj:`opentelemetry.context.Context`
    :return: JSON dictionary with response.
    :rtype: dict
    """
    with TRACER.start_as_current_span("abort-etos", context=ctx):
        return await _abort(suite_id)


@ETOSV0.get("/ping", tags=["etos"], status_code=204)
async def ping():
    """Ping the ETOS service in order to check if it is up and running.

    :return: HTTP 204 response.
    :rtype: :obj:`starlette.responses.Response`
    """
    return Response(status_code=204)


@ETOSV0.get("/selftest/ping")
async def oldping():
    """Ping the ETOS service in order to check if it is up and running.

    This is deprecated in favor of `/api/etos/ping`. Implementing here
    for backward compatibility. In newer API versions this shall not
    exist.
    """
    LOGGER.warning("DEPRECATED request to selftest/ping received!")
    return RedirectResponse("/api/ping")


async def _start(
    etos: StartEtosRequest,
    span: Span,
    ctx: otel_context.Context,
    submission: TestRunSubmission,
) -> dict:
    """Start ETOS execution.

    :param etos: ETOS pydantic model.
    :param span: An opentelemetry span for tracing.
    :param ctx: OpenTelemetry context with extracted headers.
    :param submission: Submission metrics recorder.
    :return: JSON dictionary with response.
    """
    tercc = EiffelTestExecutionRecipeCollectionCreatedEvent()
    LOGGER.identifier.set(tercc.meta.event_id)
    span.set_attribute("etos.id", tercc.meta.event_id)
    span.set_attribute("etos.version", ETOSV0.version)
    span.set_attribute(
        "parent_activity", str(etos.parent_activity) if etos.parent_activity else "None"
    )

    LOGGER.info("Validating test suite.")
    span.set_attribute("etos.test_suite.uri", etos.test_suite_url)
    submission.stage("suite_invalid")
    await validate_suite(etos.test_suite_url)
    LOGGER.info("Test suite validated.")
    submission.stage("internal_error")

    etos_library = ETOS("ETOS API", os.getenv("HOSTNAME"), "ETOS API")
    await sync_to_async(etos_library.config.rabbitmq_publisher_from_environment)

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

    if etos.parent_activity is not None:
        links = {"CAUSE": [artifact_id, str(etos.parent_activity)]}
    else:
        links = {"CAUSE": artifact_id}
    data = {
        "selectionStrategy": {"tracker": "Suite Builder", "id": str(uuid4())},
        "batchesUri": etos.test_suite_url,
    }
    config = Configuration(
        suite_id=tercc.meta.event_id,
        dataset=etos.dataset,
        execution_space_provider=etos.execution_space_provider,
        iut_provider=etos.iut_provider,
        log_area_provider=etos.log_area_provider,
    )
    submission.stage("environment_configuration_failed")
    try:
        # etcd lease expiration will have 10 minutes safety margin:
        etcd_lease_expiration_time = etos_library.debug.default_test_result_timeout + 10 * 60
        await configure_testrun(config, etcd_lease_expiration_time)
    except AssertionError as exception:
        LOGGER.critical(exception)
        raise HTTPException(
            status_code=400,
            detail=f"Could not configure environment provider. {exception}",
        ) from exception
    LOGGER.info("Environment provider configured.")
    submission.stage("internal_error")

    ctx = otel_baggage.set_baggage("testrun_id", tercc.meta.event_id, context=ctx)
    ctx = otel_baggage.set_baggage("artifact_id", artifact_id, context=ctx)

    LOGGER.info("Start event publisher.")
    submission.stage("event_publish_failed")
    await sync_to_async(etos_library.start_publisher)
    if not etos_library.debug.disable_sending_events:
        await sync_to_async(etos_library.publisher.wait_start)
    LOGGER.info("Event published started successfully.")
    LOGGER.info("Publish TERCC event.")
    try:
        event = etos_library.events.send(tercc, links, data, ctx=ctx)
        await sync_to_async(etos_library.publisher.wait_for_unpublished_events)
    finally:
        if not etos_library.debug.disable_sending_events:
            await sync_to_async(etos_library.publisher.stop)
            await sync_to_async(etos_library.publisher.wait_close)
    LOGGER.info("Event published.")
    submission.stage("internal_error")

    LOGGER.info("ETOS triggered successfully.")
    return {
        "tercc": event.meta.event_id,
        "artifact_id": artifact_id,
        "artifact_identity": identity,
        "event_repository": etos_library.debug.graphql_server,
    }


async def _abort(suite_id: str) -> dict:
    """Abort an ETOS v0 test suite execution."""
    kubernetes = Kubernetes()

    batch_api = client.BatchV1Api()
    jobs = batch_api.list_namespaced_job(namespace=kubernetes.namespace)

    delete_options = client.V1DeleteOptions(
        propagation_policy="Background"  # asynchronous cascading deletion
    )
    for job in jobs.items:
        if (
            job.metadata.labels.get("app") == "suite-runner"
            and job.metadata.labels.get("id") == suite_id
        ):
            batch_api.delete_namespaced_job(
                name=job.metadata.name, namespace=kubernetes.namespace, body=delete_options
            )
            LOGGER.info("Deleted suite-runner job: %s", job.metadata.name)
            break
    else:
        raise HTTPException(status_code=404, detail="Suite ID not found.")
    return {"message": f"Abort triggered for suite id: {suite_id}."}
