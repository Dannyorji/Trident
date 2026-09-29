#!/usr/bin/env python3
"""Enforce a coverage floor on the hand-written Python SDK client (issue #325).

Reads a Cobertura ``coverage.xml`` and computes line coverage over the SDK's
hand-written sources only.

Generated models are excluded. ``openapi_models_gen.py`` is emitted by
``scripts/generate_sdk_models.py``, so "cover the generator's output" is not a
meaningful ask of a test suite — and including it drags the reported total to
roughly 53%, which would fail the build for a reason no test can fix.

The floor is set from a measured baseline (84.0% at the time it was
introduced), not from aspiration: a floor above the current figure turns the
next unrelated merge red and trains people to bypass the gate.

Usage:
    python scripts/check-python-sdk-coverage.py [coverage.xml] [--floor 75.0]
"""

from __future__ import annotations

import argparse
import sys
import xml.etree.ElementTree as ET

# Filename suffixes excluded from the enforced figure.
EXCLUDE = ("openapi_models_gen.py",)

DEFAULT_FLOOR = 75.0


def measure(path: str) -> tuple[int, int]:
    """Return (covered_lines, total_lines) over non-excluded sources."""
    try:
        root = ET.parse(path).getroot()
    except FileNotFoundError:
        print(f"::error::{path} not found — did the coverage step run?")
        raise SystemExit(1)
    except ET.ParseError as exc:
        print(f"::error::{path} is not valid XML: {exc}")
        raise SystemExit(1)

    hits = total = 0
    for cls in root.iter("class"):
        if cls.get("filename", "").endswith(EXCLUDE):
            continue
        for line in cls.iter("line"):
            total += 1
            if int(line.get("hits", "0")) > 0:
                hits += 1
    return hits, total


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("report", nargs="?", default="coverage.xml")
    parser.add_argument("--floor", type=float, default=DEFAULT_FLOOR)
    args = parser.parse_args()

    hits, total = measure(args.report)

    if total == 0:
        # An empty measurement means the report covered nothing we care about,
        # which is a broken run rather than perfect coverage. Fail rather than
        # report a vacuous 0% or 100%.
        print(f"::error::{args.report} contained no measurable non-generated lines")
        return 1

    pct = 100.0 * hits / total
    print(
        f"hand-written client coverage: {pct:.1f}% "
        f"({hits}/{total} lines, floor {args.floor}%)"
    )

    if pct < args.floor:
        print(
            f"::error::Python SDK client coverage {pct:.1f}% is below "
            f"the {args.floor}% floor"
        )
        return 1

    return 0


if __name__ == "__main__":
    sys.exit(main())
