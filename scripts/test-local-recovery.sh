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
  --env "POSTGRES_DB=${database}" \
  --health-cmd "pg_isready -U ${user} -d ${database}" \
  --health-interval 250ms --health-timeout 5s --health-start-period 1s --health-retries 60 \
  "${image}" >/dev/null

[[ "$(docker inspect "${container}" --format '{{.HostConfig.NetworkMode}}')" == none ]]
[[ "$(docker inspect "${container}" --format '{{.HostConfig.ReadonlyRootfs}}')" == true ]]
[[ "$(docker inspect "${container}" --format '{{json .HostConfig.CapDrop}}')" == '["ALL"]' ]]
cap_add=$(docker inspect "${container}" --format '{{json .HostConfig.CapAdd}}')
[[ "${cap_add}" == *CHOWN* && "${cap_add}" == *DAC_OVERRIDE* && "${cap_add}" == *FOWNER* &&
  "${cap_add}" == *SETGID* && "${cap_add}" == *SETUID* ]]
security_opt=$(docker inspect "${container}" --format '{{json .HostConfig.SecurityOpt}}')
[[ "${security_opt}" == *no-new-privileges* ]]
[[ "$(docker inspect "${container}" --format '{{.HostConfig.PidsLimit}}')" == 256 ]]

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

tampered="${state_dir}/snapshot-tampered.dump"
cp -- "${snapshot}" "${tampered}"
jq --arg snapshot "${tampered}" '.snapshot = $snapshot | .size += 1' "${snapshot%.dump}.json" >"${tampered%.dump}.json"
if THORNHILL_RECOVERY_DIR="${state_dir}" \
  THORNHILL_RECOVERY_MAX_BYTES=67108864 \
  THORNHILL_RECOVERY_MAX_COUNT=2 \
  THORNHILL_RECOVERY_MIN_FREE_BYTES=268435456 \
  THORNHILL_RECOVERY_POSTGRES_IMAGE="${image}" \
  DB_CONTAINER="${container}" \
    scripts/local-recovery.sh restore-check "${tampered}"; then
  printf 'Recovery accepted a snapshot whose metadata size was altered\n' >&2
  exit 1
fi
