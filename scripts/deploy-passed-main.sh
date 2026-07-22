#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
REPOSITORY=${REPOSITORY:-qiviut/thornhill}
PROJECT_NAME=${PROJECT_NAME:-thornhill}
LOCAL_APP_URL=${LOCAL_APP_URL:-http://127.0.0.1:8787/}
LOCAL_STATUS_URL=${LOCAL_STATUS_URL:-http://127.0.0.1:8787/api/status}
PUBLIC_APP_URL=${PUBLIC_APP_URL:?set the externally reachable Thornhill URL}
PUBLIC_STATUS_URL=${PUBLIC_STATUS_URL:?set its /api/status URL}
STATE_DIR=${STATE_DIR:-${HOME}/.local/state/thornhill-ci-deploy}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-30}
export GH_HTTP_TIMEOUT=${GH_HTTP_TIMEOUT:-20}

install -d -m 0700 "${STATE_DIR}"
exec 9>"${STATE_DIR}/deploy.lock"
if ! flock -n 9; then
  echo "A Thornhill deployment is already running"
	if [[ "${CHECK_ONLY:-0}" == 1 ]]; then
		exit 1
	fi
  exit 0
fi

revision=""
run_id=""
run_url=""
image=""
db_image=""
previous_revision=""
previous_image=""
previous_db_image=""
tmp=""
source_dir=""
previous_source_dir=""
db_image_override=""
credential_override=""
database_password=""
database_password_sha256=""
database_password_rotated=false
transition_file="${STATE_DIR}/credential-transition.json"
transition_active=false
recovery_completed=false
deployed=false
draining=false
stage=select
compose=()
previous_compose=()
previous_compose_with_password=()

defer_or_fail_checkout() {
  local reason=$1 temporary current_head
  if [[ "${DEPLOY_POLL_MODE:-0}" != 1 ]]; then
    printf 'Refusing deployment: %s\n' "${reason}" >&2
    exit 1
  fi

  current_head=$(git rev-parse HEAD 2>/dev/null || true)
  temporary=$(mktemp "${STATE_DIR}/deferred.json.XXXXXX")
  jq -n --arg reason "${reason}" --arg source_commit "${revision}" \
    --arg checkout_commit "${current_head}" \
    '{reason:$reason,source_commit:$source_commit,checkout_commit:$checkout_commit,deferred_at:(now|todate)}' >"${temporary}"
  mv "${temporary}" "${STATE_DIR}/deferred.json"
  if [[ "${QUIET_DEPLOY_DEFERRALS:-0}" != 1 ]]; then
    printf 'Deployment deferred: %s\n' "${reason}"
  fi
  exit 0
}

write_receipt() {
  local deployed_revision=$1 deployed_image=$2 deployed_db_image=$3 deployed_run_id=$4 deployed_run_url=$5 temporary
  temporary=$(mktemp "${STATE_DIR}/deployed.json.XXXXXX")
  jq -n --arg source_commit "${deployed_revision}" --arg image "${deployed_image}" \
    --arg db_image "${deployed_db_image}" --argjson ci_run_id "${deployed_run_id}" \
    --arg ci_url "${deployed_run_url}" \
    '{source_commit:$source_commit,image:$image,db_image:$db_image,ci_run_id:$ci_run_id,ci_url:$ci_url}' >"${temporary}"
  mv "${temporary}" "${STATE_DIR}/deployed.json"
}

mark_failed() {
  local reason=$1 temporary
  [[ -n "${revision}" ]] || return 0
  temporary=$(mktemp "${STATE_DIR}/failed.json.XXXXXX") || return 1
  jq -n --arg source_commit "${revision}" --arg reason "${reason}" \
    --argjson ci_run_id "${run_id:-0}" --arg ci_url "${run_url}" \
    '{source_commit:$source_commit,ci_run_id:$ci_run_id,ci_url:$ci_url,reason:$reason,failed_at:(now|todate)}' >"${temporary}" || return 1
  chmod 0600 "${temporary}" || return 1
  fsync_path "${temporary}" || return 1
  mv "${temporary}" "${STATE_DIR}/failed.json" || return 1
  fsync_path "${STATE_DIR}" || return 1
}

fsync_path() {
  local path=$1
  python3 - "${path}" <<'PY'
import os
import sys

path = sys.argv[1]
flags = os.O_RDONLY
if os.path.isdir(path):
    flags |= getattr(os, "O_DIRECTORY", 0)
fd = os.open(path, flags)
try:
    os.fsync(fd)
finally:
    os.close(fd)
PY
}

write_transition() {
  local phase_value=$1 temporary credential_model=previous
  [[ "${revision}" =~ ^[0-9a-f]{40}$ && "${previous_revision}" =~ ^[0-9a-f]{40}$ ]] || return 1
  [[ -n "${image}" && -n "${db_image}" && -n "${previous_image}" && -n "${previous_db_image}" ]] || return 1
  [[ "${database_password_sha256}" =~ ^[0-9a-f]{64}$ ]] || return 1
  [[ "${database_password_rotated}" != true ]] || credential_model=journaled
  temporary=$(mktemp "${STATE_DIR}/credential-transition.json.XXXXXX") || return 1
  jq -n \
    --arg phase "${phase_value}" \
    --arg target_revision "${revision}" --arg target_image "${image}" --arg target_db_image "${db_image}" \
    --arg previous_revision "${previous_revision}" --arg previous_image "${previous_image}" --arg previous_db_image "${previous_db_image}" \
    --arg ci_run_id "${run_id}" --arg ci_url "${run_url}" \
    --arg password_sha256 "${database_password_sha256}" \
    --arg credential_model "${credential_model}" \
    '{version:2,phase:$phase,credential_model:$credential_model,target_revision:$target_revision,target_image:$target_image,target_db_image:$target_db_image,
      previous_revision:$previous_revision,previous_image:$previous_image,previous_db_image:$previous_db_image,
      ci_run_id:$ci_run_id,ci_url:$ci_url,password_sha256:$password_sha256,updated_at:(now|todate)}' >"${temporary}" || return 1
  chmod 0600 "${temporary}" || return 1
  fsync_path "${temporary}" || return 1
  mv "${temporary}" "${transition_file}" || return 1
  fsync_path "${STATE_DIR}" || return 1
  transition_active=true
}

clear_transition() {
  if [[ -e "${transition_file}" ]]; then
    rm -f "${transition_file}" || return 1
    fsync_path "${STATE_DIR}" || return 1
  fi
  transition_active=false
}

# The PostgreSQL variables intentionally expand inside the container shell.
# shellcheck disable=SC2016
db_sql() {
  timeout 15s docker exec -i "${PROJECT_NAME}-db-1" sh -c 'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atq'
}

load_database_password() {
  local owner mode mode_value
  local -a matches=()
  if [[ ! -f "${ROOT}/.env" ]]; then
    echo "Refusing deployment: missing ${ROOT}/.env" >&2
    return 1
  fi
  owner=$(stat -c %u "${ROOT}/.env")
  mode=$(stat -c %a "${ROOT}/.env")
  mode_value=$((8#${mode}))
  if [[ "${owner}" != "$(id -u)" ]] || (( (mode_value & 077) != 0 )); then
    echo "Refusing deployment: .env must be owned by the deployment user and inaccessible to group/other" >&2
    return 1
  fi
  mapfile -t matches < <(grep -E '^THORNHILL_DB_PASSWORD=' "${ROOT}/.env" || true)
  if (( ${#matches[@]} != 1 )); then
    echo "Refusing deployment: .env must contain exactly one THORNHILL_DB_PASSWORD" >&2
    return 1
  fi
  database_password=${matches[0]#THORNHILL_DB_PASSWORD=}
  if [[ ! "${database_password}" =~ ^[0-9a-f]{64}$ ]]; then
    echo "Refusing deployment: THORNHILL_DB_PASSWORD must be 64 lowercase hexadecimal characters" >&2
    return 1
  fi
  export THORNHILL_DB_PASSWORD="${database_password}"
  database_password_sha256=$(printf '%s' "${database_password}" | sha256sum)
  database_password_sha256=${database_password_sha256%% *}
}

verify_database_password() {
  printf '%s\n' "${database_password}" |
    "${ROOT}/scripts/rotate-postgres-role-password.sh" --verify-only "${PROJECT_NAME}-db-1"
}

verify_running_app_database_password() {
  local running_sha256
  running_sha256=$(
    docker inspect "${PROJECT_NAME}-app-1" --format '{{json .Config.Env}}' |
      jq -erj '[.[] | select(startswith("THORNHILL_DB_PASSWORD="))] |
        if length == 1 then .[0] | sub("^[^=]*="; "") else error("expected one database password") end' |
      sha256sum
  ) || return 1
  running_sha256=${running_sha256%% *}
  [[ "${running_sha256}" == "${database_password_sha256}" ]] || return 1
}

rotate_database_password() {
  [[ "${database_password}" =~ ^[0-9a-f]{64}$ ]] || return 1
  # Mark the migration before ALTER ROLE. If the helper fails after commit but
  # before verification, rollback retries the same idempotent rotation before
  # deciding which Compose credential to use.
  database_password_rotated=true
  printf '%s\n' "${database_password}" |
    "${source_dir}/scripts/rotate-postgres-role-password.sh" "${PROJECT_NAME}-db-1" || return 1
}

set_dispatch_paused() {
  local value=$1 result
  printf '%s\n' "BEGIN; SELECT pg_advisory_xact_lock(72623859790382856); UPDATE deployment_control SET dispatch_paused=${value}, updated_at=now() WHERE singleton=TRUE; COMMIT;" | db_sql >/dev/null || return 1
  result=$(printf '%s\n' "SELECT dispatch_paused FROM deployment_control WHERE singleton=TRUE;" | db_sql) || return 1
  if [[ "${value}" == TRUE ]]; then
    [[ "${result}" == t ]] || return 1
    draining=true
  else
    [[ "${result}" == f ]] || return 1
    draining=false
  fi
}

ensure_deployment_control() {
  db_sql >/dev/null <<'SQL'
CREATE TABLE IF NOT EXISTS deployment_control (
  singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
  dispatch_paused BOOLEAN NOT NULL DEFAULT FALSE,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO deployment_control (singleton, dispatch_paused)
VALUES (TRUE, FALSE) ON CONFLICT (singleton) DO NOTHING;
CREATE OR REPLACE FUNCTION thornhill_guard_job_dispatch() RETURNS trigger
LANGUAGE plpgsql AS $guard$
BEGIN
  IF TG_OP = 'UPDATE' AND NEW.status IS NOT DISTINCT FROM OLD.status THEN
    RETURN NEW;
  END IF;
  IF NEW.status NOT IN ('queued', 'running') THEN
    RETURN NEW;
  END IF;
  PERFORM pg_advisory_xact_lock(72623859790382856);
  IF (SELECT dispatch_paused FROM deployment_control WHERE singleton=TRUE) THEN
    RAISE EXCEPTION USING ERRCODE='55000', MESSAGE='job dispatch is temporarily paused for deployment';
  END IF;
  RETURN NEW;
END
$guard$;
DROP TRIGGER IF EXISTS thornhill_guard_job_insert_trigger ON jobs;
DROP TRIGGER IF EXISTS thornhill_guard_job_dispatch_trigger ON jobs;
DROP FUNCTION IF EXISTS thornhill_guard_job_insert();
CREATE TRIGGER thornhill_guard_job_dispatch_trigger
BEFORE INSERT OR UPDATE OF status ON jobs
FOR EACH ROW EXECUTE FUNCTION thornhill_guard_job_dispatch();
SQL
}

active_jobs() {
  printf "%s\n" "SELECT count(*) FROM jobs WHERE status IN ('queued','running','needs_input','needs_approval');" | db_sql
}

status_revision() {
  local url=$1
  curl --fail --silent --show-error --max-time 15 "${url}" |
    jq -er 'select(.status == "ok" and .versioned == true) | .source_commit'
}

verify_running_revision() {
  local expected=$1 image_id label binary
  image_id=$(docker inspect "${PROJECT_NAME}-app-1" --format '{{.Image}}') || return 1
  label=$(docker image inspect "${image_id}" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}') || return 1
  binary=$(timeout 10s docker exec "${PROJECT_NAME}-app-1" /app/thornhill --version) || return 1
  [[ "${label}" == "${expected}" && "${binary}" == "thornhill ${expected}" ]] || return 1
}

verify_running_db() {
  local expected_image=$1 actual_image health read_only cap_drop security_opt pids_limit runtime_uid
  actual_image=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{.Config.Image}}')
  health=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{.State.Health.Status}}')
  read_only=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{.HostConfig.ReadonlyRootfs}}')
  cap_drop=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{json .HostConfig.CapDrop}}')
  security_opt=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{json .HostConfig.SecurityOpt}}')
  pids_limit=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{.HostConfig.PidsLimit}}')
  runtime_uid=$(docker exec "${PROJECT_NAME}-db-1" stat -c %u /proc/1)
  [[ "${actual_image}" == "${expected_image}" && "${health}" == healthy && "${read_only}" == true && \
    "${cap_drop}" == *ALL* && "${security_opt}" == *no-new-privileges* && \
    "${pids_limit}" == 256 && "${runtime_uid}" == 70 ]]
}

verify_live_revision() {
  local expected=$1 local_revision public_revision
  curl --fail --silent --show-error --output /dev/null --max-time 15 "${LOCAL_APP_URL}" || return 1
  curl --fail --silent --show-error --output /dev/null --max-time 15 "${PUBLIC_APP_URL}" || return 1
  local_revision=$(status_revision "${LOCAL_STATUS_URL}") || return 1
  public_revision=$(status_revision "${PUBLIC_STATUS_URL}") || return 1
  [[ "${local_revision}" == "${expected}" && "${public_revision}" == "${expected}" ]] || return 1
  verify_running_revision "${expected}" || return 1
}

verify_clean_stopped_container() {
  local container=$1 state
  # Sample twice so an automatic restart cannot masquerade as a stable clean
  # stop between docker wait and recreation.
  for _ in 1 2; do
    if ! state=$(docker inspect "${container}" --format '{{.State.Running}} {{.State.Restarting}} {{.State.ExitCode}}'); then
      return 1
    fi
    [[ "${state}" == "false false 0" ]] || return 1
    sleep 0.2
  done
}

verify_application_stopped() {
  local container=$1 state
  # Application rollback is idempotent across interrupted force-recreate. A
  # daemon-confirmed missing container is already stopped, and a stable exited
  # candidate is safe regardless of its prior exit code. PostgreSQL continues
  # to use the stricter exit-zero checkpoint verifier above.
  for _ in 1 2; do
    if state=$(docker inspect "${container}" --format '{{.State.Running}} {{.State.Restarting}}'); then
      [[ "${state}" == "false false" ]] || return 1
    else
      docker info >/dev/null 2>&1 || return 1
      if docker container inspect "${container}" >/dev/null 2>&1; then
        return 1
      fi
    fi
    sleep 0.2
  done
}

stop_application_cleanly() {
  local container="${PROJECT_NAME}-app-1" running
  if ! running=$(docker inspect "${container}" --format '{{.State.Running}}'); then
    docker info >/dev/null 2>&1 || return 1
    if docker container inspect "${container}" >/dev/null 2>&1; then
      return 1
    fi
    return 0
  fi
  if [[ "${running}" == true ]] && ! docker stop --timeout 30 "${container}" >/dev/null; then
    return 1
  fi
  verify_application_stopped "${container}"
}

stop_database_cleanly() {
  local container="${PROJECT_NAME}-db-1" pid1_uid exit_code running
  if ! running=$(docker inspect "${container}" --format '{{.State.Running}}'); then
    return 1
  fi
  if [[ "${running}" != true ]]; then
    verify_clean_stopped_container "${container}"
    return
  fi
  if ! pid1_uid=$(docker exec "${container}" stat -c %u /proc/1); then
    return 1
  fi
  if [[ "${pid1_uid}" == 70 ]]; then
    if ! docker stop --timeout 30 "${container}" >/dev/null; then
      return 1
    fi
    verify_clean_stopped_container "${container}"
    return
  fi

  # One-time upgrade path from the former root-owned tini shim. With all
  # capabilities dropped, that shim cannot signal UID-70 PostgreSQL. Ask
  # PostgreSQL itself to checkpoint and stop as its owning user instead.
  echo "Stopping legacy PostgreSQL PID 1 (uid=${pid1_uid}) through pg_ctl"
  # The persisted legacy service used unless-stopped. Disable automatic restart
  # before pg_ctl so docker wait cannot observe a transient clean exit followed
  # by the same unsafe root-init container restarting behind the deployer.
  if ! docker update --restart=no "${container}" >/dev/null; then
    return 1
  fi
  if ! docker exec --detach --user 70:70 "${container}" \
    sh -ec 'exec pg_ctl -D "$PGDATA" -m fast -w -t 30 stop'; then
    return 1
  fi
  if ! exit_code=$(timeout 35s docker wait "${container}"); then
    return 1
  fi
  [[ "${exit_code}" == 0 ]] && verify_clean_stopped_container "${container}"
}

prepare_compose_models() {
  # Environment material is attached only after any untrusted build context has
  # been consumed. Recovery does not rebuild, but preserves the same boundary.
  [[ -e "${source_dir}/.env" ]] || ln -s "${ROOT}/.env" "${source_dir}/.env"
  [[ -e "${previous_source_dir}/.env" ]] || ln -s "${ROOT}/.env" "${previous_source_dir}/.env"
  db_image_override="${tmp}/db-image.override.yml"
  printf 'services:\n  db:\n    image: %s\n' \
    "\${THORNHILL_POSTGRES_IMAGE:?set THORNHILL_POSTGRES_IMAGE}" >"${db_image_override}"
  credential_override="${tmp}/database-credential.override.yml"
  # Compose, not this shell, expands the credential in the generated override.
  # shellcheck disable=SC2016
  printf '%s\n' \
    'services:' \
    '  db:' \
    '    environment:' \
    '      POSTGRES_PASSWORD: ${THORNHILL_DB_PASSWORD:?set THORNHILL_DB_PASSWORD}' \
    '  app:' \
    '    environment:' \
    '      THORNHILL_DB_PASSWORD: ${THORNHILL_DB_PASSWORD:?set THORNHILL_DB_PASSWORD}' \
    '      DATABASE_URL: postgres://thornhill:${THORNHILL_DB_PASSWORD:?set THORNHILL_DB_PASSWORD}@127.0.0.1:55432/thornhill?sslmode=disable' \
    >"${credential_override}"
  compose=(docker compose --project-name "${PROJECT_NAME}" -f "${source_dir}/docker-compose.yml" -f "${source_dir}/docker-compose.host.yml" -f "${db_image_override}" -f "${credential_override}")
  previous_compose=(docker compose --project-name "${PROJECT_NAME}" -f "${previous_source_dir}/docker-compose.yml" -f "${previous_source_dir}/docker-compose.host.yml" -f "${db_image_override}")
  previous_compose_with_password=(docker compose --project-name "${PROJECT_NAME}" -f "${previous_source_dir}/docker-compose.yml" -f "${previous_source_dir}/docker-compose.host.yml" -f "${db_image_override}" -f "${credential_override}")
}

ensure_database_for_credential_convergence() {
  local deadline db_running
  if ! docker inspect "${PROJECT_NAME}-db-1" >/dev/null 2>&1; then
    THORNHILL_APP_IMAGE="${image}" THORNHILL_POSTGRES_IMAGE="${db_image}" \
      "${compose[@]}" up -d --no-build db || return 1
  else
    db_running=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{.State.Running}}') || return 1
    if [[ "${db_running}" != true ]]; then
      docker start "${PROJECT_NAME}-db-1" >/dev/null || return 1
    fi
  fi
  deadline=$((SECONDS + TIMEOUT_SECONDS))
  until printf 'SELECT 1;\n' | db_sql >/dev/null 2>&1; do
    if (( SECONDS >= deadline )); then
      echo "PostgreSQL did not become available for credential convergence" >&2
      return 1
    fi
    sleep 1
  done
}

cleanup_worktrees() {
  local directory
  for directory in "${source_dir}" "${previous_source_dir}"; do
    [[ -n "${directory}" ]] || continue
    if git worktree list --porcelain | grep -Fqx "worktree ${directory}"; then
      git worktree remove --force "${directory}" >/dev/null 2>&1 || true
    fi
  done
  [[ -z "${tmp}" ]] || rm -rf "${tmp}"
}

recover_pending_transition() {
  [[ -f "${transition_file}" ]] || return 0
  local journal_phase journal_credential_model deadline active

  if ! jq -e '
    .version == 2 and
    (.credential_model | IN("previous", "journaled")) and
    (.phase | IN(
      "prepared", "application_stop_pending", "application_stopped",
      "credential_rotation_pending", "password_rotated", "database_stop_pending",
      "database_stopped", "target_start_pending", "target_started",
      "recover_stop_application", "recover_drain_pending", "recovery_drained",
      "recover_credential", "rollback_verified"
    )) and
    (.target_revision | test("^[0-9a-f]{40}$")) and
    (.previous_revision | test("^[0-9a-f]{40}$")) and
    (.target_image | type == "string" and length > 0) and
    (.target_db_image | type == "string" and length > 0) and
    (.previous_image | type == "string" and length > 0) and
    (.previous_db_image | type == "string" and length > 0) and
    (.ci_run_id | test("^[0-9]+$")) and
    (.password_sha256 | test("^[0-9a-f]{64}$"))
  ' "${transition_file}" >/dev/null; then
    echo "CRITICAL: malformed credential transition journal; dispatch state requires manual recovery" >&2
    return 1
  fi

  journal_phase=$(jq -r .phase "${transition_file}")
  journal_credential_model=$(jq -r .credential_model "${transition_file}")
  revision=$(jq -r .target_revision "${transition_file}")
  image=$(jq -r .target_image "${transition_file}")
  db_image=$(jq -r .target_db_image "${transition_file}")
  previous_revision=$(jq -r .previous_revision "${transition_file}")
  previous_image=$(jq -r .previous_image "${transition_file}")
  previous_db_image=$(jq -r .previous_db_image "${transition_file}")
  run_id=$(jq -r .ci_run_id "${transition_file}")
  run_url=$(jq -r .ci_url "${transition_file}")
  if [[ $(jq -r .password_sha256 "${transition_file}") != "${database_password_sha256}" ]]; then
    echo "CRITICAL: .env changed during credential transition; restore the journaled password before recovery" >&2
    return 1
  fi
  if ! git merge-base --is-ancestor "${revision}" HEAD; then
    echo "CRITICAL: local recovery controller predates the journaled target" >&2
    return 1
  fi
  local controlled_path
  for controlled_path in scripts/deploy-passed-main.sh scripts/rotate-postgres-role-password.sh; do
    if ! git diff --quiet -- "${controlled_path}" || ! git diff --cached --quiet -- "${controlled_path}" || \
      [[ $(git hash-object "${controlled_path}") != $(git rev-parse "HEAD:${controlled_path}") ]]; then
      echo "CRITICAL: modified deployment control ${controlled_path}; refusing journal recovery" >&2
      return 1
    fi
  done
  if ! git cat-file -e "${revision}^{commit}" || ! git cat-file -e "${previous_revision}^{commit}" || \
    ! docker image inspect "${image}" "${db_image}" "${previous_image}" "${previous_db_image}" >/dev/null 2>&1; then
    echo "CRITICAL: credential transition recovery artifacts are missing" >&2
    return 1
  fi

  tmp=$(mktemp -d)
  source_dir="${tmp}/source"
  previous_source_dir="${tmp}/previous"
  git worktree add --quiet --detach "${source_dir}" "${revision}"
  git worktree add --quiet --detach "${previous_source_dir}" "${previous_revision}"
  prepare_compose_models
  transition_active=true

  if [[ "${journal_phase}" == rollback_verified ]]; then
    database_password_rotated=false
    [[ "${journal_credential_model}" != journaled ]] || database_password_rotated=true
    if ! verify_live_revision "${previous_revision}" || ! verify_running_db "${previous_db_image}"; then
      echo "CRITICAL: journaled rollback completion no longer matches the previous runtime" >&2
      return 1
    fi
    if [[ "${database_password_rotated}" == true ]] && \
      { ! verify_database_password || ! verify_running_app_database_password; }; then
      echo "CRITICAL: journaled rollback credential model no longer matches the previous runtime" >&2
      return 1
    fi
    ensure_deployment_control || return 1
    set_dispatch_paused TRUE || return 1
    mark_failed "verified rollback recovered after interrupted failure quarantine" || return 1
    set_dispatch_paused FALSE || return 1
    clear_transition || return 1
    recovery_completed=true
    echo "Recovered verified rollback to ${previous_revision}"
    return 0
  fi

  # A restart cannot prove whether ALTER ROLE committed before power was lost.
  # Conservatively converge the journaled credential before any rollback.
  database_password_rotated=true

  if verify_live_revision "${revision}" >/dev/null 2>&1 && \
    verify_running_db "${db_image}" >/dev/null 2>&1 && verify_database_password && \
    verify_running_app_database_password; then
    ensure_deployment_control || return 1
    set_dispatch_paused TRUE || return 1
    write_receipt "${revision}" "${image}" "${db_image}" "${run_id}" "${run_url}" || return 1
    clear_transition || return 1
    set_dispatch_paused FALSE || return 1
    recovery_completed=true
    echo "Recovered completed credential transition to ${revision}"
    return 0
  fi

  echo "Recovering interrupted credential transition (${journal_phase}) to ${revision}" >&2
  stage=recover_drain
  write_transition "recover_drain_pending" || return 1
  ensure_database_for_credential_convergence || return 1
  ensure_deployment_control || return 1
  set_dispatch_paused TRUE || return 1
  deadline=$((SECONDS + TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    active=$(active_jobs) || return 1
    [[ "${active}" == 0 ]] && break
    sleep 1
  done
  active=$(active_jobs) || return 1
  if [[ "${active}" != 0 ]]; then
    echo "Credential recovery deferred: ${active} active job(s) did not drain" >&2
    return 1
  fi

  deployed=true
  write_transition "recovery_drained" || return 1
  stage=recover_stop_application
  stop_application_cleanly || return 1

  stage=recover_credential
  write_transition "recover_credential" || return 1
  rotate_database_password || return 1
  write_transition "password_rotated" || return 1

  stage=recover_stop_database
  write_transition "database_stop_pending" || return 1
  stop_database_cleanly || return 1
  write_transition "database_stopped" || return 1

  stage=recover_start_target
  write_transition "target_start_pending" || return 1
  THORNHILL_APP_IMAGE="${image}" THORNHILL_POSTGRES_IMAGE="${db_image}" \
    "${compose[@]}" up -d --no-build --force-recreate db app || return 1
  write_transition "target_started" || return 1

  stage=recover_verify
  deadline=$((SECONDS + TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    if verify_live_revision "${revision}" >/dev/null 2>&1 && \
      verify_running_db "${db_image}" >/dev/null 2>&1 && verify_database_password && \
      verify_running_app_database_password; then
      break
    fi
    sleep 1
  done
  if ! verify_live_revision "${revision}" || ! verify_running_db "${db_image}" || \
    ! verify_database_password || ! verify_running_app_database_password; then
    echo "Credential transition recovery could not verify target ${revision}" >&2
    return 1
  fi
  write_receipt "${revision}" "${image}" "${db_image}" "${run_id}" "${run_url}" || return 1
  clear_transition || return 1
  deployed=false
  set_dispatch_paused FALSE || return 1
  recovery_completed=true
  echo "Recovered interrupted credential transition to ${revision}"
}

rollback() {
  local rollback_ok=false application_stopped=false database_stopped=false deadline
  local -a rollback_compose=()
  if [[ "${database_password_rotated}" == true ]]; then
    rollback_compose=("${previous_compose_with_password[@]}")
  else
    rollback_compose=("${previous_compose[@]}")
  fi
  if [[ "${deployed}" == true && -n "${previous_image}" && -n "${previous_db_image}" && ${#rollback_compose[@]} -gt 0 ]]; then
    echo "${stage} failed; rolling back to ${previous_revision} (${previous_image}, ${previous_db_image})" >&2
    # Stop the application before PostgreSQL so the database can checkpoint and
    # exit cleanly even when the failed stack still owns pooled connections.
    if stop_application_cleanly >/dev/null 2>&1; then
      application_stopped=true
      if [[ "${database_password_rotated}" == true ]] && \
        { ! ensure_database_for_credential_convergence >/dev/null 2>&1 || \
          ! printf '%s\n' "${database_password}" | \
            "${source_dir}/scripts/rotate-postgres-role-password.sh" "${PROJECT_NAME}-db-1" >/dev/null 2>&1; }; then
        echo "CRITICAL: refusing rollback recreation because database credential convergence failed" >&2
      elif stop_database_cleanly >/dev/null 2>&1; then
        database_stopped=true
      else
        echo "CRITICAL: refusing rollback recreation after unverified database shutdown" >&2
      fi
    else
      echo "CRITICAL: refusing rollback recreation because the application did not stop cleanly" >&2
    fi
    if [[ "${application_stopped}" == true && "${database_stopped}" == true ]] && \
      THORNHILL_APP_IMAGE="${previous_image}" THORNHILL_POSTGRES_IMAGE="${previous_db_image}" \
      "${rollback_compose[@]}" up -d --no-build --force-recreate db app >/dev/null; then
      deadline=$((SECONDS + TIMEOUT_SECONDS))
      while (( SECONDS < deadline )); do
        if verify_live_revision "${previous_revision}" >/dev/null 2>&1 && \
          verify_running_db "${previous_db_image}" >/dev/null 2>&1 && \
          { [[ "${database_password_rotated}" != true ]] || \
            { verify_database_password && verify_running_app_database_password; }; }; then
          rollback_ok=true
          break
        fi
        sleep 1
      done
    fi
  fi
  # A verified rollback is journaled as a durable completion marker before
  # dispatch is unpaused. If power fails in that window, startup verifies the
  # previous runtime and consumes this marker without retrying the failed target.
  if [[ "${rollback_ok}" == true && "${transition_active}" == true ]]; then
    write_transition "rollback_verified" || return 1
  fi
  if [[ "${deployed}" == true ]]; then
    mark_failed "${stage} failed; rollback_ok=${rollback_ok}" || return 1
  fi
  if [[ "${draining}" == true ]]; then
    if [[ "${deployed}" == true && "${rollback_ok}" != true ]]; then
      echo "CRITICAL: rollback is unverified; deployment dispatch remains paused for recovery" >&2
    elif ! set_dispatch_paused FALSE; then
      echo "CRITICAL: failed to leave deployment drain mode" >&2
      return 1
    fi
  fi
  # Clearing after the unpause is safe because rollback_verified is itself a
  # restart-safe marker; a surviving marker is simply re-verified and consumed.
  if [[ "${rollback_ok}" == true && "${transition_active}" == true ]]; then
    clear_transition || return 1
  fi
  [[ "${rollback_ok}" == true || "${deployed}" == false ]]
}

on_exit() {
  local rc=$?
  trap - EXIT INT TERM
  if (( rc != 0 )); then
    rollback || rc=1
  elif [[ "${draining}" == true ]]; then
    set_dispatch_paused FALSE || rc=1
  fi
  cleanup_worktrees
  exit "${rc}"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cd "${ROOT}"
if [[ -f "${transition_file}" ]]; then
  load_database_password
  recover_pending_transition
  if [[ "${recovery_completed}" == true ]]; then
    exit 0
  fi
fi
timeout 60s git fetch --quiet --prune origin main
remote_main=$(git rev-parse origin/main)
runs=$(gh run list --repo "${REPOSITORY}" --workflow CI --branch main --event push --limit 50 \
  --json databaseId,headSha,status,conclusion,url,createdAt)
passed=$(jq -c '[.[] | select(.status == "completed" and .conclusion == "success")][0] // empty' <<<"${runs}")
if [[ -z "${passed}" ]]; then
  echo "No successful main push CI run found" >&2
  exit 1
fi
revision=$(jq -r .headSha <<<"${passed}")
run_id=$(jq -r .databaseId <<<"${passed}")
run_url=$(jq -r .url <<<"${passed}")
if [[ ! "${revision}" =~ ^[0-9a-f]{40}$ ]]; then
  echo "Refusing invalid CI revision ${revision@Q}" >&2
  exit 1
fi
if ! git merge-base --is-ancestor "${revision}" "${remote_main}"; then
  echo "Latest passing CI revision ${revision} is not an ancestor of origin/main ${remote_main}" >&2
  exit 1
fi
for controlled_path in scripts/deploy-passed-main.sh scripts/rotate-postgres-role-password.sh; do
  if ! git diff --quiet -- "${controlled_path}" || ! git diff --cached --quiet -- "${controlled_path}"; then
    defer_or_fail_checkout "modified deployment control ${controlled_path}"
  fi
  if [[ $(git hash-object "${controlled_path}") != $(git rev-parse "${revision}:${controlled_path}") ]]; then
    defer_or_fail_checkout "${controlled_path} does not match passing revision ${revision}; update the local checkout"
  fi
done
rollback_mode=$(git show "${revision}:docs/rollback-compatibility.json" | jq -er '.mode')
if [[ "${rollback_mode}" != backward-compatible-additive ]]; then
  echo "Refusing automatic deployment: schema policy mode is ${rollback_mode@Q}, not backward-compatible-additive" >&2
  exit 1
fi
load_database_password

previous_revision=$(status_revision "${LOCAL_STATUS_URL}" 2>/dev/null || true)
if [[ "${previous_revision}" == "${revision}" ]]; then
  current_db_image=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{.Config.Image}}')
  if ! verify_database_password; then
    echo "Configured THORNHILL_DB_PASSWORD does not authenticate; refusing to record an already-current deployment" >&2
    exit 1
  fi
  if ! verify_live_revision "${revision}" || ! verify_running_db "${current_db_image}" || \
    ! verify_running_app_database_password; then
    echo "Revision ${revision} is local but failed full app/PostgreSQL runtime verification" >&2
    exit 1
  fi
  current_image=$(docker inspect "${PROJECT_NAME}-app-1" --format '{{.Config.Image}}')
  write_receipt "${revision}" "${current_image}" "${current_db_image}" "${run_id}" "${run_url}"
  rm -f "${STATE_DIR}/failed.json" "${STATE_DIR}/deferred.json"
  set_dispatch_paused FALSE
  if [[ "${QUIET_ALREADY_CURRENT:-0}" != 1 ]]; then
    echo "Already running latest passing CI revision ${revision} (run ${run_id})"
  fi
  exit 0
fi
if [[ "${CHECK_ONLY:-0}" == 1 ]]; then
  echo "Revision mismatch: live=${previous_revision:-unversioned} latest_passing_ci=${revision} run=${run_id}" >&2
  exit 1
fi
if [[ -f "${STATE_DIR}/failed.json" && "${RETRY_FAILED:-0}" != 1 ]]; then
  failed_revision=$(jq -r '.source_commit // empty' "${STATE_DIR}/failed.json")
  if [[ "${failed_revision}" == "${revision}" ]]; then
    echo "Revision ${revision} is quarantined after a failed deployment; set RETRY_FAILED=1 to retry" >&2
    exit 0
  fi
fi
if [[ ! "${previous_revision}" =~ ^[0-9a-f]{40}$ ]] || ! verify_live_revision "${previous_revision}"; then
  echo "Refusing deployment without a healthy, versioned rollback target" >&2
  exit 1
fi
previous_image=$(docker inspect "${PROJECT_NAME}-app-1" --format '{{.Config.Image}}')
previous_db_image=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{.Config.Image}}')
if [[ -z "${previous_image}" || -z "${previous_db_image}" ]] || \
  ! docker image inspect "${previous_image}" "${previous_db_image}" >/dev/null 2>&1; then
  echo "Refusing deployment without existing app and PostgreSQL images for rollback" >&2
  exit 1
fi

tmp=$(mktemp -d)
source_dir="${tmp}/source"
previous_source_dir="${tmp}/previous"
git worktree add --quiet --detach "${source_dir}" "${revision}"
git worktree add --quiet --detach "${previous_source_dir}" "${previous_revision}"
image="thornhill-app:${revision}"
db_image="thornhill-postgres:${revision}"
stage=build
docker buildx version >/dev/null || {
  echo "Docker Buildx is required for reproducible BuildKit builds; install the Docker buildx CLI plugin" >&2
  exit 1
}
docker buildx build --pull --load \
  --build-arg "THORNHILL_REVISION=${revision}" \
  --label "org.opencontainers.image.source=https://github.com/${REPOSITORY}" \
  --tag "${image}" "${source_dir}"
docker buildx build --pull --no-cache --load \
  --file "${source_dir}/Dockerfile.postgres" \
  --tag "${db_image}" "${source_dir}"
label=$(docker image inspect "${image}" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}')
if [[ "${label}" != "${revision}" || $(docker run --rm "${image}" --version) != "thornhill ${revision}" ]]; then
  echo "Built image or binary does not report ${revision}" >&2
  exit 1
fi

# Link the host environment only after the untrusted build context has been consumed.
prepare_compose_models

stage=drain
ensure_deployment_control
set_dispatch_paused TRUE
active=$(active_jobs)
if [[ "${active}" != 0 ]]; then
  echo "Deferring ${revision}: ${active} active job(s) after entering drain mode"
  set_dispatch_paused FALSE
  exit 0
fi

write_transition "prepared"
deployed=true
stage=stop
# Compose replacement can otherwise stop the dependency before its client. Stop
# the app first, rotate the role while PostgreSQL's local socket is available,
# then give PostgreSQL its full clean-shutdown grace period.
write_transition "application_stop_pending"
stop_application_cleanly
write_transition "application_stopped"
stage=credential
write_transition "credential_rotation_pending"
rotate_database_password
write_transition "password_rotated"
stage=stop
write_transition "database_stop_pending"
stop_database_cleanly
write_transition "database_stopped"
stage=deploy
write_transition "target_start_pending"
THORNHILL_APP_IMAGE="${image}" THORNHILL_POSTGRES_IMAGE="${db_image}" \
  "${compose[@]}" up -d --no-build --force-recreate db app
write_transition "target_started"
stage=verify
deadline=$((SECONDS + TIMEOUT_SECONDS))
while (( SECONDS < deadline )); do
  if verify_live_revision "${revision}" >/dev/null 2>&1 && \
    verify_running_db "${db_image}" >/dev/null 2>&1 && verify_database_password && \
    verify_running_app_database_password; then
    break
  fi
  sleep 1
done
if ! verify_live_revision "${revision}" || ! verify_running_db "${db_image}" || \
  ! verify_database_password || ! verify_running_app_database_password; then
  echo "Local/Tailnet/app/PostgreSQL runtime verification failed for ${revision}" >&2
  exit 1
fi
rm -f "${STATE_DIR}/failed.json" "${STATE_DIR}/deferred.json"
write_receipt "${revision}" "${image}" "${db_image}" "${run_id}" "${run_url}"
clear_transition
deployed=false
set_dispatch_paused FALSE
echo "Deployed ${revision} from successful CI run ${run_id}: ${run_url}"
