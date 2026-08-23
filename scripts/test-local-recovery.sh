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
recovery_dir="${state_dir}/recovery"
watcher=""
cleanup() {
  if [[ -n "${watcher:-}" ]]; then
    kill "${watcher}" >/dev/null 2>&1 || true
    wait "${watcher}" >/dev/null 2>&1 || true
  fi
  docker rm --force "${container}" >/dev/null 2>&1 || true
  rm -rf -- "${state_dir}"
}
trap cleanup EXIT INT TERM

recovery_restore_check() {
  THORNHILL_RECOVERY_DIR="${recovery_dir}" \
  THORNHILL_RECOVERY_MAX_BYTES=67108864 \
  THORNHILL_RECOVERY_MAX_COUNT=2 \
  THORNHILL_RECOVERY_MIN_FREE_BYTES=268435456 \
  THORNHILL_RECOVERY_POSTGRES_IMAGE="${image}" \
  DB_CONTAINER="${container}" \
    scripts/local-recovery.sh restore-check "$1"
}

expect_rejected() {
  local label=$1 candidate=$2
  if recovery_restore_check "${candidate}" >"${state_dir}/${label}.log" 2>&1; then
    printf 'Recovery accepted the invalid %s case\n' "${label}" >&2
    exit 1
  fi
}

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

THORNHILL_RECOVERY_DIR="${recovery_dir}" \
THORNHILL_RECOVERY_MAX_BYTES=67108864 \
THORNHILL_RECOVERY_MAX_COUNT=2 \
THORNHILL_RECOVERY_MIN_FREE_BYTES=268435456 \
THORNHILL_RECOVERY_POSTGRES_IMAGE="${image}" \
DB_CONTAINER="${container}" \
  scripts/local-recovery.sh snapshot
snapshot=$(find "${recovery_dir}" -maxdepth 1 -type f -name 'snapshot-*.dump' -print -quit)
[[ -n "${snapshot}" && -s "${snapshot}" ]]

restore_inspect="${state_dir}/restore-inspect.json"
restore_name_file="${state_dir}/restore-container-name"
(
  for _ in $(seq 1 200); do
    while IFS= read -r candidate; do
      [[ "${candidate}" == "${container}" ]] && continue
      [[ "${candidate}" == thornhill-recovery-* ]] || continue
      if docker inspect "${candidate}" >"${restore_inspect}" 2>/dev/null; then
        printf '%s\n' "${candidate}" >"${restore_name_file}"
        exit 0
      fi
    done < <(docker container ls --all --format '{{.Names}}')
    sleep 0.1
  done
  exit 1
) &
watcher=$!
recovery_restore_check "${snapshot}"
if ! wait "${watcher}"; then
  printf 'Recovery test did not observe the disposable restore container\n' >&2
  exit 1
fi
watcher=""
restore_container=$(<"${restore_name_file}")
jq -e --arg name "${restore_container}" '
  .[0].Name == ("/" + $name) and
  .[0].HostConfig.NetworkMode == "none" and
  .[0].HostConfig.ReadonlyRootfs == true and
  ((.[0].HostConfig.CapDrop // []) | index("ALL") != null) and
  (((["CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_SETGID", "CAP_SETUID"] - (.[0].HostConfig.CapAdd // [])) | length) == 0) and
  ((.[0].HostConfig.SecurityOpt // []) | index("no-new-privileges:true") != null) and
  .[0].HostConfig.PidsLimit == 256
' "${restore_inspect}" >/dev/null
if docker container inspect "${restore_container}" >/dev/null 2>&1; then
  printf 'Recovery left the disposable restore container behind\n' >&2
  exit 1
fi

zero_sha=$(printf '%064d' 0)
outside_dump="${state_dir}/outside.dump"
outside_size=$(stat -c %s "${snapshot}")
outside_sha=$(sha256sum "${snapshot}" | awk '{print $1}')
cp -- "${snapshot}" "${outside_dump}"
symlink_snapshot="${recovery_dir}/snapshot-symlink.dump"
ln -s -- "${outside_dump}" "${symlink_snapshot}"
jq -n \
  --arg snapshot "${symlink_snapshot}" --arg sha256 "${outside_sha}" --argjson size "${outside_size}" \
  '{version:1,snapshot:$snapshot,sha256:$sha256,size:$size}' \
  >"${recovery_dir}/snapshot-symlink.json"
expect_rejected symlink-snapshot "${symlink_snapshot}"

metadata_link_snapshot="${recovery_dir}/snapshot-metadata-link.dump"
metadata_link="${recovery_dir}/snapshot-metadata-link.json"
metadata_target="${state_dir}/outside-metadata.json"
cp -- "${snapshot}" "${metadata_link_snapshot}"
jq -n \
  --arg snapshot "${metadata_link_snapshot}" --arg sha256 "${outside_sha}" --argjson size "${outside_size}" \
  '{version:1,snapshot:$snapshot,sha256:$sha256,size:$size}' \
  >"${metadata_target}"
ln -s -- "${metadata_target}" "${metadata_link}"
expect_rejected symlink-metadata "${metadata_link_snapshot}"

checksum_tampered="${recovery_dir}/snapshot-checksum.dump"
cp -- "${snapshot}" "${checksum_tampered}"
jq -n \
  --arg snapshot "${checksum_tampered}" --arg sha256 "${zero_sha}" --argjson size "${outside_size}" \
  '{version:1,snapshot:$snapshot,sha256:$sha256,size:$size}' \
  >"${checksum_tampered%.dump}.json"
expect_rejected checksum "${checksum_tampered}"

size_tampered="${recovery_dir}/snapshot-size.dump"
cp -- "${snapshot}" "${size_tampered}"
jq -n \
  --arg snapshot "${size_tampered}" --arg sha256 "${outside_sha}" --argjson size "$((outside_size + 1))" \
  '{version:1,snapshot:$snapshot,sha256:$sha256,size:$size}' \
  >"${size_tampered%.dump}.json"
expect_rejected size "${size_tampered}"

over_budget="${recovery_dir}/snapshot-over-budget.dump"
truncate -s 67108865 "${over_budget}"
jq -n \
  --arg snapshot "${over_budget}" --arg sha256 "${zero_sha}" --argjson size 67108865 \
  '{version:1,snapshot:$snapshot,sha256:$sha256,size:$size}' \
  >"${over_budget%.dump}.json"
expect_rejected over-budget "${over_budget}"

printf 'Recovery rejection and cleanup checks passed\n'
