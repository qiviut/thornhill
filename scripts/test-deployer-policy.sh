#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
controller=$(<"${root}/scripts/deploy-passed-main.sh")
[[ "${controller}" == *"trap on_exit EXIT"* && "${controller}" == *"trap 'exit 130' INT"* && \
  "${controller}" == *"trap 'exit 143' TERM"* && "${controller}" != *"trap on_exit EXIT INT TERM"* ]] || {
  printf 'Deployer signal traps must route INT/TERM through a nonzero EXIT rollback\n' >&2
  exit 1
}
direct_stop_pattern="docker stop --timeout 30 \"\${container}\""
compose_stop_pattern="\"\${compose[@]}\" stop"
[[ "${controller}" == *"${direct_stop_pattern}"* && \
  "${controller}" != *"${compose_stop_pattern}"* ]] || {
  printf 'Deployer must stop identified containers without Compose image interpolation\n' >&2
  exit 1
}
pg_ctl_pattern="pg_ctl -D \"\$PGDATA\" -m fast -w -t 30 stop"
[[ "${controller}" == *"stop_database_cleanly()"* && \
  "${controller}" == *"docker update --restart=no"* && \
  "${controller}" == *"docker exec --detach --user 70:70"* && \
  "${controller}" == *"${pg_ctl_pattern}"* && \
  "${controller}" == *".State.Restarting"* && \
  "${controller}" == *$'stop_database_cleanly\nstage=deploy'* ]] || {
  printf 'Deployer must disable restart and checkpoint a legacy root-init PostgreSQL before recreation\n' >&2
  exit 1
}
containers_stopped_gate="[[ \"\${application_stopped}\" == true && \"\${database_stopped}\" == true ]]"
state_pattern='{{.State.Running}} {{.State.Restarting}} {{.State.ExitCode}}'
[[ "${controller}" == *"verify_clean_stopped_container()"* && \
  "${controller}" == *"${state_pattern}"* && \
  "${controller}" == *"for _ in 1 2"* && \
  "${controller}" == *"stop_application_cleanly()"* && \
  "${controller}" == *"application_stopped=false"* && \
  "${controller}" == *"database_stopped=false"* && \
  "${controller}" == *"${containers_stopped_gate}"* && \
  "${controller}" == *"refusing rollback recreation after unverified database shutdown"* && \
  "${controller}" != *"stop_database_cleanly >/dev/null 2>&1 || true"* && \
  "${controller}" == *$'stop_application_cleanly\nstop_database_cleanly\nstage=deploy'* ]] || {
  printf 'Deploy and rollback must verify stable exit-zero app/database stops before recreation\n' >&2
  exit 1
}
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
