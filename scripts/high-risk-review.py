#!/usr/bin/env python3
"""Run deterministic evidence for high-risk changes in the current revision.

This is an automated review gate, not a claim of human or model approval.  It
classifies changed paths and executes the smallest relevant evidence matrix on
the exact checkout SHA supplied by CI.
"""

from __future__ import annotations

import argparse
import fnmatch
import json
import os
import subprocess
import sys
from pathlib import Path

GIT_COMMAND_TIMEOUT_SECONDS = 30
EVIDENCE_COMMAND_TIMEOUT_SECONDS = 300
EMPTY_TREE_SHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

RULES = {
    "lifecycle-ownership": (
        "internal/bridge/**",
        "internal/dispatch/**",
        "internal/events/**",
    ),
    "stateful-deployment": (
        "scripts/deploy-passed-main.sh",
        "scripts/install-*.sh",
        "scripts/rotate-postgres-role-password.sh",
        "scripts/local-recovery.sh",
        "scripts/capped-copy.py",
        "scripts/test-deployer-*.sh",
        "scripts/test-local-recovery.sh",
        "scripts/test-postgres-integration.sh",
        "internal/store/**",
        "docs/rollback-compatibility.json",
    ),
    "pipeline-container": (
        ".github/workflows/**",
        ".github/branch-protection.json",
        "Dockerfile",
        "Dockerfile.*",
        "docker-compose.yml",
        ".github/scanners/**",
        "scripts/check-ci-policy.sh",
        "scripts/check-coverage.py",
        "scripts/high-risk-review.py",
        "scripts/run-security-scans.sh",
        "scripts/run-canary.sh",
        "scripts/test-high-risk-review.py",
        "scripts/test-container-hardening.sh",
        "internal/cipolicy/**",
    ),
}


def changed_paths(base: str, head: str) -> list[str]:
    if base and set(base) != {"0"}:
        git_args = ["git", "diff", "--name-only", f"{base}...{head}"]
    else:
        # GitHub sends an all-zero `before` for an initial push.  There may be
        # no parent at all, and a multi-commit initial push must include every
        # path present in the submitted head rather than only its tip commit.
        git_args = ["git", "diff", "--name-only", EMPTY_TREE_SHA, head]
    result = subprocess.run(
        git_args,
        check=True,
        capture_output=True,
        text=True,
        timeout=GIT_COMMAND_TIMEOUT_SECONDS,
    )
    return [line for line in result.stdout.splitlines() if line]


def classify(paths: list[str]) -> dict[str, list[str]]:
    return {
        category: sorted(
            path
            for path in paths
            if any(fnmatch.fnmatch(path, pattern) for pattern in patterns)
        )
        for category, patterns in RULES.items()
    }


def run(label: str, command: list[str]) -> dict[str, object]:
    print(f"[risk-review] {label}: {' '.join(command)}")
    try:
        result = subprocess.run(
            command,
            check=False,
            text=True,
            timeout=EVIDENCE_COMMAND_TIMEOUT_SECONDS,
        )
    except subprocess.TimeoutExpired:
        print(
            f"[risk-review] {label} exceeded {EVIDENCE_COMMAND_TIMEOUT_SECONDS}s",
            file=sys.stderr,
        )
        raise SystemExit(124) from None
    if result.returncode != 0:
        raise SystemExit(result.returncode)
    return {"label": label, "command": command, "exit_code": result.returncode}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--base", default=os.environ.get("RISK_REVIEW_BASE_SHA", ""))
    parser.add_argument("--head", default=os.environ.get("RISK_REVIEW_HEAD_SHA", "HEAD"))
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()

    paths = changed_paths(args.base, args.head)
    categories = classify(paths)
    high_risk = {name: paths for name, paths in categories.items() if paths}
    evidence: list[dict[str, object]] = []

    if "lifecycle-ownership" in high_risk:
        evidence.append(run("lifecycle race tests", ["go", "test", "-race", "./internal/bridge", "./internal/dispatch"]))
    if "stateful-deployment" in high_risk:
        evidence.append(run("deployer policy", ["scripts/test-deployer-policy.sh"]))
        evidence.append(run("deployer transition recovery", ["scripts/test-deployer-transition-recovery.sh"]))
        # The full recovery harness needs the image built by the image lane.
        # Keep this source-side matrix independent from another job's Docker
        # daemon; CI runs the image-backed harness in that owning lane.
    if "pipeline-container" in high_risk:
        evidence.append(run("workflow lint", ["go", "tool", "actionlint", *(str(path) for path in sorted(Path(".github/workflows").glob("*.yml")))]))
        evidence.append(run("checked-in CI policy", ["scripts/check-ci-policy.sh"]))
        evidence.append(run("Dockerfile checks", ["docker", "buildx", "build", "--check", "."]))
        evidence.append(run("PostgreSQL Dockerfile checks", ["docker", "buildx", "build", "--check", "--file", "Dockerfile.postgres", "."]))

    report = {
        "version": 1,
        "base_sha": args.base if args.base and set(args.base) != {"0"} else None,
        "head_sha": subprocess.check_output(
            ["git", "rev-parse", args.head],
            text=True,
            timeout=GIT_COMMAND_TIMEOUT_SECONDS,
        ).strip(),
        "changed_paths": paths,
        "high_risk_categories": high_risk,
        "evidence": evidence,
        "disposition": "automated-evidence-complete",
    }
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(report, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
