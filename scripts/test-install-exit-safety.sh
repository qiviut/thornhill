#!/usr/bin/env bash
# Installer functions are extracted dynamically and use these fixture variables.
# shellcheck disable=SC2034
# Exercise release journal, dispatch, and receipt failure paths without deployment.
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
installer="${root}/scripts/install-local-release.sh"
[[ -f "${installer}" ]] || { printf 'Installer not found: %s\n' "${installer}" >&2; exit 1; }
tmp_root=$(mktemp -d "${TMPDIR:-/tmp}/thornhill-install-exit-safety.XXXXXX")
cleanup() { rm -rf -- "${tmp_root}"; }
trap cleanup EXIT

extract_function() {
  local name=$1
  awk -v target="${name}" '
    !copying && $0 ~ ("^" target "\\(\\) \\{") {
      copying=1
      found=1
      print
      if ($0 ~ /}[[:space:]]*$/) { closed=1; copying=0; exit }
      next
    }
    copying {
      print
      if ($0 ~ /^}[[:space:]]*$/) { closed=1; copying=0; exit }
    }
    END { if (!found || !closed) exit 1 }
  ' "${installer}"
}

functions_file="${tmp_root}/installer-functions.sh"
: >"${functions_file}"
for name in set_dispatch_paused clear_journal write_failed rollback on_exit; do
  if ! extract_function "${name}" >>"${functions_file}"; then
    printf 'Could not extract %s() from %s\n' "${name}" "${installer}" >&2
    exit 1
  fi
done

# Source only the real installer functions; external effects are sandboxed below.
# shellcheck disable=SC1090
source "${functions_file}"

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}

journal_state() {
  if [[ -e "${STATE_DIR}/transition.json" ]]; then printf present; else printf absent; fi
}

record_event() {
  printf '%s:%s\n' "$1" "$(journal_state)" >>"${EVENT_LOG}"
}

# db_sql models the installer SQL boundary and persists dispatch state in the
# per-case sandbox. An unpause may succeed at UPDATE but fail its readback.
db_sql() {
  local query value state
  query=$(cat) || return 1
  if [[ "${query}" == *'UPDATE deployment_control'* ]]; then
    case "${query}" in
      *'dispatch_paused=TRUE'*) value=TRUE ;;
      *'dispatch_paused=FALSE'*) value=FALSE ;;
      *) return 90 ;;
    esac
    record_event "dispatch-update-${value}"
    if [[ "${value}" == FALSE &&
      ( "${TEST_CASE}" == pause-unpause-error || "${TEST_CASE}" == rollback-unpause-error ) ]]; then
      : # Keep the prior paused state so the actual readback check fails.
    elif [[ "${value}" == TRUE ]]; then
      printf 't\n' >"${STATE_DIR}/dispatch-state"
    else
      printf 'f\n' >"${STATE_DIR}/dispatch-state"
    fi
    return 0
  fi
  if [[ "${query}" == *'SELECT dispatch_paused FROM deployment_control'* ]]; then
    state=$(<"${STATE_DIR}/dispatch-state")
    record_event "dispatch-readback-${state}"
    if [[ "${TEST_CASE}" == committed-unpause-readback-error && "${state}" == f && ! -e "${STATE_DIR}/readback-failed" ]]; then
      : >"${STATE_DIR}/readback-failed"
      return 9
    fi
    printf '%s\n' "${state}"
    return 0
  fi
  printf 'Unexpected db_sql query: %s\n' "${query}" >&2
  return 91
}

# Delegate to real jq except for the injected quarantine-write failure.
jq() {
  if [[ "${1:-}" == -n ]]; then
    record_event quarantine-json
    [[ "${TEST_CASE}" != quarantine-fail ]] || return 92
  fi
  command jq "$@"
}

# Record the durable quarantine rename and prove the journal still exists then.
mv() {
  local -a args=("$@")
  local destination=${args[${#args[@]}-1]}
  if [[ "${destination}" == "${STATE_DIR}/failed.json" ]]; then
    record_event quarantine-commit
    [[ -e "${STATE_DIR}/transition.json" ]] || return 93
  fi
  command mv "$@"
}

compose_stub() {
  [[ "$*" == 'up -d --no-build --pull never --force-recreate db app' ]]
}

write_db_override() {
  [[ "${1:-}" == "${previous_db_local}" ]]
}

wait_for_runtime() {
  [[ "${1:-}" == "${previous_revision}" && "${2:-}" == "${previous_db_local}" ]]
}

active_jobs() { printf '0\n'; }
stop_application_cleanly() {
  record_event "stop-app-$(<"${STATE_DIR}/dispatch-state")"
  [[ "$(<"${STATE_DIR}/dispatch-state")" == t ]]
}
stop_database_cleanly() {
  record_event "stop-db-$(<"${STATE_DIR}/dispatch-state")"
  [[ "$(<"${STATE_DIR}/dispatch-state")" == t ]]
}

# on_exit's temp cleanup is outside these regressions; the outer trap owns it.
cleanup_tmp() { :; }

setup_case() {
  TEST_CASE=$1
  CASE_DIR=$2
  STATE_DIR="${CASE_DIR}/state"
  tmp="${CASE_DIR}/tmp"
  EVENT_LOG="${CASE_DIR}/events.log"
  mkdir -p "${STATE_DIR}" "${tmp}"
  : >"${EVENT_LOG}"
  printf 'journal fixture\n' >"${STATE_DIR}/transition.json"
  printf 't\n' >"${STATE_DIR}/dispatch-state"

  EXPECTED_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  previous_revision=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  previous_app_local="thornhill-rollback-app:${previous_revision}"
  previous_db_local="thornhill-rollback-db:${previous_revision}"
  THORNHILL_APP_IMAGE=thornhill-app:target
  THORNHILL_POSTGRES_IMAGE=thornhill-postgres:target
  app_stopped=true
  db_stopped=true
  target_start_attempted=false
  had_previous_receipt=false
  changed=false
  draining=false
  rollback_ok=false
  compose=(compose_stub)
}

seed_previous_receipt() {
  had_previous_receipt=true
  printf '{"receipt":"previous"}\n' >"${tmp}/previous-receipt.json"
  printf '{"receipt":"target"}\n' >"${STATE_DIR}/deployed.json"
}

assert_events() {
  local -a expected=("$@") actual=()
  mapfile -t actual <"${EVENT_LOG}"
  if [[ "${actual[*]}" != "${expected[*]}" ]]; then
    fail "event sequence mismatch; expected [${expected[*]}], got [${actual[*]}]"
  fi
}

run_exit_hook() {
  local expected_status=$1 actual_status
  if (
    trap - EXIT INT TERM
    set +e
    bash -c 'exit "$1"' _ "${expected_status}"
    on_exit
  ) >"${CASE_DIR}/on-exit.stdout" 2>"${CASE_DIR}/on-exit.stderr"; then
    actual_status=0
  else
    actual_status=$?
  fi
  [[ "${actual_status}" == "${expected_status}" ]] ||
    fail "on_exit changed failure status: expected ${expected_status}, got ${actual_status}"
}

# A successful rollback unpauses with verified readback, writes quarantine, then
# clears the journal while preserving the original failure exit status.
setup_case recovery-success "${tmp_root}/recovery-success"
seed_previous_receipt
changed=true
draining=true
run_exit_hook 23
assert_events \
  dispatch-update-FALSE:present dispatch-readback-f:present \
  quarantine-json:present quarantine-commit:present
if [[ -e "${STATE_DIR}/transition.json" ]]; then fail 'successful recovery left the journal'; fi
if ! command jq -e --arg target "${EXPECTED_SHA}" --arg previous "${previous_revision}" \
  '.target_revision == $target and .previous_revision == $previous and .outcome == "verified-rollback"' \
  "${STATE_DIR}/failed.json" >/dev/null; then
  fail 'successful recovery did not write the expected quarantine receipt'
fi
if [[ "$(<"${STATE_DIR}/dispatch-state")" != f ]]; then fail 'successful recovery did not verify dispatch unpause'; fi
if ! cmp -s "${tmp}/previous-receipt.json" "${STATE_DIR}/deployed.json"; then
  fail 'successful recovery did not restore the previous receipt'
fi

# A failed quarantine write must retain the journal and keep the original status.
setup_case quarantine-fail "${tmp_root}/quarantine-fail"
seed_previous_receipt
changed=true
draining=true
run_exit_hook 23
assert_events \
  dispatch-update-FALSE:present dispatch-readback-f:present quarantine-json:present
if [[ ! -e "${STATE_DIR}/transition.json" ]]; then fail 'failed quarantine cleared the journal'; fi
if [[ -e "${STATE_DIR}/failed.json" ]]; then fail 'failed quarantine created a completed receipt'; fi
if ! grep -Fq 'quarantine not recorded; transition journal retained' "${CASE_DIR}/on-exit.stderr"; then
  fail 'failed quarantine did not report the retained journal'
fi

# If dispatch unpause cannot be verified during the drain-only failure path,
# on_exit reasserts pause, retains the journal, and preserves the failure status.
setup_case pause-unpause-error "${tmp_root}/pause-unpause-error"
draining=true
run_exit_hook 23
assert_events \
  dispatch-update-FALSE:present dispatch-readback-t:present \
  dispatch-update-TRUE:present dispatch-readback-t:present
if [[ ! -e "${STATE_DIR}/transition.json" ]]; then fail 'unpause verification failure cleared the journal'; fi
if [[ "$(<"${STATE_DIR}/dispatch-state")" != t ]]; then fail 'dispatch was not left paused after unpause failure'; fi
if ! grep -Fq 'dispatch state is unknown; transition journal retained' "${CASE_DIR}/on-exit.stderr"; then
  fail 'unpause verification failure was not reported'
fi

# Rollback restores the prior receipt without disturbing unrelated state.
setup_case receipt-restore "${tmp_root}/receipt-restore"
seed_previous_receipt
printf 'unrelated quarantine marker\n' >"${STATE_DIR}/failed.json"
if rollback; then
  [[ "${rollback_ok}" == true ]] || fail 'successful rollback did not set rollback_ok'
else
  fail 'rollback failed while restoring a previous receipt'
fi
if ! cmp -s "${tmp}/previous-receipt.json" "${STATE_DIR}/deployed.json"; then
  fail 'rollback did not restore the previous receipt byte-for-byte'
fi
if [[ "$(<"${STATE_DIR}/failed.json")" != 'unrelated quarantine marker' ]]; then
  fail 'rollback changed an unrelated quarantine marker'
fi
if [[ ! -e "${STATE_DIR}/transition.json" ]]; then fail 'rollback cleared the transition journal'; fi

# With no prior receipt, rollback removes only the target receipt.
setup_case receipt-remove-new "${tmp_root}/receipt-remove-new"
printf '{"receipt":"new target"}\n' >"${STATE_DIR}/deployed.json"
printf 'unrelated quarantine marker\n' >"${STATE_DIR}/failed.json"
if rollback; then
  [[ "${rollback_ok}" == true ]] || fail 'successful receipt removal did not set rollback_ok'
else
  fail 'rollback failed while removing a newly-created receipt'
fi
if [[ -e "${STATE_DIR}/deployed.json" ]]; then fail 'rollback retained the newly-created receipt'; fi
if [[ "$(<"${STATE_DIR}/failed.json")" != 'unrelated quarantine marker' ]]; then
  fail 'rollback removed or changed unrelated state'
fi
if [[ ! -e "${STATE_DIR}/transition.json" ]]; then fail 'rollback cleared the transition journal'; fi

# A failed unpause verification makes rollback fail and must not set rollback_ok.
setup_case rollback-unpause-error "${tmp_root}/rollback-unpause-error"
seed_previous_receipt
if rollback; then
  fail 'rollback reported success despite failed dispatch-unpause verification'
fi
if [[ "${rollback_ok}" != false ]]; then fail 'rollback_ok was true after unpause verification failed'; fi
if [[ ! -e "${STATE_DIR}/transition.json" ]]; then fail 'failed rollback cleared the transition journal'; fi
if ! cmp -s "${tmp}/previous-receipt.json" "${STATE_DIR}/deployed.json"; then
  fail 'failed-unpause rollback did not restore the previous receipt before failing'
fi

# A committed unpause with failed readback must be reversed before shutdown.
setup_case committed-unpause-readback-error "${tmp_root}/committed-unpause-readback-error"
seed_previous_receipt
changed=true
draining=true
target_start_attempted=true
if set_dispatch_paused FALSE; then
  fail 'injected unpause readback failure was not detected'
fi
run_exit_hook 23
if ! grep -Fq 'stop-app-t:present' "${EVENT_LOG}" || ! grep -Fq 'stop-db-t:present' "${EVENT_LOG}"; then
  fail 'rollback stopped services without first verifying paused dispatch'
fi
if [[ -e "${STATE_DIR}/transition.json" ]]; then fail 'successful rollback retained journal'; fi

if (( failures != 0 )); then
  printf 'Installer exit safety regression checks failed (%d issue(s))\n' "${failures}" >&2
  exit 1
fi
printf '%s\n' 'Installer exit safety regression checks passed'
