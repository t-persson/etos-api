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
"""Print an ETOS benchmark report as a terminal table."""

import argparse
import json
from pathlib import Path


def _row(burst: dict) -> tuple[str, ...]:
    """A single burst row."""
    concurrency = burst["concurrency"]
    accepted = burst["accepted"]
    rate = burst["accepted_per_second"]
    slowest = burst["slowest_accepted_seconds"]
    return (
        f"{concurrency:d}",
        f"{accepted:d}",
        f"{concurrency - accepted:d}",
        f"{rate:.3f}",
        "-" if slowest is None else f"{slowest:.3f}",
    )


def _table(rows: list[tuple[str, ...]]) -> str:
    """Generate the table for the report."""
    headers = ("Concurrency", "Accepted", "Not accepted", "Accepted/s", "Slowest accepted (s)")
    widths = [max(map(len, column)) for column in zip(headers, *rows, strict=True)]
    header = "  ".join(value.ljust(width) for value, width in zip(headers, widths, strict=True))
    divider = "  ".join("-" * width for width in widths)
    body = [
        "  ".join(value.rjust(width) for value, width in zip(row, widths, strict=True))
        for row in rows
    ]
    return "\n".join([header, divider, *body])


def main() -> None:
    """Read the report and print one row per completed or attempted burst."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("report", type=Path, nargs="?", default=Path("etos-api-start-report.json"))
    args = parser.parse_args()
    try:
        report = json.loads(args.report.read_text(encoding="utf-8"))
        if not isinstance(report.get("ok"), bool) or not isinstance(report.get("bursts"), list):
            raise TypeError("expected a benchmark status and list of bursts")
        rows = [_row(burst) for burst in report["bursts"]]
    except (OSError, KeyError, TypeError, ValueError) as error:
        parser.error(f"{args.report}: {error}")
    print(f"Benchmark: {report.get('benchmark', 'Unknown')}")
    print(_table(rows) if rows else "No benchmark bursts recorded.")
    if not report["ok"]:
        reason = report.get("error") or report.get("report_write_error") or "unknown error"
        print(f"Benchmark failed: {reason}")


if __name__ == "__main__":
    main()
