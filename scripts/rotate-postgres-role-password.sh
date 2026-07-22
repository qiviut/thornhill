#!/usr/bin/env bash
# Verify or rotate Thornhill's persisted PostgreSQL role.
# The 64-character lowercase hexadecimal password is read from stdin.
set -euo pipefail

verify_only=false
if [[ "${1:-}" == --verify-only ]]; then
  verify_only=true
  shift
fi
container=${1:?usage: rotate-postgres-role-password.sh [--verify-only] CONTAINER}
[[ $# == 1 ]] || { printf 'Unexpected credential-helper argument\n' >&2; exit 2; }
IFS= read -r password
if [[ ! "${password}" =~ ^[0-9a-f]{64}$ ]]; then
  printf 'Database password must be exactly 64 lowercase hexadecimal characters\n' >&2
  exit 1
fi

# Validate the verification path before changing the role. Multiple named
# networks are supported; this matters when monitoring is attached separately.
mapfile -t networks < <(
  docker inspect "${container}" --format '{{json .NetworkSettings.Networks}}' | jq -r 'keys[]'
)
if (( ${#networks[@]} == 0 )); then
  printf 'Database container must belong to at least one named network\n' >&2
  exit 1
fi
image=$(docker inspect "${container}" --format '{{.Config.Image}}')
[[ -n "${image}" ]] || { printf 'Database container image is unavailable\n' >&2; exit 1; }

probe_client_path() {
  local network
  for network in "${networks[@]}"; do
    if timeout 20s docker run --rm --network "${network}" \
      --entrypoint pg_isready "${image}" \
      -h "${container}" -U thornhill -d thornhill -t 10 >/dev/null 2>&1; then
      return 0
    fi
  done
  return 1
}

verify_password() {
  local network
  for network in "${networks[@]}"; do
    # Inherit PGPASSWORD rather than expanding it into docker's argv. The
    # disposable client container is removed after every bounded probe.
    if PGPASSWORD="${password}" timeout 20s docker run --rm --network "${network}" \
      --env PGPASSWORD --entrypoint psql "${image}" \
      -v ON_ERROR_STOP=1 -h "${container}" -U thornhill -d thornhill -Atq \
      -c 'SELECT 1' 2>/dev/null | grep -qx 1; then
      return 0
    fi
  done
  return 1
}

if ! probe_client_path; then
  printf 'Disposable PostgreSQL client cannot reach the database on any named network\n' >&2
  exit 1
fi

if [[ "${verify_only}" == true ]]; then
  verify_password
  exit
fi

# A hexadecimal value is safe as an unquoted psql variable; :'<name>' performs
# SQL literal quoting. The value is supplied on stdin rather than in SQL argv.
# The PostgreSQL environment variables expand inside the database container.
# shellcheck disable=SC2016
printf '\\set db_password %s\nALTER ROLE thornhill PASSWORD :'\''db_password'\'';\n' \
  "${password}" |
  timeout 15s docker exec -i "${container}" \
    sh -c 'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atq' \
    >/dev/null

# PostgreSQL trusts its own loopback; the external container probe exercises the
# same host-authentication path used by Thornhill.
verify_password
