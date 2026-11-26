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
"""Benchmark ETOS child starts from a terminal or a test runner."""

import argparse
import http.client
import json
import os
import threading
import traceback
import time
import urllib.error
import urllib.request
from collections.abc import Iterator
from concurrent.futures import ThreadPoolExecutor
from functools import lru_cache
from pathlib import Path
from urllib.parse import SplitResult, urlsplit
from uuid import UUID


def _start(api: str, body: bytes, ready: threading.Barrier) -> dict:
    """Start a child testrun against the API."""
    url = urlsplit(api)
    assert url.hostname is not None
    factory = http.client.HTTPSConnection if url.scheme == "https" else http.client.HTTPConnection
    connection = factory(url.hostname, url.port, timeout=60)
    ready.wait(timeout=10)
    began = time.monotonic()
    result: dict = {"status": None, "tercc": None}
    try:
        connection.request(
            "POST", "/api/v1beta1/testrun", body, {"Content-Type": "application/json"}
        )
        response = connection.getresponse()
        result["status"] = response.status
        if response.status == 200:
            result["tercc"] = str(UUID(json.loads(response.read(65536))["tercc"]))
    except (OSError, http.client.HTTPException, ValueError, TypeError, KeyError) as error:
        result["error"] = type(error).__name__
    finally:
        connection.close()
    result["seconds"] = round(time.monotonic() - began, 3)
    return result


def _burst(api: str, body: bytes, concurrency: int) -> dict:
    """Start a burst of child testruns, returning the result."""
    ready = threading.Barrier(concurrency)
    began = time.monotonic()
    with ThreadPoolExecutor(max_workers=concurrency) as executor:
        requests = [executor.submit(_start, api, body, ready) for _ in range(concurrency)]
        results = [request.result() for request in requests]
    duration = time.monotonic() - began
    accepted = {result["tercc"] for result in results if result["tercc"]}
    return {
        "concurrency": concurrency,
        "accepted": len(accepted),
        "accepted_per_second": round(len(accepted) / duration, 3),
        "slowest_accepted_seconds": max(
            (result["seconds"] for result in results if result["tercc"]), default=None
        ),
        "requests": results,
    }


def _concurrency_steps(maximum: int) -> Iterator[int]:
    """Yield the amount of concurrency steps, fibonacci up until maximum."""
    current, following = 1, 2
    while current < maximum:
        yield current
        current, following = following, current + following
    yield maximum


def _benchmark(config: dict, report_path: Path) -> int:
    """Run the etos-api-start-requests benchmark against an ETOS API service."""
    report: dict = {"benchmark": "etos-api-start-requests", "ok": False, "bursts": []}
    try:
        if not report_path.parent.is_dir():
            raise ValueError(f"Report directory does not exist: {report_path.parent}")
        body = json.dumps(config["body"]).encode()
        for concurrency in _concurrency_steps(config["max_concurrency"]):
            result = _burst(config["api"], body, concurrency)
            report["bursts"].append(result)
            if result["accepted"] != concurrency:
                raise ValueError("Incomplete burst; no POSTs retried and no further bursts sent")
    except urllib.error.HTTPError as error:
        report["error"] = f"Child suite preflight returned HTTP {error.code}"
    except urllib.error.URLError as error:
        report["error"] = f"Child suite preflight failed: {error.reason}"
    except (OSError, http.client.HTTPException, ValueError, threading.BrokenBarrierError) as error:
        report["error"] = str(error) if isinstance(error, ValueError) else type(error).__name__
    else:
        report["ok"] = True
    try:
        report_path.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    except OSError as error:
        report["ok"] = False
        report["report_write_error"] = type(error).__name__
    print(json.dumps(report), flush=True)
    return 0 if report["ok"] else 1


def _config(args: argparse.Namespace) -> dict:
    """Generate a testrun configuration."""
    url = urlsplit(args.api)
    if url.scheme not in {"http", "https"} or not url.hostname or url.username is not None:
        raise ValueError("--api must be an HTTP(S) origin with no credentials or path")
    if url.port == 0:
        raise ValueError("--api port must be nonzero")
    clean_url = f"{url.scheme}://{url.netloc}"
    return {
        "api": clean_url,
        "max_concurrency": args.max_concurrency,
        "body": {
            "artifact_id": str(UUID(args.artifact_id)),
            "artifact_identity": None,
            "test_suite_url": args.child_suite_url,
            "dataset": {},
            "timeout": 600,
            "execution_space_provider": args.execution_space_provider,
            "iut_provider": args.iut_provider,
            "log_area_provider": args.log_area_provider,
        },
    }


@lru_cache
def _environment(url: SplitResult) -> dict:
    """Fetch and environment from the ETOS API."""
    assert url.hostname is not None
    factory = http.client.HTTPSConnection if url.scheme == "https" else http.client.HTTPConnection
    connection = factory(url.hostname, url.port, timeout=60)

    timeout = time.time() + 10
    while time.time() < timeout:
        connection.request("GET", str(url.geturl()))
        response = connection.getresponse()
        if response.status == 200:
            environment = json.loads(response.read(655536))
            return environment
        print(f"Status code: {response.status}")
        time.sleep(1)
    raise TimeoutError(f"timed out waiting for {url.geturl()}")


def _artifact_id_from_environment() -> str:
    """Fetch artifact ID from the environment."""
    environment_url_str = os.getenv("ENVIRONMENT_URL")
    if environment_url_str is None:
        return ""
    url = urlsplit(environment_url_str)
    try:
        return _environment(url)["artifact"]
    except (json.JSONDecodeError, KeyError):
        traceback.print_exc()
        return ""


def _provider_from_environment(provider_key: str) -> str:
    """Fetch provider from the environment."""
    environment_url_str = os.getenv("ENVIRONMENT_URL")
    if environment_url_str is None:
        return ""
    url = urlsplit(environment_url_str)
    try:
        return _environment(url)[provider_key]["provider_id"]
    except (json.JSONDecodeError, KeyError):
        traceback.print_exc()
        return ""


def main() -> int:
    """Run the benchmark and write its report.

    :return: Zero on success, nonzero if the sweep does not complete.
    """
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api", default=os.getenv("ETOS_API"), help="ETOS API origin")
    parser.add_argument("--child-suite-url", required=True, help="Uploaded print-only suite URL")
    parser.add_argument("--artifact-id", help="Existing artifact UUID for child starts")
    parser.add_argument("--iut-provider", help="IUT provider name")
    parser.add_argument("--execution-space-provider", help="Execution space provider name")
    parser.add_argument("--log-area-provider", help="Log area provider name")
    parser.add_argument(
        "--max-concurrency",
        type=int,
        default=4,
        help="Number of parallel POSTs in the largest burst (default: 4)",
    )
    args = parser.parse_args()
    print(os.environ)
    print(os.environ["ENVIRONMENT_URL"])
    if args.api == "":
        parser.error("--api or environment ETOS_API must be set")
    if args.max_concurrency < 1:
        parser.error("--max-concurrency must be a positive integer")
    if args.artifact_id is None:
        args.artifact_id = _artifact_id_from_environment()
        if args.artifact_id == "":
            parser.error("--artifact-id or environment ENVIRONMENT_URL must be set")
    if args.iut_provider is None:
        args.iut_provider = _provider_from_environment("iut")
        if args.iut_provider == "":
            parser.error("--iut-provider or environment ENVIRONMENT_URL must be set")
    if args.execution_space_provider is None:
        args.execution_space_provider = _provider_from_environment("executor")
        if args.execution_space_provider == "":
            parser.error("--execution-space-provider or environment ENVIRONMENT_URL must be set")
    if args.log_area_provider is None:
        args.log_area_provider = _provider_from_environment("logArea")
        if args.log_area_provider == "":
            parser.error("--log-area-provider or environment ENVIRONMENT_URL must be set")
    try:
        config = _config(args)
    except (OSError, ValueError) as error:
        parser.error(str(error))
    directory = Path(os.environ.get("ARTIFACT_PATH") or ".")
    return _benchmark(config, directory / "etos-api-start-report.json")


if __name__ == "__main__":
    raise SystemExit(main())
