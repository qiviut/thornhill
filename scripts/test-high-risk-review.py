"""Regression tests for the high-risk path classifier."""

from __future__ import annotations

import importlib.util
import os
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
MODULE_PATH = ROOT / "scripts" / "high-risk-review.py"


def load_review_module():
    spec = importlib.util.spec_from_file_location("high_risk_review", MODULE_PATH)
    if spec is None or spec.loader is None:
        raise AssertionError(f"could not load {MODULE_PATH}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def git(repo: Path, *args: str) -> str:
    result = subprocess.run(
        ["git", *args],
        cwd=repo,
        check=True,
        capture_output=True,
        text=True,
    )
    return result.stdout.strip()


def check_initial_push_ranges(review) -> None:
    with tempfile.TemporaryDirectory() as temporary:
        repo = Path(temporary)
        git(repo, "init", "-q")
        git(repo, "config", "user.email", "review@example.invalid")
        git(repo, "config", "user.name", "High-risk review")
        previous_cwd = Path.cwd()
        os.chdir(repo)
        try:
            (repo / "scripts").mkdir()
            (repo / "scripts" / "install-root.sh").write_text("#!/bin/sh\n", encoding="utf-8")
            git(repo, "add", "scripts/install-root.sh")
            git(repo, "commit", "-qm", "root")
            root_sha = git(repo, "rev-parse", "HEAD")
            root_paths = review.changed_paths("0" * 40, root_sha)
            require(root_paths == ["scripts/install-root.sh"], "root initial push range is incomplete")

            (repo / "scripts" / "install-later.sh").write_text("#!/bin/sh\n", encoding="utf-8")
            git(repo, "add", "scripts/install-later.sh")
            git(repo, "commit", "-qm", "later")
            head_sha = git(repo, "rev-parse", "HEAD")
            initial_paths = set(review.changed_paths("0" * 40, head_sha))
            require(
                initial_paths == {"scripts/install-root.sh", "scripts/install-later.sh"},
                "multi-commit initial push range omitted an earlier path",
            )
            normal_paths = review.changed_paths(root_sha, head_sha)
            require(normal_paths == ["scripts/install-later.sh"], "normal base/head range changed")
        finally:
            os.chdir(previous_cwd)


def main() -> None:
    review = load_review_module()
    check_initial_push_ranges(review)
    paths = [
        "scripts/install-ci-autodeploy.sh",
        ".github/scanners/compose.yml",
        "scripts/test-high-risk-review.py",
    ]
    categories = review.classify(paths)
    require(
        "scripts/install-ci-autodeploy.sh" in categories["stateful-deployment"],
        "deployment installer is not stateful high-risk",
    )
    require(
        ".github/scanners/compose.yml" in categories["pipeline-container"],
        "scanner configuration is not pipeline high-risk",
    )
    require(
        "scripts/test-high-risk-review.py" in categories["pipeline-container"],
        "classifier regression is not pipeline high-risk",
    )
    print("high-risk classification regression passed")


if __name__ == "__main__":
    main()
