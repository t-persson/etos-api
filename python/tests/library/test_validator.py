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
"""Tests for the validator library."""

import logging
import sys
from unittest.mock import patch

import pytest

from etos_api.library.validator import SuiteValidator

logging.basicConfig(level=logging.DEBUG, stream=sys.stdout)


class TestValidator:
    """Test the validator library."""

    logger = logging.getLogger(__name__)
    # Mark all test methods as asyncio methods to tell pytest to 'await' them.
    pytestmark = pytest.mark.asyncio

    @patch("etos_api.library.validator.Docker.digest")
    async def test_validate_proper_suite(self, digest_mock):
        """Test that the validator validates a proper suite correctly.

        Approval criteria:
            - Suite validator shall approve a proper suite.

        Test steps::
            1. Validate a proper suite.
            2. Verify that no exceptions were raised.
        """
        digest_mock.return_value = (
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        )
        test_suite = [
            {
                "name": "TestValidator",
                "priority": 1,
                "recipes": [
                    {
                        "constraints": [
                            {"key": "ENVIRONMENT", "value": {}},
                            {"key": "PARAMETERS", "value": {}},
                            {"key": "COMMAND", "value": "exit 0"},
                            {"key": "TEST_RUNNER", "value": "TestRunner"},
                            {"key": "EXECUTE", "value": []},
                            {"key": "CHECKOUT", "value": ["echo 'checkout'"]},
                        ],
                        "id": "131a7499-7ad4-4c4a-8a66-4e9ac95c7885",
                        "testCase": {
                            "id": "test_validate_proper_suite",
                            "tracker": "Github",
                            "url": "https://github.com/eiffel-community/etos-api",
                        },
                    }
                ],
            }
        ]
        self.logger.info("STEP: Validate a proper suite.")
        validator = SuiteValidator()
        try:
            await validator.validate(test_suite)
            exception = False
        except AssertionError, AssertionError:
            exception = True
        self.logger.info("STEP: Verify that no exceptions were raised.")
        assert exception is False

    async def test_validate_missing_constraints(self):
        """Test that the validator fails when missing required constraints.

        Approval criteria:
            - Suite validator shall not approve a suite with missing constraints.

        Test steps::
            1. Validate a suite with a missing constraint.
            2. Verify that the validator raises AssertionError.
        """
        test_suite = [
            {
                "name": "TestValidator",
                "priority": 1,
                "recipes": [
                    {
                        "constraints": [
                            {"key": "ENVIRONMENT", "value": {}},
                            {"key": "PARAMETERS", "value": {}},
                            {"key": "COMMAND", "value": "exit 0"},
                            {"key": "EXECUTE", "value": []},
                            {"key": "CHECKOUT", "value": ["echo 'checkout'"]},
                        ],
                        "id": "131a7499-7ad4-4c4a-8a66-4e9ac95c7887",
                        "testCase": {
                            "id": "test_validate_missing_constraints",
                            "tracker": "Github",
                            "url": "https://github.com/eiffel-community/etos-api",
                        },
                    }
                ],
            }
        ]  # TEST_RUNNER is missing
        self.logger.info("STEP: Validate a suite with a missing constraint.")
        validator = SuiteValidator()
        try:
            await validator.validate(test_suite)
            exception = False
        except AssertionError:
            exception = True
        self.logger.info("STEP: Verify that the validator raises AssertionError.")
        assert exception is True

    async def test_validate_wrong_types(self):
        """Test that the validator fails when constraints have the wrong types.

        Approval criteria:
            - Suite validator shall not approve a suite wrong constraint types.

        Test steps::
            1. For each constraint.
                1. Validate constraint with wrong type.
                2. Verify that the validator raises AssertionError.
        """
        base_suite = {
            "name": "TestValidator",
            "priority": 1,
            "recipes": [
                {
                    "constraints": [],  # Filled in loop
                    "id": "131a7499-7ad4-4c4a-8a66-4e9ac95c7886",
                    "testCase": {
                        "id": "test_validate_wrong_types",
                        "tracker": "Github",
                        "url": "https://github.com/eiffel-community/etos-api",
                    },
                }
            ],
        }
        constraints = [
            [
                {"key": "ENVIRONMENT", "value": "Wrong"},  # Wrong
                {"key": "PARAMETERS", "value": {}},
                {"key": "COMMAND", "value": "exit 0"},
                {"key": "TEST_RUNNER", "value": "TestRunner"},
                {"key": "EXECUTE", "value": []},
                {"key": "CHECKOUT", "value": ["echo 'checkout'"]},
            ],
            [
                {"key": "ENVIRONMENT", "value": {}},
                {"key": "PARAMETERS", "value": "Wrong"},  # Wrong
                {"key": "COMMAND", "value": "exit 0"},
                {"key": "TEST_RUNNER", "value": "TestRunner"},
                {"key": "EXECUTE", "value": []},
                {"key": "CHECKOUT", "value": ["echo 'checkout'"]},
            ],
            [
                {"key": "ENVIRONMENT", "value": {}},
                {"key": "PARAMETERS", "value": {}},
                {"key": "COMMAND", "value": {"wrong": True}},  # Wrong
                {"key": "TEST_RUNNER", "value": "TestRunner"},
                {"key": "EXECUTE", "value": []},
                {"key": "CHECKOUT", "value": ["echo 'checkout'"]},
            ],
            [
                {"key": "ENVIRONMENT", "value": {}},
                {"key": "PARAMETERS", "value": {}},
                {"key": "COMMAND", "value": "exit 0"},
                {"key": "TEST_RUNNER", "value": {"wrong": True}},  # Wrong
                {"key": "EXECUTE", "value": []},
                {"key": "CHECKOUT", "value": ["echo 'checkout'"]},
            ],
            [
                {"key": "ENVIRONMENT", "value": {}},
                {"key": "PARAMETERS", "value": {}},
                {"key": "COMMAND", "value": "exit 0"},
                {"key": "TEST_RUNNER", "value": "TestRunner"},
                {"key": "EXECUTE", "value": "Wrong"},  # Wrong
                {"key": "CHECKOUT", "value": ["echo 'checkout'"]},
            ],
            [
                {"key": "ENVIRONMENT", "value": {}},
                {"key": "PARAMETERS", "value": {}},
                {"key": "COMMAND", "value": "exit 0"},
                {"key": "TEST_RUNNER", "value": "TestRunner"},
                {"key": "EXECUTE", "value": []},
                {"key": "CHECKOUT", "value": "Wrong"},  # Wrong
            ],
        ]
        self.logger.info("STEP: For each constraint.")
        validator = SuiteValidator()
        for constraint in constraints:
            self.logger.info("STEP: Validate constraint with wrong type.")
            base_suite["recipes"][0]["constraints"] = constraint
            self.logger.info("STEP: Verify that the validator raises AssertionError.")
            with pytest.raises(AssertionError):
                await validator.validate([base_suite])

    async def test_validate_too_many_constraints(self):
        """Test that the validator fails when a constraint is defined multiple times.

        Approval criteria:
            - Suite validator shall not approve a suite with too many constraints.

        Test steps::
            1. Validate a suite with a constraint defined multiple times.
            2. Verify that the validator raises AssertionError.
        """
        test_suite = [
            {
                "name": "TestValidator",
                "priority": 1,
                "recipes": [
                    {
                        "constraints": [
                            {"key": "ENVIRONMENT", "value": {}},
                            {"key": "PARAMETERS", "value": {}},
                            {"key": "TEST_RUNNER", "value": "TestRunner"},
                            {"key": "TEST_RUNNER", "value": "AnotherTestRunner"},
                            {"key": "COMMAND", "value": "exit 0"},
                            {"key": "EXECUTE", "value": []},
                            {"key": "CHECKOUT", "value": ["echo 'checkout'"]},
                        ],
                        "id": "131a7499-7ad4-4c4a-8a66-4e9ac95c7887",
                        "testCase": {
                            "id": "test_validate_too_many_constraints",
                            "tracker": "Github",
                            "url": "https://github.com/eiffel-community/etos-api",
                        },
                    }
                ],
            }
        ]
        self.logger.info("STEP: Validate a suite with a constraint defined multiple times.")
        validator = SuiteValidator()
        try:
            await validator.validate(test_suite)
            exception = False
        except AssertionError:
            exception = True
        self.logger.info("STEP: Verify that the validator raises AssertionError.")
        assert exception is True

    async def test_validate_unknown_constraint(self):
        """Test that the validator fails when an unknown constraint is defined.

        Approval criteria:
            - Suite validator shall not approve a suite with an unknown constraint.

        Test steps::
            1. Validate a suite with an unknown constraint.
            2. Verify that the validator raises AssertionError.
        """
        test_suite = [
            {
                "name": "TestValidator",
                "priority": 1,
                "recipes": [
                    {
                        "constraints": [
                            {"key": "ENVIRONMENT", "value": {}},
                            {"key": "PARAMETERS", "value": {}},
                            {"key": "TEST_RUNNER", "value": "TestRunner"},
                            {"key": "COMMAND", "value": "exit 0"},
                            {"key": "EXECUTE", "value": []},
                            {"key": "CHECKOUT", "value": ["echo 'checkout'"]},
                            {"key": "UNKNOWN", "value": "Hello"},
                        ],
                        "id": "131a7499-7ad4-4c4a-8a66-4e9ac95c7887",
                        "testCase": {
                            "id": "test_validate_unknown_constraint",
                            "tracker": "Github",
                            "url": "https://github.com/eiffel-community/etos-api",
                        },
                    }
                ],
            }
        ]
        self.logger.info("STEP: Validate a suite with an unknown constraint.")
        validator = SuiteValidator()
        try:
            await validator.validate(test_suite)
            exception = False
        except TypeError:
            exception = True
        self.logger.info("STEP: Verify that the validator raises AssertionError.")
        assert exception is True

    async def test_validate_empty_constraints(self):
        """Test that required constraints are not empty.

        Approval criteria:
            - Constraints 'TEST_RUNNER', 'CHECKOUT' & 'COMMAND' shall not be empty.

        Test steps::
            1. For each required key.
                1. Validate a suite without the required key.
        """
        base_suite = {
            "name": "TestValidator",
            "priority": 1,
            "recipes": [
                {
                    "constraints": [],  # Filled below
                    "id": "131a7499-7ad4-4c4a-8a66-4e9ac95c7892",
                    "testCase": {
                        "id": "test_validate_empty_constraints",
                        "tracker": "Github",
                        "url": "https://github.com/eiffel-community/etos-api",
                    },
                }
            ],
        }
        constraints = [
            [
                {"key": "ENVIRONMENT", "value": {}},
                {"key": "PARAMETERS", "value": {}},
                {"key": "COMMAND", "value": ""},  # Empty
                {"key": "TEST_RUNNER", "value": "TestRunner"},
                {"key": "EXECUTE", "value": []},
                {"key": "CHECKOUT", "value": ["echo 'checkout'"]},
            ],
            [
                {"key": "ENVIRONMENT", "value": {}},
                {"key": "PARAMETERS", "value": {}},
                {"key": "COMMAND", "value": "exit 0"},
                {"key": "TEST_RUNNER", "value": ""},  # Empty
                {"key": "EXECUTE", "value": []},
                {"key": "CHECKOUT", "value": ["echo 'checkout'"]},
            ],
            [
                {"key": "ENVIRONMENT", "value": {}},
                {"key": "PARAMETERS", "value": {}},
                {"key": "COMMAND", "value": "exit 0"},
                {"key": "TEST_RUNNER", "value": "TestRunner"},
                {"key": "EXECUTE", "value": []},
                {"key": "CHECKOUT", "value": []},  # Empty
            ],
        ]
        self.logger.info("STEP: For each required key.")
        validator = SuiteValidator()
        for constraint in constraints:
            base_suite["recipes"][0]["constraints"] = constraint
            self.logger.info("STEP: Validate a suite without the required key.")
            with pytest.raises(AssertionError):
                await validator.validate([base_suite])
