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
"""Utilities specific for the testrun endpoint."""

import re
import logging
import asyncio
import time
from typing import Any

import requests
from fastapi import HTTPException
from opentelemetry import trace
from etos_api.library.graphql import GraphqlQueryHandler
from etos_api.library.graphql_queries import (
    ARTIFACT_IDENTITY_QUERY,
    VERIFY_ARTIFACT_ID_EXISTS,
)
from etos_api.library.metrics import ArtifactLookup
from etos_api.library.validator import SuiteValidator

API_VERSION = "v1alpha"
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


async def download_suite(test_suite_url: str) -> list[dict[str, Any]]:
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


async def validate_suite(test_suite: list[dict[str, Any]]) -> None:
    """Validate the ETOS test suite through the SuiteValidator.

    :param test_suite_url: The URL to the test suite to validate.
    """
    span = trace.get_current_span()

    try:
        await SuiteValidator().validate(test_suite)
    except AssertionError as exception:
        LOGGER.error("Test suite validation failed!")
        LOGGER.error(exception)
        span.add_event("Test suite validation failed")
        raise HTTPException(
            status_code=400, detail=f"Test suite validation failed. {exception}"
        ) from exception


def convert_to_rfc1123(value: str) -> str:
    """Convert string to RFC-1123 accepted string.

    https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#dns-label-names

    Some resource types require their names to follow the DNS label standard as defined in RFC 1123.
    This means the name must:

        contain at most 63 characters
        contain only lowercase alphanumeric characters or '-'
        start with an alphanumeric character
        end with an alphanumeric character

    This method does not care about the length of the string since ETOS uses generateName for
    creating Kubernetes resources and that function will truncate the string down to 63-5 and
    then add 5 random characters.
    """
    # Replace all characters that are not alphanumeric (A-Z, a-z, 0-9) with a hyphen
    result = re.sub(r"[^A-Z\d]", "-", value, flags=re.IGNORECASE)
    # Remove leading hyphens
    result = re.sub(r"^-+", "", result)
    # Remove trailing hyphens
    result = re.sub(r"-+$", "", result)
    # Replace multiple consecutive hyphens with a single hyphen
    result = re.sub(r"-+", "-", result)
    return result.lower()


async def recipes_from_tests(tests: list[dict]) -> list[dict]:
    """Load Eiffel TERCC recipes from test.

    :param tests: The tests defined in a Test model.
    :return: A list of Eiffel TERCC recipes.
    """
    recipes: list[dict] = []
    for test in tests:
        recipes.append(
            {
                "id": test["id"],
                "testCase": test["testCase"],
                "constraints": [
                    {
                        "key": "ENVIRONMENT",
                        "value": test["execution"]["environment"],
                    },
                    {
                        "key": "COMMAND",
                        "value": test["execution"]["command"],
                    },
                    {
                        "key": "EXECUTE",
                        "value": test["execution"].get("execute", []),
                    },
                    {
                        "key": "CHECKOUT",
                        "value": test["execution"]["checkout"],
                    },
                    {
                        "key": "PARAMETERS",
                        "value": test["execution"]["parameters"],
                    },
                    {
                        "key": "TEST_RUNNER",
                        "value": test["execution"]["testRunner"],
                    },
                ],
            }
        )
    return recipes
