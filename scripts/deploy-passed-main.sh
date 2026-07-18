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
deployed=false
draining=false
stage=select
compose=()
previous_compose=()

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
  temporary=$(mktemp "${STATE_DIR}/failed.json.XXXXXX")
  jq -n --arg source_commit "${revision}" --arg reason "${reason}" \
    --argjson ci_run_id "${run_id:-0}" --arg ci_url "${run_url}" \
    '{source_commit:$source_commit,ci_run_id:$ci_run_id,ci_url:$ci_url,reason:$reason,failed_at:(now|todate)}' >"${temporary}"
  mv "${temporary}" "${STATE_DIR}/failed.json"
}

# The PostgreSQL variables intentionally expand inside the container shell.
# shellcheck disable=SC2016
db_sql() {
  timeout 15s docker exec -i "${PROJECT_NAME}-db-1" sh -c 'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atq'
}

set_dispatch_paused() {
  local value=$1 result
  printf '%s\n' "BEGIN; SELECT pg_advisory_xact_lock(72623859790382856); UPDATE deployment_control SET dispatch_paused=${value}, updated_at=now() WHERE singleton=TRUE; COMMIT;" | db_sql >/dev/null
  result=$(printf '%s\n' "SELECT dispatch_paused FROM deployment_control WHERE singleton=TRUE;" | db_sql)
  if [[ "${value}" == TRUE ]]; then
    [[ "${result}" == t ]]
    draining=true
  else
    [[ "${result}" == f ]]
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
  image_id=$(docker inspect "${PROJECT_NAME}-app-1" --format '{{.Image}}')
  label=$(docker image inspect "${image_id}" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}')
  binary=$(timeout 10s docker exec "${PROJECT_NAME}-app-1" /app/thornhill --version)
  [[ "${label}" == "${expected}" && "${binary}" == "thornhill ${expected}" ]]
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
  curl --fail --silent --show-error --output /dev/null --max-time 15 "${LOCAL_APP_URL}"
  curl --fail --silent --show-error --output /dev/null --max-time 15 "${PUBLIC_APP_URL}"
  local_revision=$(status_revision "${LOCAL_STATUS_URL}")
  public_revision=$(status_revision "${PUBLIC_STATUS_URL}")
  [[ "${local_revision}" == "${expected}" && "${public_revision}" == "${expected}" ]]
  verify_running_revision "${expected}"
}

stop_database_cleanly() {
  local container="${PROJECT_NAME}-db-1" pid1_uid exit_code
  if ! docker container inspect "${container}" >/dev/null 2>&1 || \
    [[ $(docker inspect "${container}" --format '{{.State.Running}}') != true ]]; then
    return 0
  fi
  if ! pid1_uid=$(docker exec "${container}" stat -c %u /proc/1); then
    return 1
  fi
  if [[ "${pid1_uid}" == 70 ]]; then
    "${compose[@]}" stop --timeout 30 db
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
  [[ "${exit_code}" == 0 &&
    $(docker inspect "${container}" --format '{{.State.Running}}') == false &&
    $(docker inspect "${container}" --format '{{.State.Restarting}}') == false &&
    $(docker inspect "${container}" --format '{{.State.ExitCode}}') == 0 ]]
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

rollback() {
  local rollback_ok=false database_stopped=false deadline
  if [[ "${deployed}" == true && -n "${previous_image}" && -n "${previous_db_image}" && ${#previous_compose[@]} -gt 0 ]]; then
    echo "${stage} failed; rolling back to ${previous_revision} (${previous_image}, ${previous_db_image})" >&2
    # Stop the application before PostgreSQL so the database can checkpoint and
    # exit cleanly even when the failed stack still owns pooled connections.
    if "${compose[@]}" stop --timeout 30 app >/dev/null 2>&1; then
      if stop_database_cleanly >/dev/null 2>&1; then
        database_stopped=true
      else
        echo "CRITICAL: refusing rollback recreation after unverified database shutdown" >&2
      fi
    else
      echo "CRITICAL: refusing rollback recreation because the application did not stop cleanly" >&2
    fi
    if [[ "${database_stopped}" == true ]] && \
      THORNHILL_APP_IMAGE="${previous_image}" THORNHILL_POSTGRES_IMAGE="${previous_db_image}" \
      "${previous_compose[@]}" up -d --no-build --force-recreate db app >/dev/null; then
      deadline=$((SECONDS + TIMEOUT_SECONDS))
      while (( SECONDS < deadline )); do
        if verify_live_revision "${previous_revision}" >/dev/null 2>&1; then
          rollback_ok=true
          break
        fi
        sleep 1
      done
    fi
    mark_failed "${stage} failed; rollback_ok=${rollback_ok}"
  fi
  if [[ "${draining}" == true ]]; then
    if ! set_dispatch_paused FALSE; then
      echo "CRITICAL: failed to leave deployment drain mode" >&2
      return 1
    fi
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
controller=scripts/deploy-passed-main.sh
if ! git diff --quiet -- "${controller}" || ! git diff --cached --quiet -- "${controller}"; then
  defer_or_fail_checkout "modified controller ${controller}"
fi
if [[ $(git hash-object "${controller}") != $(git rev-parse "${revision}:${controller}") ]]; then
  defer_or_fail_checkout "controller does not match passing revision ${revision}; update the local checkout"
fi
rollback_mode=$(git show "${revision}:docs/rollback-compatibility.json" | jq -er '.mode')
if [[ "${rollback_mode}" != backward-compatible-additive ]]; then
  echo "Refusing automatic deployment: schema policy mode is ${rollback_mode@Q}, not backward-compatible-additive" >&2
  exit 1
fi

previous_revision=$(status_revision "${LOCAL_STATUS_URL}" 2>/dev/null || true)
if [[ "${previous_revision}" == "${revision}" ]]; then
  current_db_image=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{.Config.Image}}')
  if ! verify_live_revision "${revision}" || ! verify_running_db "${current_db_image}"; then
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
ln -s "${ROOT}/.env" "${source_dir}/.env"
ln -s "${ROOT}/.env" "${previous_source_dir}/.env"
db_image_override="${tmp}/db-image.override.yml"
printf 'services:\n  db:\n    image: %s\n' \
  "\${THORNHILL_POSTGRES_IMAGE:?set THORNHILL_POSTGRES_IMAGE}" >"${db_image_override}"
compose=(docker compose --project-name "${PROJECT_NAME}" -f "${source_dir}/docker-compose.yml" -f "${source_dir}/docker-compose.host.yml" -f "${db_image_override}")
previous_compose=(docker compose --project-name "${PROJECT_NAME}" -f "${previous_source_dir}/docker-compose.yml" -f "${previous_source_dir}/docker-compose.host.yml" -f "${db_image_override}")

stage=drain
ensure_deployment_control
set_dispatch_paused TRUE
active=$(active_jobs)
if [[ "${active}" != 0 ]]; then
  echo "Deferring ${revision}: ${active} active job(s) after entering drain mode"
  set_dispatch_paused FALSE
  exit 0
fi

deployed=true
stage=stop
# Compose replacement can otherwise stop the dependency before its client. Stop
# the app first, then give PostgreSQL its full clean-shutdown grace period.
"${compose[@]}" stop --timeout 30 app
stop_database_cleanly
stage=deploy
THORNHILL_APP_IMAGE="${image}" THORNHILL_POSTGRES_IMAGE="${db_image}" \
  "${compose[@]}" up -d --no-build --force-recreate db app
stage=verify
deadline=$((SECONDS + TIMEOUT_SECONDS))
while (( SECONDS < deadline )); do
  if verify_live_revision "${revision}" >/dev/null 2>&1 && \
    verify_running_db "${db_image}" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! verify_live_revision "${revision}" || ! verify_running_db "${db_image}"; then
  echo "Local/Tailnet/app/PostgreSQL runtime verification failed for ${revision}" >&2
  exit 1
fi
set_dispatch_paused FALSE
deployed=false
rm -f "${STATE_DIR}/failed.json" "${STATE_DIR}/deferred.json"
write_receipt "${revision}" "${image}" "${db_image}" "${run_id}" "${run_url}"
echo "Deployed ${revision} from successful CI run ${run_id}: ${run_url}"
