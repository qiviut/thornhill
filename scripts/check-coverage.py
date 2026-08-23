#!/usr/bin/env python3
"""Enforce risk-focused Go coverage requirements from a coverprofile."""

from __future__ import annotations

import argparse
import json
import math
import sys
from collections import defaultdict
from pathlib import Path


def read_profile(path: Path) -> dict[str, tuple[int, int]]:
    totals: dict[str, list[int]] = defaultdict(lambda: [0, 0])
    for raw in path.read_text(encoding="utf-8").splitlines()[1:]:
        if not raw.strip():
            continue
        record, statements_text, count_text = raw.rsplit(" ", 2)
        file_path = record.rsplit(":", 1)[0]
        statements = int(statements_text)
        count = int(count_text)
        totals[file_path][0] += statements
        if count > 0:
            totals[file_path][1] += statements
    return {name: (values[0], values[1]) for name, values in totals.items()}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--profile", type=Path, required=True)
    parser.add_argument(
        "--require",
        action="append",
        default=[],
        metavar="PATH_PREFIX=MIN_PERCENT",
    )
    args = parser.parse_args()
    if not args.profile.is_file():
        print(f"coverage profile is missing: {args.profile}", file=sys.stderr)
        return 2

    files = read_profile(args.profile)
    results: list[dict[str, object]] = []
    failures: list[str] = []
    for requirement in args.require:
        try:
            prefix, minimum_text = requirement.rsplit("=", 1)
            minimum = float(minimum_text)
        except ValueError as exc:
            raise SystemExit(f"invalid --require value: {requirement}") from exc
        if not prefix or not math.isfinite(minimum) or not 0 <= minimum <= 100:
            raise SystemExit(f"invalid --require value: {requirement}")
        matched = {
            path: stats
            for path, stats in files.items()
            if path == prefix or path.startswith(prefix.rstrip("/") + "/")
        }
        statements = sum(total for total, _ in matched.values())
        covered = sum(done for _, done in matched.values())
        percent = 100.0 * covered / statements if statements else 0.0
        result = {
            "requirement": prefix,
            "minimum_percent": minimum,
            "covered_statements": covered,
            "total_statements": statements,
            "percent": round(percent, 2),
            "files": sorted(matched),
        }
        results.append(result)
        if not matched:
            failures.append(f"{prefix}: no matching files in {args.profile}")
        elif percent + 1e-9 < minimum:
            failures.append(f"{prefix}: {percent:.2f}% is below required {minimum:.2f}%")

    print(json.dumps({"profile": str(args.profile), "requirements": results}, sort_keys=True))
    if failures:
        for failure in failures:
            print(f"coverage check failed: {failure}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
