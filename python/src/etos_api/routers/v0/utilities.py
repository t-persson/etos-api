# Copyright 2020-2021 Axis Communications AB.
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
"""Utilities specific for the ETOS endpoint."""

import logging
import asyncio
import time

import requests
from opentelemetry import trace

from etos_api.library.graphql import GraphqlQueryHandler
from etos_api.library.graphql_queries import (
    ARTIFACT_IDENTITY_QUERY,
    VERIFY_ARTIFACT_ID_EXISTS,
)
from etos_api.library.metrics import ArtifactLookup, SubmissionFailed
from etos_api.library.validator import SuiteValidator

API_VERSION = "v0"
LOGGER = logging.getLogger(__name__)


async def wait_for_artifact_created(etos_library, artifact_identity, artifact_id, timeout=30):
    """Execute graphql query and wait for an artifact created.

    :param etos_library: ETOS library instande.
    :type etos_library: :obj:`etos_lib.etos.ETOS`
    :param artifact_identity: Identity of the artifact to get.
    :type artifact_identity: str
    :param artifact_id: ID of the artifact to get.
    :type artifact_id: UUID
    :param timeout: Maximum time to wait for a response (seconds).
    :type timeout: int
    :return: ArtifactCreated edges from GraphQL.
    :rtype: list
    """
    timeout = time.time() + timeout
    query_handler = GraphqlQueryHandler(etos_library)
    if artifact_id is not None:
        LOGGER.info("Verify that artifact ID %r exists.", artifact_id)
        query = VERIFY_ARTIFACT_ID_EXISTS
    elif artifact_identity is not None:
        LOGGER.info("Getting artifact from packageURL %r", artifact_identity)
        query = ARTIFACT_IDENTITY_QUERY
        if artifact_identity.startswith("pkg:"):
            # This makes the '$regex' query to the event repository more efficient.
            artifact_identity = f"^{artifact_identity}"
    else:
        raise ValueError("'artifact_id' and 'artifact_identity' are both None!")
    artifact_identifier = artifact_identity or str(artifact_id)

    lookup = ArtifactLookup(API_VERSION, artifact_id)
    LOGGER.debug("Wait for artifact created event.")
    while time.time() < timeout:
        try:
            artifacts = await query_handler.execute(query % artifact_identifier)
            assert artifacts is not None
            assert artifacts["artifactCreated"]["edges"]
        except (AssertionError, KeyError):
            lookup.attempt("not_ready")
            LOGGER.warning("Artifact created not ready yet")
        except Exception:
            lookup.attempt("error", final=True)
            raise
        else:
            lookup.attempt("found", final=True)
            return artifacts["artifactCreated"]["edges"]
        await asyncio.sleep(2)
    lookup.finish("not_found")
    LOGGER.error("Artifact %r not found.", artifact_identifier)
    return None


async def download_suite(test_suite_url: str) -> dict:
    """Attempt to download suite.

    :param test_suite_url: URL to test suite to download.
    :return: Downloaded test suite as JSON.
    """
    try:
        suite = requests.get(test_suite_url, timeout=60)
        suite.raise_for_status()
    except Exception as exception:  # pylint:disable=broad-except
        raise AssertionError(f"Unable to download suite from {test_suite_url}") from exception
    return suite.json()


async def validate_suite(test_suite_url: str) -> None:
    """Validate the ETOS test suite through the SuiteValidator.

    :param test_suite_url: The URL to the test suite to validate.
    """
    span = trace.get_current_span()

    outcome = "suite_download_failed"
    try:
        test_suite = await download_suite(test_suite_url)
        outcome = "suite_invalid"
        await SuiteValidator().validate(test_suite)
    except AssertionError as exception:
        LOGGER.error("Test suite validation failed!")
        LOGGER.error(exception)
        span.add_event("Test suite validation failed")
        raise SubmissionFailed(
            outcome, status_code=400, detail=f"Test suite validation failed. {exception}"
        ) from exception
