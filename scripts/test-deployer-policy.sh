#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
controller=$(<"${root}/scripts/deploy-passed-main.sh")
credential_helper=$(<"${root}/scripts/rotate-postgres-role-password.sh")
recovery_helper=$(<"${root}/scripts/local-recovery.sh")
[[ "${controller}" == *"resolve_published_images"* && "${controller}" == *"validate_digest_ref"* && \
  "${controller}" == *"docker pull"* && "${controller}" == *"@sha256:"* && \
  "${controller}" == *"--no-build"* && "${controller}" != *"docker buildx build"* ]] || {
  printf 'Deployer must promote pulled CI images without rebuilding on the host\n' >&2
  exit 1
}
[[ "${controller}" == *"local-recovery.sh"* && "${controller}" == *"THORNHILL_RECOVERY_SOURCE_COMMIT"* && \
  "${recovery_helper}" == *"THORNHILL_RECOVERY_MAX_BYTES"* && "${recovery_helper}" == *"pg_restore --exit-on-error"* ]] || {
  printf 'Deployer must create bounded local recovery and verify disposable restore\n' >&2
  exit 1
}
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
[[ "${controller}" == *'--json databaseId,headSha,status,conclusion,event,url,createdAt'* && \
  "${controller}" == *'(.event == "push" or .event == "workflow_dispatch")'* && \
  "${controller}" != *'--event push'* ]] || {
  printf 'Deployer must accept only trusted push or protected-main workflow_dispatch CI runs\n' >&2
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

# Exercise the actual drain block with fake database operations. No host service
# is contacted; a count or persistence error must not reach the recovery stage.
python3 - "${root}/scripts/deploy-passed-main.sh" "${state_dir}" <<'PY'
import json
import os
from pathlib import Path
import subprocess
import sys

source = Path(sys.argv[1]).read_text()
state = Path(sys.argv[2])
def function(name):
    start = source.index(name + '() {\n')
    return source[start:source.index('\n}\n', start) + 3]

block = source.split('\nstage=drain\n', 1)[1].split('\nstage=recovery\n', 1)[0]
harness = '''set -euo pipefail
revision=0123456789abcdef0123456789abcdef01234567
draining=false
ensure_deployment_control() { :; }
set_dispatch_paused() {
  printf '%s\\n' "$1" >>"$CASE_DIR/operations"
  if [[ "$1" == TRUE ]]; then
    draining=true
    return 0
  fi
  [[ "$FAIL_UNPAUSE" != 1 ]] || return 1
  draining=false
}
active_jobs() { printf '%s\\n' "$COUNT"; }
deployed=false
database_password_rotated=false
transition_active=false
previous_compose=()
previous_compose_with_password=()
cleanup_worktrees() { :; }
''' + function('fsync_path') + function('write_deferral') + function('rollback') + function('on_exit') + '\ntrap on_exit EXIT\n' + block + '\nprintf "REACHED RECOVERY\\n"\n'

for count, quiet, fail_unpause, bad_state in [('3', '1', '0', False), ('3', '0', '0', False), ('invalid', '1', '0', False), ('3', '1', '1', False), ('3', '1', '0', True)]:
    case = state / ('case-' + str(len(list(state.glob('case-*')))))
    case.mkdir()
    target = case / 'missing' if bad_state else case
    env = dict(os.environ, STATE_DIR=str(target), CASE_DIR=str(case), COUNT=count, QUIET_DEPLOY_DEFERRALS=quiet, FAIL_UNPAUSE=fail_unpause)
    result = subprocess.run(['bash', '-c', harness], env=env, text=True, capture_output=True)
    assert 'REACHED RECOVERY' not in result.stdout, result
    operations = (case / 'operations').read_text().splitlines()
    if bad_state:
        assert result.returncode != 0 and operations == ['TRUE', 'FALSE'], (result, operations)
        continue
    if count == 'invalid':
        assert result.returncode != 0 and operations == ['TRUE', 'FALSE'], (result, operations)
    else:
        record = json.loads((case / 'deferred.json').read_text())
        assert record['source_commit'] == '0123456789abcdef0123456789abcdef01234567'
        assert record['reason'] == '3 active job(s) after entering drain mode'
        expected_operations = ['TRUE', 'FALSE'] if fail_unpause == '0' else ['TRUE', 'FALSE', 'FALSE']
        assert operations == expected_operations, operations
        assert (result.returncode != 0) == (fail_unpause == '1'), result
        assert ('Deployment deferred:' in result.stdout) == (quiet == '0'), result
print('Deployer drain deferral: quiet, visible, malformed count, rollback unpause and state-write failure passed')
PY
