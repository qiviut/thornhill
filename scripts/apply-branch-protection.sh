#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "${root}"

defer_private=false
if [[ "${1:-}" == --defer-private ]]; then
  defer_private=true
  shift
fi
if [[ $# -ne 0 ]]; then
  echo "usage: $0 [--defer-private]" >&2
  exit 2
fi

repo=${REPOSITORY:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}
visibility=$(gh repo view "${repo}" --json visibility --jq .visibility)
branch=$(gh repo view "${repo}" --json defaultBranchRef --jq .defaultBranchRef.name)

if [[ "${branch}" != main ]]; then
  echo "refusing to protect unexpected default branch ${branch@Q}" >&2
  exit 1
fi
scripts/check-ci-policy.sh

if [[ "${visibility}" != PUBLIC && "${ALLOW_PRIVATE:-0}" != 1 ]]; then
  if [[ "${defer_private}" == true ]]; then
    echo "${repo} is ${visibility}; explicit private-repository deferral recorded."
    exit 0
  fi
  echo "${repo} is ${visibility}; refusing to report success without --defer-private." >&2
  exit 1
fi

gh api --method PUT "repos/${repo}/branches/main/protection" \
  --input .github/branch-protection.json >/dev/null

actual=$(mktemp)
trap 'rm -f "${actual}"' EXIT
gh api "repos/${repo}/branches/main/protection" >"${actual}"

jq -e '
  .required_status_checks.strict == true and
  ([.required_status_checks.checks[].context] == ["Go, web, and image build"]) and
  .enforce_admins.enabled == true and
  .required_pull_request_reviews.dismiss_stale_reviews == true and
  .required_pull_request_reviews.required_approving_review_count == 0 and
  .required_linear_history.enabled == true and
  .allow_force_pushes.enabled == false and
  .allow_deletions.enabled == false and
  .required_conversation_resolution.enabled == true
' "${actual}" >/dev/null

jq '{required_checks: [.required_status_checks.checks[].context], strict: .required_status_checks.strict, enforce_admins: .enforce_admins.enabled, required_reviews: .required_pull_request_reviews.required_approving_review_count, linear_history: .required_linear_history.enabled, force_pushes: .allow_force_pushes.enabled, deletions: .allow_deletions.enabled, conversation_resolution: .required_conversation_resolution.enabled}' "${actual}"
