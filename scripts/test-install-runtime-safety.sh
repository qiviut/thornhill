#!/usr/bin/env bash
# Variables are consumed by actual installer functions extracted at runtime.
# shellcheck disable=SC2034
# Exercise installer runtime verification and rollback with a deterministic Docker stub.
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
installer="${root}/scripts/install-local-release.sh"
[[ -f "${installer}" ]] || { printf 'Installer not found: %s\n' "${installer}" >&2; exit 1; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/thornhill-install-runtime-safety.XXXXXX")
cleanup() { rm -rf -- "${tmp}"; }
trap cleanup EXIT

extract_function() {
  local name=$1
  awk -v target="${name}" '
    $0 == target "() {" { copying=1; found=1 }
    copying { print }
    copying && /^}$/ { closed=1; copying=0; exit }
    END { if (!found || !closed) exit 1 }
  ' "${installer}"
}

functions_file="${tmp}/installer-functions.sh"
: >"${functions_file}"
for name in \
  verify_running_revision verify_running_db verify_runtime \
  stop_application_cleanly stop_database_cleanly rollback; do
  if ! extract_function "${name}" >>"${functions_file}"; then
    printf 'Could not extract %s() from %s\n' "${name}" "${installer}" >&2
    exit 1
  fi
done

# Source only the real function bodies; every external command is stubbed below.
# This deliberately calls verify_runtime in an if-condition, matching wait_for_runtime.
# shellcheck disable=SC1090
source "${functions_file}"

PROJECT_NAME=runtime-safety-test
TEST_REVISION=0123456789abcdef0123456789abcdef01234567
TEST_DB_IMAGE=thornhill-postgres:expected
LOCAL_APP_URL=http://local-app.test/health
PUBLIC_APP_URL=https://public-app.test/health
LOCAL_STATUS_URL=http://local-status.test/status
PUBLIC_STATUS_URL=https://public-status.test/status
TEST_MODE=runtime
TEST_CASE=pass
STATE_DIR="${tmp}/state"
mkdir -p "${STATE_DIR}"
TEST_REVISION_LABEL=${TEST_REVISION}
APP_RUNNING=true
DB_RUNNING=true
DB_UID=70
EVENTS=()

curl() {
  local url arg
  for arg in "$@"; do url=${arg}; done
  case "${TEST_CASE}:${url}" in
    "http-local-error:${LOCAL_APP_URL}"|"http-public-error:${PUBLIC_APP_URL}") return 22 ;;
  esac
  return 0
}

status_revision() {
  case "${TEST_CASE}:$1" in
    "status-local-error:${LOCAL_STATUS_URL}"|"status-public-error:${PUBLIC_STATUS_URL}") return 7 ;;
    "status-local-output-error:${LOCAL_STATUS_URL}"|"status-public-output-error:${PUBLIC_STATUS_URL}")
      printf '%s\n' "${TEST_REVISION}"
      return 7 ;;
    "status-local-mismatch:${LOCAL_STATUS_URL}"|"status-public-mismatch:${PUBLIC_STATUS_URL}")
      printf '%s\n' 'ffffffffffffffffffffffffffffffffffffffff'
      return 0 ;;
  esac
  printf '%s\n' "${TEST_REVISION}"
}

timeout() {
  shift
  "$@"
}

sleep() { :; }

fake_compose() {
  EVENTS+=(compose-up)
  return 0
}

write_db_override() {
  EVENTS+=(write-db-override)
  return 0
}

wait_for_runtime() {
  EVENTS+=(wait-runtime)
  return 0
}

set_dispatch_paused() {
  EVENTS+=("dispatch-$1")
  [[ "${TEST_CASE}" != rollback-pause-error || "${1}" != TRUE ]] || return 1
  return 0
}

active_jobs() {
  [[ "${TEST_CASE}" != rollback-active-jobs ]] || { printf '1\n'; return; }
  printf '0\n'
}

docker() {
  local container format image_id
  if [[ "${TEST_MODE}" == runtime ]]; then
    case "${1:-}" in
      inspect)
        container=${2:-}
        format=${4:-}
        if [[ "${container}" == "${PROJECT_NAME}-app-1" ]]; then
          case "${format}" in
            '{{.Image}}')
              [[ "${TEST_CASE}" != app-inspect-error ]] || return 1
              printf '%s\n' 'sha256:runtime-app'
              return 0 ;;
          esac
        elif [[ "${container}" == "${PROJECT_NAME}-db-1" ]]; then
          case "${format}" in
            '{{.Config.Image}}')
              [[ "${TEST_CASE}" != db-image-error ]] || return 1
              if [[ "${TEST_CASE}" == db-image-mismatch ]]; then printf '%s\n' 'thornhill-postgres:wrong'
              else printf '%s\n' "${TEST_DB_IMAGE}"; fi
              return 0 ;;
            '{{.State.Health.Status}}')
              [[ "${TEST_CASE}" != db-health-error ]] || return 1
              if [[ "${TEST_CASE}" == db-health-mismatch ]]; then printf '%s\n' unhealthy
              else printf '%s\n' healthy; fi
              return 0 ;;
            '{{.HostConfig.ReadonlyRootfs}}')
              [[ "${TEST_CASE}" != db-readonly-error ]] || return 1
              if [[ "${TEST_CASE}" == db-readonly-mismatch ]]; then printf '%s\n' false
              else printf '%s\n' true; fi
              return 0 ;;
            '{{json .HostConfig.CapDrop}}')
              [[ "${TEST_CASE}" != db-capdrop-error ]] || return 1
              if [[ "${TEST_CASE}" == db-capdrop-mismatch ]]; then printf '%s\n' '[]'
              else printf '%s\n' '["ALL"]'; fi
              return 0 ;;
            '{{json .HostConfig.SecurityOpt}}')
              [[ "${TEST_CASE}" != db-security-error ]] || return 1
              if [[ "${TEST_CASE}" == db-security-mismatch ]]; then printf '%s\n' '[]'
              else printf '%s\n' '["no-new-privileges:true"]'; fi
              return 0 ;;
            '{{.HostConfig.PidsLimit}}')
              [[ "${TEST_CASE}" != db-pids-error ]] || return 1
              if [[ "${TEST_CASE}" == db-pids-mismatch ]]; then printf '%s\n' 128
              else printf '%s\n' 256; fi
              return 0 ;;
          esac
        fi
        ;;
      image)
        [[ "${2:-}" == inspect ]] || { printf 'Unexpected docker command: %s\n' "$*" >&2; return 2; }
        image_id=${3:-}
        format=${5:-}
        [[ "${image_id}" == sha256:runtime-app ]] || { printf 'Unexpected image ID: %s\n' "${image_id}" >&2; return 2; }
        case "${format}" in
          *Config.Labels*)
            [[ "${TEST_CASE}" != app-label-error ]] || return 1
            if [[ "${TEST_CASE}" == app-label-mismatch ]]; then printf '%s\n' 'ffffffffffffffffffffffffffffffffffffffff'
            else printf '%s\n' "${TEST_REVISION_LABEL}"; fi
            return 0 ;;
        esac
        ;;
      exec)
        container=${2:-}
        if [[ "${container}" == "${PROJECT_NAME}-app-1" ]]; then
          case "${TEST_CASE}" in
            app-version-error) return 1 ;;
            app-version-mismatch) printf '%s\n' 'thornhill ffffffffffffffffffffffffffffffffffffffff'; return 0 ;;
          esac
          printf 'thornhill %s\n' "${TEST_REVISION}"
          return 0
        elif [[ "${container}" == "${PROJECT_NAME}-db-1" ]]; then
          [[ "${TEST_CASE}" != db-uid-error ]] || return 1
          if [[ "${TEST_CASE}" == db-uid-mismatch ]]; then printf '%s\n' 999
          else printf '%s\n' 70; fi
          return 0
        fi
        ;;
    esac
    printf 'Unexpected runtime docker command: %s\n' "$*" >&2
    return 2
  fi

  case "${1:-}" in
    container)
      [[ "${TEST_CASE}" != rollback-list-error ]] || return 1
      if [[ "${TEST_CASE}" != rollback-app-missing ]]; then printf '%s\n' "${PROJECT_NAME}-app-1"; fi
      printf '%s\n' "${PROJECT_NAME}-db-1"
      return 0 ;;
    inspect)
      container=${2:-}
      format=${4:-}
      if [[ "${container}" == "${PROJECT_NAME}-app-1" ]]; then
        [[ "${TEST_CASE}" != rollback-app-missing && "${TEST_CASE}" != rollback-list-error ]] || return 1
        case "${format}" in
          '{{.State.Running}}') printf '%s\n' "${APP_RUNNING}"; return 0 ;;
          '{{.State.Running}} {{.State.Restarting}}') printf '%s false\n' "${APP_RUNNING}"; return 0 ;;
        esac
      elif [[ "${container}" == "${PROJECT_NAME}-db-1" ]]; then
        case "${format}" in
          '{{.State.Running}}') printf '%s\n' "${DB_RUNNING}"; return 0 ;;
          '{{.State.Running}} {{.State.Restarting}} {{.State.ExitCode}}') printf '%s false 0\n' "${DB_RUNNING}"; return 0 ;;
        esac
      fi
      ;;
    exec)
      if [[ "${2:-}" == "${PROJECT_NAME}-db-1" ]]; then
        printf '%s\n' "${DB_UID}"
        return 0
      fi
      ;;
    stop)
      container=${4:-}
      case "${container}" in
        "${PROJECT_NAME}-app-1")
          EVENTS+=(stop-app)
          [[ "${TEST_CASE}" != rollback-app-stop-error ]] || return 31
          APP_RUNNING=false
          return 0 ;;
        "${PROJECT_NAME}-db-1")
          EVENTS+=(stop-db)
          [[ "${TEST_CASE}" != rollback-db-stop-error ]] || return 32
          DB_RUNNING=false
          return 0 ;;
      esac
      ;;
  esac
  printf 'Unexpected rollback docker command: %s\n' "$*" >&2
  return 2
}

runtime_failures=(
  http-local-error http-public-error
  status-local-error status-local-output-error status-local-mismatch
  status-public-error status-public-output-error status-public-mismatch
  app-inspect-error app-label-error app-label-mismatch app-version-error app-version-mismatch
  db-image-error db-image-mismatch db-health-error db-health-mismatch
  db-readonly-error db-readonly-mismatch db-capdrop-error db-capdrop-mismatch
  db-security-error db-security-mismatch db-pids-error db-pids-mismatch
  db-uid-error db-uid-mismatch
)

failures=0
baseline_ok=true
if ! verify_runtime "${TEST_REVISION}" "${TEST_DB_IMAGE}" >/dev/null 2>&1; then
  printf '%s\n' 'Runtime verification rejected the all-good fixture' >&2
  baseline_ok=false
  failures=$((failures + 1))
fi
runtime_rejected=0
for TEST_CASE in "${runtime_failures[@]}"; do
  if verify_runtime "${TEST_REVISION}" "${TEST_DB_IMAGE}" >/dev/null 2>&1; then
    printf 'Runtime verification accepted failure case: %s\n' "${TEST_CASE}" >&2
    failures=$((failures + 1))
  else
    runtime_rejected=$((runtime_rejected + 1))
  fi
done
printf 'Runtime verification: %d/%d failure cases rejected; all-good baseline %s\n' \
  "${runtime_rejected}" "${#runtime_failures[@]}" "${baseline_ok}"

assert_event_order() {
  local before=$1 after=$2 before_index=-1 after_index=-1 event index=0
  for event in "${EVENTS[@]}"; do
    if [[ "${event}" == "${before}" && ${before_index} == -1 ]]; then before_index=${index}; fi
    if [[ "${event}" == "${after}" && ${after_index} == -1 ]]; then after_index=${index}; fi
    index=$((index + 1))
  done
  if (( before_index < 0 || after_index < 0 || before_index >= after_index )); then
    printf 'Expected event %s before %s; observed: %s\n' \
      "${before}" "${after}" "${EVENTS[*]:-(none)}" >&2
    return 1
  fi
}

assert_event_absent() {
  local missing=$1 event
  for event in "${EVENTS[@]}"; do
    if [[ "${event}" == "${missing}" ]]; then
      printf 'Unexpected event %s; observed: %s\n' "${missing}" "${EVENTS[*]}" >&2
      return 1
    fi
  done
}

reset_rollback_case() {
  TEST_MODE=rollback
  TEST_CASE=$1
  EVENTS=()
  APP_RUNNING=true
  DB_RUNNING=true
  DB_UID=70
  app_stopped=true
  db_stopped=true
  previous_revision=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  previous_app_local=thornhill-rollback-app:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  previous_db_local=thornhill-rollback-db:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  rollback_ok=false
  target_start_attempted=true
  had_previous_receipt=false
  THORNHILL_APP_IMAGE=thornhill-app:target
  THORNHILL_POSTGRES_IMAGE=thornhill-postgres:target
  compose=(fake_compose)
}

reset_rollback_case rollback-success
if rollback >/dev/null 2>&1; then
  rollback_order_ok=true
  if ! assert_event_order stop-app stop-db; then rollback_order_ok=false; fi
  if ! assert_event_order stop-db compose-up; then rollback_order_ok=false; fi
  if ! assert_event_order dispatch-TRUE stop-app; then rollback_order_ok=false; fi
  if [[ "${rollback_order_ok}" == true ]]; then
    printf '%s\n' 'Rollback stopped app, then database, before Compose recreation'
  else
    failures=$((failures + 1))
  fi
else
  printf '%s\n' 'Rollback failed with cleanly stoppable replacement containers' >&2
  failures=$((failures + 1))
fi

for failure in rollback-app-stop-error rollback-db-stop-error rollback-pause-error rollback-active-jobs rollback-list-error; do
  reset_rollback_case "${failure}"
  rollback_failed=false
  if rollback >/dev/null 2>&1; then
    printf 'Rollback reported success after shutdown failure: %s\n' "${failure}" >&2
    failures=$((failures + 1))
  else
    rollback_failed=true
  fi
  if assert_event_absent compose-up; then
    if [[ "${rollback_failed}" == true ]]; then
      printf 'Rollback did not recreate containers after %s\n' "${failure}"
    fi
  else
    failures=$((failures + 1))
  fi
done

reset_rollback_case rollback-app-missing
if ! rollback >/dev/null 2>&1 || ! assert_event_order stop-db compose-up; then
  printf '%s\n' 'Rollback failed for daemon-confirmed missing app' >&2
  failures=$((failures + 1))
fi

if (( failures != 0 )); then
  printf 'Installer runtime safety regression checks failed (%d issue(s))\n' "${failures}" >&2
  exit 1
fi
printf '%s\n' 'Installer runtime safety regression checks passed'
