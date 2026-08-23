"""Regression tests for the high-risk path classifier."""

from __future__ import annotations

import importlib.util
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


def main() -> None:
    review = load_review_module()
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
