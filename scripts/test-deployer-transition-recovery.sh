#!/usr/bin/env bash
# Exercise credential-transition recovery without Docker, GitHub, or live services.
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
cleanup() {
  rm -rf "${tmp}"
}
trap cleanup EXIT

repo=${tmp}/repo
mkdir -p "${repo}"
git -C "${root}" checkout-index --all --prefix="${repo}/"
git -C "${repo}" init --quiet
git -C "${repo}" config user.name 'Thornhill recovery test'
git -C "${repo}" config user.email 'recovery@example.invalid'
git -C "${repo}" add --all
git -C "${repo}" commit --quiet -m base
previous_revision=$(git -C "${repo}" rev-parse HEAD)
printf 'recovery target\n' >"${repo}/.recovery-target"
git -C "${repo}" add .recovery-target
git -C "${repo}" commit --quiet -m target
revision=$(git -C "${repo}" rev-parse HEAD)

state=${tmp}/fake-state
bin=${tmp}/bin
mkdir -p "${state}" "${bin}"
printf '%s\n' true >"${state}/app-present"
printf '%s\n' true >"${state}/app-running"
printf '%s\n' 0 >"${state}/app-exit-code"
printf '%s\n' true >"${state}/db-running"
printf '%s\n' false >"${state}/dispatch-paused"
printf '%s\n' "${previous_revision}" >"${state}/revision"
printf '%s\n' previous-app >"${state}/app-image"
printf '%s\n' previous-db >"${state}/db-image"
: >"${state}/argv.log"
: >"${state}/events.log"

cat >"${bin}/curl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
url=${!#}
if [[ $(<"${FAKE_DOCKER_STATE}/app-running") != true ]]; then
  exit 7
fi
if [[ "${url}" == *'/api/status'* ]]; then
  if [[ "${FAIL_PUBLIC_STATUS:-0}" == 1 && "${url}" == https://* ]]; then
    printf '{"status":"ok","versioned":true,"source_commit":"%s"}\n' "${PREVIOUS_REVISION}"
  else
    printf '{"status":"ok","versioned":true,"source_commit":"%s"}\n' "$(<"${FAKE_DOCKER_STATE}/revision")"
  fi
else
  printf '<!doctype html>\n'
fi
SH

cat >"${bin}/gh" <<'SH'
#!/usr/bin/env bash
printf 'gh must not run before transition recovery\n' >&2
exit 97
SH

cat >"${bin}/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
state=${FAKE_DOCKER_STATE}
printf '%q ' "$@" >>"${state}/argv.log"
printf '\n' >>"${state}/argv.log"
command=${1:?}
shift
case "${command}" in
  image)
    [[ "${1:-}" == inspect ]]
    if [[ " $* " == *' --format '* ]]; then
      cat "${state}/revision"
    fi
    exit 0
    ;;
  inspect)
    container=${1:?}
    if [[ "${container}" == *-app-1 && $(<"${state}/app-present") != true ]]; then
      exit 1
    fi
    shift
    format=''
    if [[ "${1:-}" == --format ]]; then
      format=${2:-}
    fi
    case "${format}" in
      '{{json .NetworkSettings.Networks}}') printf '{"fake-network":{}}\n' ;;
      '{{json .Config.Env}}') printf '["THORNHILL_DB_PASSWORD=%s"]\n' "${TEST_PASSWORD}" ;;
      '{{.Config.Image}}')
        if [[ "${container}" == *-app-1 ]]; then
          cat "${state}/app-image"
        else
          cat "${state}/db-image"
        fi
        ;;
      '{{.Image}}') cat "${state}/app-image" ;;
      '{{.State.Health.Status}}') printf 'healthy\n' ;;
      '{{.HostConfig.ReadonlyRootfs}}') printf 'true\n' ;;
      '{{json .HostConfig.SecurityOpt}}') printf '["no-new-privileges:true"]\n' ;;
      '{{.HostConfig.PidsLimit}}') printf '256\n' ;;
      '{{ index .Config.Labels "org.opencontainers.image.revision" }}')
        if [[ "${container}" == *-app-1 ]]; then
          cat "${state}/revision"
        else
          printf '%s\n' "${TARGET_REVISION}"
        fi
        ;;
      '{{.Config.User}}') printf '70\n' ;;
      '{{json .HostConfig.CapDrop}}') printf '["ALL"]\n' ;;
      '{{.State.Running}}')
        if [[ "${container}" == *-app-1 ]]; then cat "${state}/app-running"; else cat "${state}/db-running"; fi
        ;;
      '{{.State.Running}} {{.State.Restarting}} {{.State.ExitCode}}')
        if [[ "${container}" == *-app-1 ]]; then
          printf '%s false %s\n' "$(<"${state}/app-running")" "$(<"${state}/app-exit-code")"
        else
          printf '%s false 0\n' "$(<"${state}/db-running")"
        fi
        ;;
      '{{.State.Running}} {{.State.Restarting}}')
        printf '%s false\n' "$(<"${state}/app-running")"
        ;;
      *) printf 'unsupported docker inspect format: %s\n' "${format}" >&2; exit 2 ;;
    esac
    ;;
  info)
    exit 0
    ;;
  container)
    [[ "${1:-}" == inspect ]]
    [[ $(<"${state}/app-present") == true ]]
    ;;
  run)
    printf '1\n'
    ;;
  stop)
    container=${!#}
    if [[ "${container}" == *-app-1 ]]; then
      printf '%s\n' true >"${state}/app-present"
      printf '%s\n' false >"${state}/app-running"
      printf '%s\n' 0 >"${state}/app-exit-code"
      printf 'stop:app\n' >>"${state}/events.log"
    else
      printf '%s\n' false >"${state}/db-running"
    fi
    ;;
  start)
    printf '%s\n' true >"${state}/db-running"
    ;;
  update)
    ;;
  exec)
    args=" $* "
    if [[ "${args}" == *' /app/thornhill --version '* ]]; then
      printf 'thornhill %s\n' "$(<"${state}/revision")"
      exit 0
    fi
    if [[ "${args}" == *' stat -c %u /proc/1 '* ]]; then
      printf '70\n'
      exit 0
    fi
    if [[ "${args}" == *' --detach '* ]]; then
      printf '%s\n' false >"${state}/db-running"
      exit 0
    fi
    if [[ "${args}" == *' --user 70:70 '* ]]; then
      exit 0
    fi
    sql=$(cat)
    if [[ "${sql}" == *'UPDATE deployment_control SET dispatch_paused=TRUE'* ]]; then
      printf '%s\n' true >"${state}/dispatch-paused"
      printf 'pause:true\n' >>"${state}/events.log"
    elif [[ "${sql}" == *'UPDATE deployment_control SET dispatch_paused=FALSE'* ]]; then
      printf '%s\n' false >"${state}/dispatch-paused"
    elif [[ "${sql}" == *'SELECT dispatch_paused FROM deployment_control'* ]]; then
      if [[ $(<"${state}/dispatch-paused") == true ]]; then printf 't\n'; else printf 'f\n'; fi
    elif [[ "${sql}" == *'SELECT count(*)'* ]]; then
      printf '0\n'
    elif [[ "${sql}" == *'SELECT 1'* ]]; then
      printf '1\n'
    fi
    ;;
  wait)
    printf '0\n'
    ;;
  compose)
    args=" $* "
    if [[ "${args}" == *' up '* ]]; then
      printf '%s\n' true >"${state}/db-running"
      if [[ "${args}" == *' app'* ]]; then
        printf '%s\n' true >"${state}/app-present"
        printf '%s\n' true >"${state}/app-running"
        printf '%s\n' 0 >"${state}/app-exit-code"
        if [[ "${THORNHILL_APP_IMAGE:-}" == previous-app ]]; then
          printf '%s\n' "${PREVIOUS_REVISION}" >"${state}/revision"
          printf '%s\n' previous-app >"${state}/app-image"
        else
          printf '%s\n' "${TARGET_REVISION}" >"${state}/revision"
          printf '%s\n' target-app >"${state}/app-image"
        fi
      fi
      printf '%s\n' "${THORNHILL_POSTGRES_IMAGE:-target-db}" >"${state}/db-image"
    fi
    ;;
  *) printf 'unsupported docker command: %s\n' "${command}" >&2; exit 2 ;;
esac
SH
chmod 0755 "${bin}/curl" "${bin}/gh" "${bin}/docker"

password=$(printf '%064d' 0)
printf 'OPENAI_API_KEY=test-only\nTHORNHILL_DB_PASSWORD=%s\n' "${password}" >"${repo}/.env"
chmod 0600 "${repo}/.env"
password_sha256=$(printf '%s' "${password}" | sha256sum)
password_sha256=${password_sha256%% *}
mkdir -p "${repo}/state"

write_journal() {
  local phase=$1 credential_model=${2:-previous}
  jq -n \
    --arg phase "${phase}" --arg credential_model "${credential_model}" \
    --arg target_revision "${revision}" --arg target_image target-app --arg target_db_image target-db \
    --arg previous_revision "${previous_revision}" --arg previous_image previous-app --arg previous_db_image previous-db \
    --arg ci_run_id 123 --arg ci_url https://example.invalid/actions/runs/123 \
    --arg password_sha256 "${password_sha256}" \
    '{version:2,phase:$phase,credential_model:$credential_model,
      target_revision:$target_revision,target_image:$target_image,target_db_image:$target_db_image,
      previous_revision:$previous_revision,previous_image:$previous_image,previous_db_image:$previous_db_image,
      ci_run_id:$ci_run_id,ci_url:$ci_url,password_sha256:$password_sha256}' \
    >"${repo}/state/credential-transition.json"
  chmod 0600 "${repo}/state/credential-transition.json"
}

reset_previous_runtime() {
  local present=${1:-true} running=${2:-true} exit_code=${3:-0}
  printf '%s\n' "${present}" >"${state}/app-present"
  printf '%s\n' "${running}" >"${state}/app-running"
  printf '%s\n' "${exit_code}" >"${state}/app-exit-code"
  printf '%s\n' true >"${state}/db-running"
  printf '%s\n' false >"${state}/dispatch-paused"
  printf '%s\n' "${previous_revision}" >"${state}/revision"
  printf '%s\n' previous-app >"${state}/app-image"
  printf '%s\n' previous-db >"${state}/db-image"
  : >"${state}/argv.log"
  : >"${state}/events.log"
  rm -f "${repo}/state/deployed.json" "${repo}/state/failed.json"
}

run_recovery() {
  local output=$1
  shift
  env PATH="${bin}:${PATH}" FAKE_DOCKER_STATE="${state}" \
    TARGET_REVISION="${revision}" PREVIOUS_REVISION="${previous_revision}" TEST_PASSWORD="${password}" \
    STATE_DIR="${repo}/state" PROJECT_NAME=thornhill-test TIMEOUT_SECONDS=2 \
    PUBLIC_APP_URL=https://thornhill.example.invalid/ \
    PUBLIC_STATUS_URL=https://thornhill.example.invalid/api/status \
    "$@" "${repo}/scripts/deploy-passed-main.sh" >"${output}" 2>&1
}

assert_no_password_argv() {
  if grep -Fq "${password}" "${state}/argv.log"; then
    printf 'Database password leaked into a process argument\n' >&2
    exit 1
  fi
}

# A correct container label cannot mask a broken or mismatched public status.
reset_previous_runtime true true 0
write_journal credential_rotation_pending
if run_recovery "${tmp}/endpoint-failure.output" FAIL_PUBLIC_STATUS=1; then
  printf 'Recovery accepted a mismatched public status endpoint\n' >&2
  exit 1
fi
[[ ! -e "${repo}/state/deployed.json" ]]
if grep -Fq "Recovered interrupted credential transition to ${revision}" "${tmp}/endpoint-failure.output"; then
  printf 'Mismatched public status was reported as recovered\n' >&2
  exit 1
fi
jq -e '.reason | contains("rollback_ok=")' "${repo}/state/failed.json" >/dev/null
assert_no_password_argv

# The normal interrupted-rotation path must pause dispatch before stopping app.
reset_previous_runtime true true 0
write_journal credential_rotation_pending
if ! run_recovery "${tmp}/output"; then
  printf 'Recovery harness failed:\n%s\n' "$(<"${tmp}/output")" >&2
  exit 1
fi
[[ ! -e "${repo}/state/credential-transition.json" ]]
[[ $(<"${state}/dispatch-paused") == false ]]
pause_line=$(grep -n -m1 '^pause:true$' "${state}/events.log" | cut -d: -f1)
stop_line=$(grep -n -m1 '^stop:app$' "${state}/events.log" | cut -d: -f1)
[[ -n "${pause_line}" && -n "${stop_line}" && "${pause_line}" -lt "${stop_line}" ]]
jq -e --arg revision "${revision}" '.source_commit == $revision and .ci_run_id == 123' \
  "${repo}/state/deployed.json" >/dev/null
assert_no_password_argv
grep -Fq "Recovered interrupted credential transition to ${revision}" "${tmp}/output"

# Recovery is idempotent if force-recreate was interrupted after removing app.
reset_previous_runtime false false 0
write_journal target_start_pending journaled
run_recovery "${tmp}/missing-app.output"
[[ ! -e "${repo}/state/credential-transition.json" ]]
[[ $(<"${state}/revision") == "${revision}" ]]

# A stable exited candidate is stoppable even when its prior exit was nonzero.
reset_previous_runtime true false 42
write_journal target_started journaled
run_recovery "${tmp}/exited-app.output"
[[ ! -e "${repo}/state/credential-transition.json" ]]
[[ $(<"${state}/revision") == "${revision}" ]]

# Simulate power loss after rollback verification/unpause marker persistence.
reset_previous_runtime true true 0
printf '%s\n' true >"${state}/dispatch-paused"
write_journal rollback_verified journaled
run_recovery "${tmp}/rollback-marker.output"
[[ ! -e "${repo}/state/credential-transition.json" ]]
[[ $(<"${state}/dispatch-paused") == false ]]
jq -e --arg revision "${revision}" '
  .source_commit == $revision and
  (.reason | contains("verified rollback recovered"))
' "${repo}/state/failed.json" >/dev/null
if grep -Eq '^compose ' "${state}/argv.log"; then
  printf 'Rollback completion recovery unexpectedly recreated services\n' >&2
  exit 1
fi
grep -Fq "Recovered verified rollback to ${previous_revision}" "${tmp}/rollback-marker.output"
assert_no_password_argv

printf 'Deployer transition recovery passed: endpoint, missing/exited app, rollback marker, %s -> %s\n' \
  "${previous_revision}" "${revision}"
