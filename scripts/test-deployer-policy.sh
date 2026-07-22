#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
controller=$(<"${root}/scripts/deploy-passed-main.sh")
credential_helper=$(<"${root}/scripts/rotate-postgres-role-password.sh")
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
# The dollar reference is the literal container-shell command required below.
# shellcheck disable=SC2016
pg_ctl_pattern="pg_ctl -D \"\$PGDATA\" -m fast -w -t 30 stop"
[[ "${controller}" == *"stop_database_cleanly()"* && \
  "${controller}" == *"docker update --restart=no"* && \
  "${controller}" == *"docker exec --detach --user 70:70"* && \
  "${controller}" == *"${pg_ctl_pattern}"* && \
  "${controller}" == *".State.Restarting"* ]] || {
  printf 'Deployer must disable restart and checkpoint a legacy root-init PostgreSQL before recreation\n' >&2
  exit 1
}
containers_stopped_gate="[[ \"\${application_stopped}\" == true && \"\${database_stopped}\" == true ]]"
state_pattern='{{.State.Running}} {{.State.Restarting}} {{.State.ExitCode}}'
# Literal deployer-source assertions intentionally contain unevaluated variables.
# shellcheck disable=SC2016
[[ "${controller}" == *"verify_clean_stopped_container()"* && \
  "${controller}" == *"${state_pattern}"* && \
  "${controller}" == *"verify_application_stopped()"* && \
  "${controller}" == *"docker info >/dev/null 2>&1 || return 1"* && \
  "${controller}" == *"for _ in 1 2"* && \
  "${controller}" == *"stop_application_cleanly()"* && \
  "${controller}" == *"application_stopped=false"* && \
  "${controller}" == *"database_stopped=false"* && \
  "${controller}" == *"${containers_stopped_gate}"* && \
  "${controller}" == *"refusing rollback recreation after unverified database shutdown"* && \
  "${controller}" != *"stop_database_cleanly >/dev/null 2>&1 || true"* && \
  "${controller}" == *'write_transition "prepared"'* && \
  "${controller}" == *'write_transition "credential_rotation_pending"'* && \
  "${controller}" == *'write_transition "database_stopped"'* && \
  "${controller}" == *'write_transition "target_started"'* && \
  "${controller}" == *'write_transition "rollback_verified"'* && \
  "${controller}" == *'.version == 2'* && \
  "${controller}" == *'.credential_model | IN("previous", "journaled")'* && \
  "${controller}" == *'recover_pending_transition'* && \
  "${controller}" == *'database_password_rotated=true'* && \
  "${controller}" == *'fsync_path "${temporary}"'* && \
  "${controller}" == *'fsync_path "${STATE_DIR}"'* && \
  "${controller}" == *'write_transition "recovery_drained" || return 1'* && \
  "${controller}" == *'active=$(active_jobs) || return 1'* && \
  "${controller}" == *'verify_running_app_database_password'* && \
  "${controller}" == *'public_revision=$(status_revision "${PUBLIC_STATUS_URL}") || return 1'* && \
  "${controller}" == *'database credential convergence failed'* && \
  "${controller}" == *'rollback_compose=("${previous_compose_with_password[@]}")'* && \
  "${controller}" == *'rollback is unverified; deployment dispatch remains paused'* ]] || {
  printf 'Deploy, recovery, and rollback must journal transitions, verify stops, and preserve credential/pause state\n' >&2
  exit 1
}
[[ "${credential_helper}" == *'--entrypoint pg_isready'* && \
  "${credential_helper}" == *'if ! probe_client_path'* ]] || {
  printf 'Credential rotation must prove the disposable client path before ALTER ROLE\n' >&2
  exit 1
}
recovery_prefix=${controller%%"timeout 60s git fetch"*}
[[ "${recovery_prefix}" == *'recover_pending_transition'* ]] || {
  printf 'Credential transition recovery must run before network/CI selection\n' >&2
  exit 1
}

compose_password=$(printf '%064d' 0)
if THORNHILL_ENV_FILE=.env.example docker compose --env-file /dev/null config --quiet >/dev/null 2>&1; then
  printf 'Compose unexpectedly accepted a missing THORNHILL_DB_PASSWORD\n' >&2
  exit 1
fi
compose_model=$(THORNHILL_DB_PASSWORD="${compose_password}" THORNHILL_ENV_FILE=.env.example \
  docker compose --env-file /dev/null config --format json)
jq -e --arg password "${compose_password}" '
  .services.db.environment.POSTGRES_PASSWORD == $password and
  .services.db.command == ["postgres"] and
  (.services.db.entrypoint[2] | contains("POSTGRES_PASSWORD must be 64 lowercase hexadecimal characters")) and
  .services.app.environment.THORNHILL_DB_PASSWORD == $password and
  .services.app.environment.DATABASE_URL == ("postgres://thornhill:" + $password + "@db:5432/thornhill?sslmode=disable")
' <<<"${compose_model}" >/dev/null || {
  printf 'Compose did not bind one deployment credential to PostgreSQL and the application\n' >&2
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
