#!/usr/bin/env bash
# Install a CI-produced OCI bundle using only local operator authority.
set -euo pipefail
umask 077

SCRIPT_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
BUNDLE=$(cd -- "${SCRIPT_ROOT}" && pwd)
ENV_FILE=""
EXPECTED_SHA=""
PROJECT_NAME=thornhill
LOCAL_APP_URL=${LOCAL_APP_URL:-http://127.0.0.1:8787/}
LOCAL_STATUS_URL=${LOCAL_STATUS_URL:-http://127.0.0.1:8787/api/status}
PUBLIC_APP_URL=""
PUBLIC_STATUS_URL=""
STATE_DIR=${STATE_DIR:-${HOME}/.local/state/thornhill-local-release}
COMPOSE_OVERRIDE=""
CHECK_ONLY=0
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-60}

usage() {
  cat <<'USAGE'
Usage: install-release.sh --bundle DIR --env-file FILE --expected-sha SHA [options]

Install a previously downloaded Thornhill OCI release bundle. The installer is
local-only: it does not invoke GitHub, gh, a registry, SSH, or any remote
service. It loads the exact image archives in the bundle and recreates the
existing local Compose project without building.

Required:
  --bundle DIR          Release bundle directory (defaults to this script's directory)
  --env-file FILE       Existing host-local .env file, normally Thornhill's .env
  --expected-sha SHA    Exact 40-character revision approved by the operator

Optional:
  --local-app-url URL       Local app URL (default: http://127.0.0.1:8787/)
  --local-status-url URL    Local status URL (default: http://127.0.0.1:8787/api/status)
  --public-app-url URL      Tailnet/operator-visible app URL
  --public-status-url URL   Tailnet/operator-visible status URL
  --project-name NAME       Compose project name (default: thornhill)
  --state-dir DIR           Local receipt/recovery state directory
  --compose-override FILE   Compose override inside the bundle (default: host integration)
  --check-only              Verify the bundle only; do not load images or touch containers
  --help                    Show this help
USAGE
}

while (($#)); do
  case "$1" in
    --bundle) BUNDLE=${2:?missing value for --bundle}; shift 2 ;;
    --env-file) ENV_FILE=${2:?missing value for --env-file}; shift 2 ;;
    --expected-sha) EXPECTED_SHA=${2:?missing value for --expected-sha}; shift 2 ;;
    --local-app-url) LOCAL_APP_URL=${2:?missing value for --local-app-url}; shift 2 ;;
    --local-status-url) LOCAL_STATUS_URL=${2:?missing value for --local-status-url}; shift 2 ;;
    --public-app-url) PUBLIC_APP_URL=${2:?missing value for --public-app-url}; shift 2 ;;
    --public-status-url) PUBLIC_STATUS_URL=${2:?missing value for --public-status-url}; shift 2 ;;
    --project-name) PROJECT_NAME=${2:?missing value for --project-name}; shift 2 ;;
    --state-dir) STATE_DIR=${2:?missing value for --state-dir}; shift 2 ;;
    --compose-override) COMPOSE_OVERRIDE=${2:?missing value for --compose-override}; shift 2 ;;
    --check-only) CHECK_ONLY=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) printf 'Unknown option: %s\n' "$1" >&2; usage >&2; exit 64 ;;
  esac
done

[[ "${EXPECTED_SHA}" =~ ^[0-9a-f]{40}$ ]] || {
  printf 'Invalid --expected-sha: %s\n' "${EXPECTED_SHA@Q}" >&2
  exit 64
}
BUNDLE=$(realpath -e -- "${BUNDLE}")
[[ -d "${BUNDLE}" ]] || { printf 'Bundle directory is missing: %s\n' "${BUNDLE}" >&2; exit 1; }

if [[ -z "${ENV_FILE}" ]]; then
  ENV_FILE="${BUNDLE}/compose/.env"
fi
if [[ -e "${ENV_FILE}" ]]; then
  ENV_FILE=$(realpath -e -- "${ENV_FILE}")
fi

if [[ -z "${COMPOSE_OVERRIDE}" ]]; then
  COMPOSE_OVERRIDE="${BUNDLE}/compose/docker-compose.host.yml"
elif [[ -e "${COMPOSE_OVERRIDE}" ]]; then
  COMPOSE_OVERRIDE=$(realpath -e -- "${COMPOSE_OVERRIDE}")
fi
case "${COMPOSE_OVERRIDE}" in
  "${BUNDLE}"/*) ;;
  *)
    printf 'Compose override must be a checksummed bundle member: %s\n' "${COMPOSE_OVERRIDE}" >&2
    exit 1
    ;;
esac

manifest="${BUNDLE}/images/manifest.json"
release="${BUNDLE}/release.json"
checksums="${BUNDLE}/SHA256SUMS"
for path in "${manifest}" "${release}" "${checksums}" "${BUNDLE}/images/app.tar.gz" \
  "${BUNDLE}/images/postgres.tar.gz" "${BUNDLE}/compose/docker-compose.yml" \
  "${BUNDLE}/install-release.sh" "${BUNDLE}/scripts/local-recovery.sh"; do
  [[ -e "${path}" ]] || { printf 'Bundle member is missing: %s\n' "${path}" >&2; exit 1; }
done
[[ -f "${COMPOSE_OVERRIDE}" ]] || { printf 'Compose override is missing: %s\n' "${COMPOSE_OVERRIDE}" >&2; exit 1; }

jq -e --arg expected "${EXPECTED_SHA}" '
  .version == 1 and .source_commit == $expected and
  .package_type == "thornhill-local-oci-bundle" and
  .images_manifest == "images/manifest.json" and
  .install_entrypoint == "install-release.sh"
' "${release}" >/dev/null
jq -e --arg expected "${EXPECTED_SHA}" '
  .version == 1 and .source_commit == $expected and
  .images.app.archive == "app.tar.gz" and
  .images.app.local_tag == "thornhill:ci" and
  .images.postgres.archive == "postgres.tar.gz" and
  .images.postgres.local_tag == "thornhill-postgres:ci" and
  (.images.app.id | test("^sha256:[0-9a-f]{64}$")) and
  (.images.postgres.id | test("^sha256:[0-9a-f]{64}$")) and
  .images.app.revision == $expected and
  .images.postgres.revision == $expected
' "${manifest}" >/dev/null
(
  cd "${BUNDLE}"
  sha256sum -c SHA256SUMS >/dev/null
)
override_rel=${COMPOSE_OVERRIDE#"${BUNDLE}/"}
grep -Fq -- "  ./${override_rel}" "${checksums}" || {
  printf 'Compose override is not covered by SHA256SUMS: %s\n' "${COMPOSE_OVERRIDE}" >&2
  exit 1
}

app_tag=$(jq -r .images.app.local_tag "${manifest}")
db_tag=$(jq -r .images.postgres.local_tag "${manifest}")
expected_app_id=$(jq -r .images.app.id "${manifest}")
expected_db_id=$(jq -r .images.postgres.id "${manifest}")
[[ "${app_tag}" == thornhill:ci && "${db_tag}" == thornhill-postgres:ci ]] || {
  printf '%s\n' 'Bundle image tags are not the approved CI tags' >&2
  exit 1
}

printf 'Bundle verified: source_commit=%s\n' "${EXPECTED_SHA}"
printf 'Bundle image IDs: app=%s db=%s\n' "${expected_app_id}" "${expected_db_id}"
if (( CHECK_ONLY )); then
  printf '%s\n' 'Check-only completed; no images, containers, volumes, or services were changed.'
  exit 0
fi

[[ -f "${ENV_FILE}" ]] || { printf 'Host env file is missing: %s\n' "${ENV_FILE}" >&2; exit 1; }
env_owner=$(stat -c %u "${ENV_FILE}")
env_mode=$(stat -c %a "${ENV_FILE}")
[[ "${env_mode}" =~ ^[0-7]00$ ]] || {
  printf 'Host env file is accessible to group/other: %s\n' "${ENV_FILE}" >&2
  exit 1
}
[[ "${env_owner}" == "$(id -u)" ]] || {
  printf 'Host env file is not owned by the installer user: %s\n' "${ENV_FILE}" >&2
  exit 1
}

for command in docker curl jq sha256sum flock timeout stat python3 grep; do
  command -v "${command}" >/dev/null || { printf 'Missing required command: %s\n' "${command}" >&2; exit 1; }
done
docker compose version >/dev/null
install -d -m 0700 "${STATE_DIR}"
exec 9>"${STATE_DIR}/install.lock"
flock -n 9 || { printf '%s\n' 'Another local release installation is running' >&2; exit 1; }
if [[ -e "${STATE_DIR}/transition.json" ]]; then
  printf 'Refusing install: unresolved transition journal exists at %s\n' "${STATE_DIR}/transition.json" >&2
  exit 1
fi
if [[ -f "${STATE_DIR}/failed.json" ]]; then
  failed_target=$(jq -er .target_revision "${STATE_DIR}/failed.json" 2>/dev/null || true)
  if [[ "${failed_target}" == "${EXPECTED_SHA}" && "${RETRY_FAILED:-0}" != 1 ]]; then
    printf 'Refusing retry of quarantined revision %s; set RETRY_FAILED=1 after review\n' "${EXPECTED_SHA}" >&2
    exit 1
  fi
fi

printf 'Loading qualified application image locally\n'
docker load --input "${BUNDLE}/images/app.tar.gz" >/dev/null
printf 'Loading qualified PostgreSQL image locally\n'
docker load --input "${BUNDLE}/images/postgres.tar.gz" >/dev/null
loaded_app_id=$(docker image inspect "${app_tag}" --format '{{.Id}}')
loaded_db_id=$(docker image inspect "${db_tag}" --format '{{.Id}}')
[[ "${loaded_app_id}" == "${expected_app_id}" && "${loaded_db_id}" == "${expected_db_id}" ]] || {
  printf 'Loaded image IDs do not match the bundle manifest\n' >&2
  exit 1
}
app_label=$(docker image inspect "${app_tag}" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}')
db_label=$(docker image inspect "${db_tag}" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}')
[[ "${app_label}" == "${EXPECTED_SHA}" && "${db_label}" == "${EXPECTED_SHA}" ]] || {
  printf 'Loaded image revision labels do not match %s\n' "${EXPECTED_SHA}" >&2
  exit 1
}
[[ "$(timeout 10s docker run --rm --network none "${app_tag}" --version)" == "thornhill ${EXPECTED_SHA}" ]] || {
  printf 'Loaded application image reports the wrong revision\n' >&2
  exit 1
}
docker tag "${app_tag}" "thornhill-app:${EXPECTED_SHA}"
docker tag "${db_tag}" "thornhill-postgres:${EXPECTED_SHA}"

tmp=$(mktemp -d "${STATE_DIR}/.install.XXXXXX")
cleanup_tmp() { rm -rf -- "${tmp}"; }
trap cleanup_tmp EXIT

db_override="${tmp}/db-image.override.yml"
write_db_override() {
  local image_ref=$1
  printf 'services:\n  db:\n    image: %s\n' "${image_ref}" >"${db_override}"
}

export THORNHILL_ENV_FILE="${ENV_FILE}"
export THORNHILL_REVISION="${EXPECTED_SHA}"
export THORNHILL_APP_IMAGE="thornhill-app:${EXPECTED_SHA}"
export THORNHILL_POSTGRES_IMAGE="thornhill-postgres:${EXPECTED_SHA}"
write_db_override "${THORNHILL_POSTGRES_IMAGE}"
compose=(docker compose --project-name "${PROJECT_NAME}" --env-file "${ENV_FILE}" \
  -f "${BUNDLE}/compose/docker-compose.yml" -f "${COMPOSE_OVERRIDE}" -f "${db_override}")
"${compose[@]}" config --quiet

status_revision() {
  curl --fail --silent --show-error --max-time 15 "$1" |
    jq -er 'select(.status == "ok" and .versioned == true) | .source_commit'
}

db_sql() {
  timeout 15s docker exec -i "${PROJECT_NAME}-db-1" \
    sh -c 'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atq'
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
    RAISE EXCEPTION USING ERRCODE='55000', MESSAGE='job dispatch is temporarily paused for local release installation';
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

set_dispatch_paused() {
  local value=$1 result
  printf 'BEGIN; SELECT pg_advisory_xact_lock(72623859790382856); UPDATE deployment_control SET dispatch_paused=%s, updated_at=now() WHERE singleton=TRUE; COMMIT;\n' "${value}" |
    db_sql >/dev/null
  result=$(printf 'SELECT dispatch_paused FROM deployment_control WHERE singleton=TRUE;\n' | db_sql)
  if [[ "${value}" == TRUE ]]; then
    [[ "${result}" == t ]]
  else
    [[ "${result}" == f ]]
  fi
}

active_jobs() {
  printf "%s\n" "SELECT count(*) FROM jobs WHERE status IN ('queued','running','needs_input','needs_approval');" | db_sql
}

verify_running_revision() {
  local expected=$1 image_id label binary
  image_id=$(docker inspect "${PROJECT_NAME}-app-1" --format '{{.Image}}')
  label=$(docker image inspect "${image_id}" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}')
  binary=$(timeout 10s docker exec "${PROJECT_NAME}-app-1" /app/thornhill --version)
  [[ "${label}" == "${expected}" && "${binary}" == "thornhill ${expected}" ]]
}

verify_running_db() {
  local expected_image=$1 actual health read_only cap_drop security pids uid
  actual=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{.Config.Image}}')
  health=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{.State.Health.Status}}')
  read_only=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{.HostConfig.ReadonlyRootfs}}')
  cap_drop=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{json .HostConfig.CapDrop}}')
  security=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{json .HostConfig.SecurityOpt}}')
  pids=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{.HostConfig.PidsLimit}}')
  uid=$(docker exec "${PROJECT_NAME}-db-1" stat -c %u /proc/1)
  [[ "${actual}" == "${expected_image}" && "${health}" == healthy && "${read_only}" == true &&
    "${cap_drop}" == *ALL* && "${security}" == *no-new-privileges* && "${pids}" == 256 && "${uid}" == 70 ]]
}

verify_runtime() {
  local expected=$1 db_image=$2
  curl --fail --silent --show-error --max-time 15 "${LOCAL_APP_URL}" >/dev/null
  if [[ -n "${PUBLIC_APP_URL}" ]]; then
    curl --fail --silent --show-error --max-time 15 "${PUBLIC_APP_URL}" >/dev/null
  fi
  [[ "$(status_revision "${LOCAL_STATUS_URL}")" == "${expected}" ]]
  [[ -z "${PUBLIC_STATUS_URL}" || "$(status_revision "${PUBLIC_STATUS_URL}")" == "${expected}" ]]
  verify_running_revision "${expected}"
  verify_running_db "${db_image}"
}

wait_for_runtime() {
  local expected=$1 db_image=$2 deadline
  deadline=$((SECONDS + TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    if verify_runtime "${expected}" "${db_image}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  verify_runtime "${expected}" "${db_image}"
}

stop_application_cleanly() {
  local container="${PROJECT_NAME}-app-1" running state
  if ! running=$(docker inspect "${container}" --format '{{.State.Running}}'); then
    return 1
  fi
  if [[ "${running}" == true ]]; then
    docker stop --timeout 30 "${container}" >/dev/null
  fi
  for _ in 1 2; do
    state=$(docker inspect "${container}" --format '{{.State.Running}} {{.State.Restarting}}')
    [[ "${state}" == 'false false' ]]
    sleep 0.2
  done
}

stop_database_cleanly() {
  local container="${PROJECT_NAME}-db-1" running uid exit_code state
  running=$(docker inspect "${container}" --format '{{.State.Running}}')
  if [[ "${running}" != true ]]; then
    state=$(docker inspect "${container}" --format '{{.State.Running}} {{.State.Restarting}} {{.State.ExitCode}}')
    [[ "${state}" == 'false false 0' ]]
    return
  fi
  uid=$(docker exec "${container}" stat -c %u /proc/1)
  if [[ "${uid}" == 70 ]]; then
    docker stop --timeout 30 "${container}" >/dev/null
  else
    docker update --restart=no "${container}" >/dev/null
    docker exec --detach --user 70:70 "${container}" \
      sh -ec 'exec pg_ctl -D "$PGDATA" -m fast -w -t 30 stop'
    exit_code=$(timeout 35s docker wait "${container}")
    [[ "${exit_code}" == 0 ]]
  fi
  for _ in 1 2; do
    state=$(docker inspect "${container}" --format '{{.State.Running}} {{.State.Restarting}} {{.State.ExitCode}}')
    [[ "${state}" == 'false false 0' ]]
    sleep 0.2
  done
}

write_receipt() {
  local previous=$1 previous_app=$2 previous_db=$3 temporary app_id db_id
  app_id=$(docker image inspect "${THORNHILL_APP_IMAGE}" --format '{{.Id}}')
  db_id=$(docker image inspect "${THORNHILL_POSTGRES_IMAGE}" --format '{{.Id}}')
  temporary=$(mktemp "${STATE_DIR}/receipt.json.XXXXXX")
  jq -n --arg source_commit "${EXPECTED_SHA}" \
    --arg app_image "${THORNHILL_APP_IMAGE}" --arg db_image "${THORNHILL_POSTGRES_IMAGE}" \
    --arg app_id "${app_id}" --arg db_id "${db_id}" \
    --arg previous_commit "${previous}" --arg previous_app_image "${previous_app}" \
    --arg previous_db_image "${previous_db}" \
    '{version:1,source_commit:$source_commit,app_image:$app_image,db_image:$db_image,
      app_image_id:$app_id,db_image_id:$db_id,
      previous_commit:$previous_commit,previous_app_image:$previous_app_image,previous_db_image:$previous_db_image,
      install_mode:"local-bundle"}' >"${temporary}"
  chmod 0600 "${temporary}"
  mv -- "${temporary}" "${STATE_DIR}/deployed.json"
}

write_failed() {
  local temporary
  temporary=$(mktemp "${STATE_DIR}/failed.json.XXXXXX")
  jq -n --arg target "${EXPECTED_SHA}" --arg previous "${previous_revision}" \
    '{version:1,target_revision:$target,previous_revision:$previous,
      outcome:"verified-rollback",install_mode:"local-bundle"}' >"${temporary}"
  chmod 0600 "${temporary}"
  mv -- "${temporary}" "${STATE_DIR}/failed.json"
}

write_journal() {
  local phase=$1 temporary
  temporary=$(mktemp "${STATE_DIR}/transition.json.XXXXXX")
  jq -n --arg phase "${phase}" --arg target "${EXPECTED_SHA}" \
    --arg previous "${previous_revision}" --arg previous_app "${previous_app_local}" \
    --arg previous_db "${previous_db_local}" \
    '{version:1,phase:$phase,target_revision:$target,previous_revision:$previous,
      previous_app_image:$previous_app,previous_db_image:$previous_db,
      updated_at:(now|todate)}' >"${temporary}"
  chmod 0600 "${temporary}"
  mv -- "${temporary}" "${STATE_DIR}/transition.json"
}

clear_journal() { rm -f -- "${STATE_DIR}/transition.json"; }

app_stopped=false
db_stopped=false
draining=false
changed=false
rollback_ok=false
previous_revision=""
previous_app_local=""
previous_db_local=""

rollback() {
  if [[ "${app_stopped}" != true || "${db_stopped}" != true ]]; then
    return 1
  fi
  export THORNHILL_APP_IMAGE="${previous_app_local}"
  export THORNHILL_POSTGRES_IMAGE="${previous_db_local}"
  write_db_override "${THORNHILL_POSTGRES_IMAGE}"
  if "${compose[@]}" up -d --no-build --force-recreate db app >/dev/null 2>&1; then
    if wait_for_runtime "${previous_revision}" "${previous_db_local}"; then
      rollback_ok=true
    fi
  fi
  if [[ "${rollback_ok}" == true ]]; then
    set_dispatch_paused FALSE
  fi
}

on_exit() {
  local rc=$?
  trap - EXIT INT TERM
  if (( rc != 0 )); then
    if [[ "${changed}" == true ]]; then
      rollback || true
    elif [[ "${draining}" == true ]]; then
      set_dispatch_paused FALSE || true
    fi
    if [[ "${rollback_ok}" != true && "${changed}" == true ]]; then
      printf '%s\n' 'CRITICAL: local rollback was not verified; dispatch remains paused' >&2
    fi
    if [[ "${rollback_ok}" == true && "${changed}" == true ]]; then
      write_failed || true
      clear_journal || true
    fi
  elif [[ "${draining}" == true ]]; then
    set_dispatch_paused FALSE || rc=1
  fi
  if (( rc == 0 )); then
    clear_journal
  fi
  exit "${rc}"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

previous_revision=$(status_revision "${LOCAL_STATUS_URL}")
previous_app_image=$(docker inspect "${PROJECT_NAME}-app-1" --format '{{.Config.Image}}')
previous_db_image=$(docker inspect "${PROJECT_NAME}-db-1" --format '{{.Config.Image}}')
[[ "${previous_revision}" =~ ^[0-9a-f]{40}$ ]]
verify_runtime "${previous_revision}" "${previous_db_image}"

docker tag "${previous_app_image}" "thornhill-rollback-app:${previous_revision}"
docker tag "${previous_db_image}" "thornhill-rollback-db:${previous_revision}"
previous_app_local="thornhill-rollback-app:${previous_revision}"
previous_db_local="thornhill-rollback-db:${previous_revision}"

# The password remains host-owned and stable. Refuse rather than silently rotate it.
database_password=$(awk -F= '$1 == "THORNHILL_DB_PASSWORD" {print substr($0, index($0, "=") + 1)}' "${ENV_FILE}")
[[ "${database_password}" =~ ^[0-9a-f]{64}$ ]]
printf '%s\n' "${database_password}" |
  "${BUNDLE}/scripts/rotate-postgres-role-password.sh" --verify-only "${PROJECT_NAME}-db-1" >/dev/null

ensure_deployment_control
set_dispatch_paused TRUE
draining=true
active=$(active_jobs)
if [[ "${active}" != 0 ]]; then
  printf 'Refusing local install: %s active job(s)\n' "${active}" >&2
  exit 1
fi

THORNHILL_RECOVERY_SOURCE_COMMIT="${EXPECTED_SHA}" \
STATE_DIR="${STATE_DIR}" PROJECT_NAME="${PROJECT_NAME}" \
THORNHILL_RECOVERY_POSTGRES_IMAGE="${previous_db_image}" \
"${BUNDLE}/scripts/local-recovery.sh" snapshot >/dev/null
write_journal prepared
changed=true
stop_application_cleanly
app_stopped=true
write_journal application_stopped
stop_database_cleanly
db_stopped=true
write_journal database_stopped
export THORNHILL_APP_IMAGE="thornhill-app:${EXPECTED_SHA}"
export THORNHILL_POSTGRES_IMAGE="thornhill-postgres:${EXPECTED_SHA}"
write_db_override "${THORNHILL_POSTGRES_IMAGE}"
"${compose[@]}" up -d --no-build --force-recreate db app >/dev/null
write_journal target_started
wait_for_runtime "${EXPECTED_SHA}" "${THORNHILL_POSTGRES_IMAGE}"
write_receipt "${previous_revision}" "${previous_app_local}" "${previous_db_local}"
draining=false
set_dispatch_paused FALSE
printf 'Installed local release %s without remote access\n' "${EXPECTED_SHA}"
