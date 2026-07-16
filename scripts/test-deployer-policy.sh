#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
state_dir=$(mktemp -d)
cleanup() {
  rm -rf "${state_dir}"
}
trap cleanup EXIT

exec 8>"${state_dir}/deploy.lock"
flock -n 8

set +e
output=$(CHECK_ONLY=1 \
  STATE_DIR="${state_dir}" \
  PUBLIC_APP_URL=https://example.invalid/ \
  PUBLIC_STATUS_URL=https://example.invalid/api/status \
  "${root}/scripts/deploy-passed-main.sh" 2>&1)
status=$?
set -e

if (( status == 0 )); then
  printf 'CHECK_ONLY incorrectly succeeded without acquiring the deploy lock\n' >&2
  exit 1
fi
if [[ "${output}" != *"A Thornhill deployment is already running"* ]]; then
  printf 'Unexpected CHECK_ONLY lock-contention output: %s\n' "${output}" >&2
  exit 1
fi

printf 'Deployer policy passed: CHECK_ONLY fails closed on lock contention\n'
