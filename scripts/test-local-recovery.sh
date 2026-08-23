#!/usr/bin/env bash
# Exercise bounded dump, archive validation, pruning, and disposable restore.
set -euo pipefail

image=${POSTGRES_TEST_IMAGE:-thornhill-postgres:ci}
suffix=$(openssl rand -hex 16)
user="u_${suffix}"
database="d_${suffix}"
password=$(openssl rand -hex 32)
container="thornhill-recovery-test-${suffix}"
state_dir=$(mktemp -d)
cleanup() {
  docker rm --force "${container}" >/dev/null 2>&1 || true
  rm -rf -- "${state_dir}"
}
trap cleanup EXIT INT TERM

docker run --detach --name "${container}" \
  --tmpfs /var/lib/postgresql:rw,noexec,nosuid,size=512m \
  --tmpfs /run/postgresql:rw,noexec,nosuid,size=16m \
  --tmpfs /tmp:rw,noexec,nosuid,size=64m \
  --env "POSTGRES_USER=${user}" \
  --env "POSTGRES_PASSWORD=${password}" \
  --env "POSTGRES_DB=${database}" \
  --health-cmd "pg_isready -U ${user} -d ${database}" \
  --health-interval 250ms --health-timeout 5s --health-start-period 1s --health-retries 60 \
  "${image}" >/dev/null

for _ in $(seq 1 60); do
  if [[ "$(docker inspect "${container}" --format '{{.State.Health.Status}}')" == healthy ]]; then
    break
  fi
  sleep 0.25
done
[[ "$(docker inspect "${container}" --format '{{.State.Health.Status}}')" == healthy ]]

THORNHILL_RECOVERY_DIR="${state_dir}" \
THORNHILL_RECOVERY_MAX_BYTES=67108864 \
THORNHILL_RECOVERY_MAX_COUNT=2 \
THORNHILL_RECOVERY_MIN_FREE_BYTES=268435456 \
THORNHILL_RECOVERY_POSTGRES_IMAGE="${image}" \
DB_CONTAINER="${container}" \
  scripts/local-recovery.sh snapshot
snapshot=$(find "${state_dir}" -maxdepth 1 -type f -name 'snapshot-*.dump' -print -quit)
[[ -n "${snapshot}" && -s "${snapshot}" ]]
THORNHILL_RECOVERY_DIR="${state_dir}" \
THORNHILL_RECOVERY_MAX_BYTES=67108864 \
THORNHILL_RECOVERY_MAX_COUNT=2 \
THORNHILL_RECOVERY_MIN_FREE_BYTES=268435456 \
THORNHILL_RECOVERY_POSTGRES_IMAGE="${image}" \
DB_CONTAINER="${container}" \
  scripts/local-recovery.sh restore-check "${snapshot}"
