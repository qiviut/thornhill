#!/usr/bin/env bash
# Bounded host-local PostgreSQL recovery snapshots.
set -euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
PROJECT_NAME=${PROJECT_NAME:-thornhill}
DB_CONTAINER=${DB_CONTAINER:-${PROJECT_NAME}-db-1}
STATE_DIR=${STATE_DIR:-${HOME}/.local/state/thornhill-ci-deploy}
RECOVERY_DIR=${THORNHILL_RECOVERY_DIR:-${STATE_DIR}/recovery}
MAX_BYTES=${THORNHILL_RECOVERY_MAX_BYTES:-1073741824}
MAX_COUNT=${THORNHILL_RECOVERY_MAX_COUNT:-2}
MIN_FREE_BYTES=${THORNHILL_RECOVERY_MIN_FREE_BYTES:-536870912}

fail() {
  printf 'Local recovery: %s\n' "$1" >&2
  exit 1
}

validate_decimal() {
  local name=$1 value=$2 minimum=$3 maximum=$4
  [[ "${value}" =~ ^[0-9]+$ ]] || fail "${name} must be a decimal integer"
  (( value >= minimum && value <= maximum )) ||
    fail "${name} must be between ${minimum} and ${maximum}"
}

validate_decimal THORNHILL_RECOVERY_MAX_BYTES "${MAX_BYTES}" 67108864 10737418240
validate_decimal THORNHILL_RECOVERY_MAX_COUNT "${MAX_COUNT}" 1 3
validate_decimal THORNHILL_RECOVERY_MIN_FREE_BYTES "${MIN_FREE_BYTES}" 268435456 10737418240

mkdir -p "${RECOVERY_DIR}"
chmod 0700 "${RECOVERY_DIR}"
RECOVERY_DIR=$(realpath -e -- "${RECOVERY_DIR}") || fail "recovery directory cannot be canonicalized"
exec 9>"${RECOVERY_DIR}/recovery.lock"
flock -n 9 || fail "another recovery operation is running"

fsync_path() {
  python3 - "$1" <<'PY'
import os
import sys

path = sys.argv[1]
flags = os.O_RDONLY | (getattr(os, "O_DIRECTORY", 0) if os.path.isdir(path) else 0)
fd = os.open(path, flags)
try:
    os.fsync(fd)
finally:
    os.close(fd)
PY
}

free_bytes() {
  df -P -B1 "${RECOVERY_DIR}" | awk 'NR == 2 {print $4}'
}

snapshot_files() {
  find "${RECOVERY_DIR}" -maxdepth 1 -type f -name 'snapshot-*.dump' -printf '%T@ %p\n' |
    sort -rn | cut -d' ' -f2-
}

prune_snapshots() {
  local index=0 path
  while IFS= read -r path; do
    [[ -n "${path}" ]] || continue
    index=$((index + 1))
    if (( index > MAX_COUNT )); then
      rm -f -- "${path}" "${path%.dump}.json"
    fi
  done < <(snapshot_files)
  fsync_path "${RECOVERY_DIR}"
}

snapshot() {
  local available temporary output created sha size metadata source_commit
  available=$(free_bytes)
  [[ "${available}" =~ ^[0-9]+$ ]] || fail "could not determine free space"
  (( available >= MAX_BYTES + MIN_FREE_BYTES )) ||
    fail "insufficient free space for the configured recovery budget"
  docker inspect "${DB_CONTAINER}" >/dev/null 2>&1 || fail "database container is missing"
  [[ "$(docker inspect "${DB_CONTAINER}" --format '{{.State.Running}}')" == true ]] ||
    fail "database container is not running"
  [[ "$(docker inspect "${DB_CONTAINER}" --format '{{.State.Health.Status}}')" == healthy ]] ||
    fail "database container is not healthy"

  temporary=$(mktemp "${RECOVERY_DIR}/.snapshot.XXXXXX")
  cleanup_temporary() { rm -f -- "${temporary}"; }
  trap cleanup_temporary EXIT
  if ! docker exec -i "${DB_CONTAINER}" sh -ec \
    'exec pg_dump --format=custom --compress=6 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' |
    python3 "${ROOT}/scripts/capped-copy.py" "${temporary}" "${MAX_BYTES}"; then
    fail "pg_dump did not complete within the local recovery budget"
  fi
  docker exec -i "${DB_CONTAINER}" sh -ec 'exec pg_restore --list' <"${temporary}" >/dev/null ||
    fail "created recovery archive failed pg_restore listing"
  size=$(stat -c %s "${temporary}")
  sha=$(sha256sum "${temporary}" | awk '{print $1}')
  created=$(date -u +%Y%m%dT%H%M%SZ)
  output="${RECOVERY_DIR}/snapshot-${created}-$$.dump"
  mv -- "${temporary}" "${output}"
  chmod 0600 "${output}"
  source_commit=${THORNHILL_RECOVERY_SOURCE_COMMIT:-unknown}
  metadata="${output%.dump}.json"
  jq -n \
    --arg source_commit "${source_commit}" \
    --arg snapshot "${output}" \
    --arg sha256 "${sha}" \
    --arg size "${size}" \
    --arg created_at "${created}" \
    '{version:1,source_commit:$source_commit,snapshot:$snapshot,sha256:$sha256,size:($size|tonumber),created_at:$created_at}' \
    >"${metadata}"
  chmod 0600 "${metadata}"
  fsync_path "${output}"
  fsync_path "${metadata}"
  fsync_path "${RECOVERY_DIR}"
  trap - EXIT
  prune_snapshots
  printf 'Local recovery snapshot: %s (%s bytes)\n' "${output}" "${size}"
}

latest_snapshot() {
  snapshot_files | head -n 1
}

verify_snapshot() {
  local snapshot=$1 metadata canonical_snapshot canonical_metadata expected_snapshot expected_sha expected_size actual_size actual_sha
  [[ -f "${snapshot}" ]] || fail "recovery snapshot is missing"
  canonical_snapshot=$(realpath -e -- "${snapshot}") || fail "recovery snapshot cannot be canonicalized"
  [[ "${canonical_snapshot}" == "${snapshot}" ]] ||
    fail "recovery snapshot path must be canonical and cannot be a symlink"
  [[ "$(dirname -- "${canonical_snapshot}")" == "${RECOVERY_DIR}" ]] ||
    fail "recovery snapshot must be directly inside the configured recovery directory"
  metadata="${snapshot%.dump}.json"
  [[ -f "${metadata}" ]] || fail "snapshot metadata is missing"
  canonical_metadata=$(realpath -e -- "${metadata}") || fail "snapshot metadata cannot be canonicalized"
  [[ "${canonical_metadata}" == "${metadata}" ]] ||
    fail "snapshot metadata path must be canonical and cannot be a symlink"
  [[ "$(dirname -- "${canonical_metadata}")" == "${RECOVERY_DIR}" ]] ||
    fail "snapshot metadata must be directly inside the configured recovery directory"
  jq -e --arg snapshot "${snapshot}" '
    .version == 1 and .snapshot == $snapshot and
    (.sha256 | test("^[0-9a-f]{64}$")) and
    (.size | type == "number" and floor == . and . >= 1)
  ' "${metadata}" >/dev/null || fail "snapshot metadata is malformed"
  expected_snapshot=$(jq -r .snapshot "${metadata}")
  expected_sha=$(jq -r .sha256 "${metadata}")
  expected_size=$(jq -r .size "${metadata}")
  [[ "${expected_snapshot}" == "${snapshot}" ]] || fail "snapshot metadata path does not match"
  actual_size=$(stat -c %s "${snapshot}")
  [[ "${actual_size}" == "${expected_size}" ]] || fail "snapshot size does not match metadata"
  (( actual_size <= MAX_BYTES )) || fail "snapshot exceeds the configured recovery budget"
  actual_sha=$(sha256sum "${snapshot}" | awk '{print $1}')
  [[ "${actual_sha}" == "${expected_sha}" ]] || fail "snapshot checksum does not match metadata"
}

restore_check() {
  local snapshot=${1:-$(latest_snapshot)} image suffix user password container ready
  [[ -n "${snapshot}" ]] || fail "no recovery snapshot exists"
  case "${snapshot}" in
    "${RECOVERY_DIR}"/snapshot-*.dump) ;;
    *) fail "snapshot must be inside the configured recovery directory" ;;
  esac
  verify_snapshot "${snapshot}"
  docker inspect "${DB_CONTAINER}" >/dev/null 2>&1 || fail "database container is missing"
  image=${THORNHILL_RECOVERY_POSTGRES_IMAGE:-$(docker inspect "${DB_CONTAINER}" --format '{{.Config.Image}}')}
  suffix=$(openssl rand -hex 16)
  user="r_${suffix}"
  password=$(openssl rand -hex 32)
  container="thornhill-recovery-${suffix}"
  cleanup_restore() {
    for _ in 1 2 3; do
      docker info >/dev/null 2>&1 || return 1
      if docker container inspect "${container}" >/dev/null 2>&1; then
        docker container rm --force "${container}" >/dev/null 2>&1 || true
      fi
      if ! docker container inspect "${container}" >/dev/null 2>&1; then
        docker info >/dev/null 2>&1 || return 1
        if ! docker container inspect "${container}" >/dev/null 2>&1; then
          return 0
        fi
      fi
      sleep 1
    done
    return 1
  }
  on_restore_exit() {
    local status=$?
    trap '' INT TERM
    if ! cleanup_restore; then
      printf 'Local recovery: disposable restore cleanup could not be verified\n' >&2
      status=1
    fi
    trap - EXIT INT TERM
    exit "${status}"
  }
  trap on_restore_exit EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  docker run --rm --detach --name "${container}" \
    --network none \
    --read-only \
    --cap-drop ALL \
    --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add SETGID --cap-add SETUID \
    --security-opt no-new-privileges:true \
    --pids-limit 256 \
    --tmpfs /var/lib/postgresql:rw,noexec,nosuid,size=512m \
    --tmpfs /run/postgresql:rw,noexec,nosuid,size=16m \
    --tmpfs /tmp:rw,noexec,nosuid,size=64m \
    --env "POSTGRES_USER=${user}" \
    --env "POSTGRES_PASSWORD=${password}" \
    --env POSTGRES_DB=recovery \
    "${image}" >/dev/null
  ready=false
  for _ in $(seq 1 60); do
    if docker exec --env "PGPASSWORD=${password}" "${container}" \
      psql --username "${user}" --dbname recovery --tuples-only --no-align \
      --command 'SELECT 1' 2>/dev/null | grep -qx 1; then
      ready=true
      break
    fi
    sleep 0.25
  done
  [[ "${ready}" == true ]] || fail "disposable restore database did not become ready"
  docker exec --env "PGPASSWORD=${password}" "${container}" \
    createdb --username "${user}" --maintenance-db recovery restored
  docker exec --env "PGPASSWORD=${password}" -i "${container}" \
    pg_restore --exit-on-error --no-owner --no-privileges \
    --username "${user}" --dbname restored <"${snapshot}" >/dev/null
  docker exec --env "PGPASSWORD=${password}" "${container}" \
    psql --username "${user}" --dbname restored --tuples-only --no-align \
    --command 'SELECT 1' | grep -qx 1
  cleanup_restore || fail "disposable restore cleanup could not be verified"
  trap - EXIT INT TERM
  printf 'Local recovery restore check passed: %s\n' "${snapshot}"
}

case "${1:-}" in
  snapshot) snapshot ;;
  restore-check) restore_check "${2:-}" ;;
  *)
    printf 'usage: %s {snapshot|restore-check [snapshot]}\n' "${BASH_SOURCE[0]}" >&2
    exit 64
    ;;
esac
