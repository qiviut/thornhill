#!/usr/bin/env bash
# Exercise the built image under the same hardening controls used by Compose.
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
app_image=${1:-thornhill:ci}
db_image=${2:-thornhill-postgres:ci}
suffix="${GITHUB_RUN_ID:-local}-$$"
network="thornhill-hardening-${suffix}"
db="thornhill-hardening-db-${suffix}"
app="thornhill-hardening-app-${suffix}"
db_url="postgres://thornhill:thornhill-test-only@${db}:5432/thornhill?sslmode=disable"

compose_model=$(THORNHILL_ENV_FILE="${root}/.env.example" \
  THORNHILL_APP_IMAGE="${app_image}" THORNHILL_POSTGRES_IMAGE="${db_image}" \
  docker compose --project-directory "${root}" --file "${root}/docker-compose.yml" config --format json)
jq -e '
  (.services.app | .init == true and .read_only == true and .pids_limit == 256 and
    .cap_drop == ["ALL"] and (.security_opt | index("no-new-privileges:true")) != null and
    (.tmpfs | index("/tmp:rw,noexec,nosuid,size=64m")) != null) and
  (.services.db | .init == false and .read_only == true and .pids_limit == 256 and
    .cap_drop == ["ALL"] and
    (.cap_add | sort) == (["CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID"] | sort) and
    (.security_opt | index("no-new-privileges:true")) != null and
    .stop_signal == "SIGINT" and .stop_grace_period == "30s" and
    any(.tmpfs[]; startswith("/run/postgresql:rw,noexec,nosuid,size=16m")))
' <<<"${compose_model}" >/dev/null || {
  printf 'Checked-in Compose hardening model does not match the qualified runtime policy\n' >&2
  exit 1
}

cleanup() {
  docker rm --force "$app" "$db" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail_with_logs() {
  printf '%s\n' "$1" >&2
  if docker container inspect "$app" >/dev/null 2>&1; then
    docker logs "$app" >&2 || true
  fi
  if docker container inspect "$db" >/dev/null 2>&1; then
    docker logs "$db" >&2 || true
  fi
  exit 1
}

postgres_ready() {
  # The official image briefly starts a temporary PostgreSQL server during
  # initialization, then stops it before exec'ing the final server as PID 1.
  # Requiring that process shape prevents a transient pg_isready success from
  # racing the intentional shutdown.
  docker exec "$db" sh -c \
    'test "$(cat /proc/1/comm)" = postgres && pg_isready --username thornhill --dbname thornhill' \
    >/dev/null 2>&1
}

revision=$(docker image inspect "$app_image" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}')
[[ "$revision" =~ ^[0-9a-f]{40}$ ]] || {
  printf 'Application image has an invalid revision label: %q\n' "$revision" >&2
  exit 1
}

runtime_user=$(docker image inspect "$app_image" --format '{{.Config.User}}')
uid=${runtime_user%%:*}
[[ "$uid" =~ ^[0-9]+$ && "$uid" -ne 0 ]] || {
  printf 'Application image must use a numeric non-root user, got %q\n' "$runtime_user" >&2
  exit 1
}
[[ "$(docker run --rm "$app_image" --version)" == "thornhill ${revision}" ]]

docker network create "$network" >/dev/null
docker run --detach --name "$db" --network "$network" \
  --env POSTGRES_USER=thornhill \
  --env POSTGRES_PASSWORD=thornhill-test-only \
  --env POSTGRES_DB=thornhill \
  --read-only \
  --tmpfs /var/lib/postgresql:rw,noexec,nosuid,size=512m,uid=70,gid=70,mode=1777 \
  --tmpfs /run/postgresql:rw,noexec,nosuid,size=16m,uid=70,gid=70,mode=2775 \
  --tmpfs /tmp:rw,noexec,nosuid,size=64m,mode=1777 \
  --cap-drop ALL \
  --cap-add CHOWN \
  --cap-add DAC_OVERRIDE \
  --cap-add FOWNER \
  --cap-add SETGID \
  --cap-add SETUID \
  --security-opt no-new-privileges:true \
  --pids-limit 256 \
  --stop-signal SIGINT \
  --stop-timeout 30 \
  "$db_image" >/dev/null

for _ in {1..60}; do
  if postgres_ready; then
    break
  fi
  sleep 1
done
postgres_ready || fail_with_logs 'PostgreSQL did not become ready'

db_uid=$(docker exec "$db" stat -c %u /proc/1)
db_readonly=$(docker inspect "$db" --format '{{.HostConfig.ReadonlyRootfs}}')
db_cap_drop=$(docker inspect "$db" --format '{{json .HostConfig.CapDrop}}')
db_cap_add=$(docker inspect "$db" --format '{{json .HostConfig.CapAdd}}')
db_security_opt=$(docker inspect "$db" --format '{{json .HostConfig.SecurityOpt}}')
db_pids_limit=$(docker inspect "$db" --format '{{.HostConfig.PidsLimit}}')
db_stop_signal=$(docker inspect "$db" --format '{{.Config.StopSignal}}')
db_stop_timeout=$(docker inspect "$db" --format '{{.Config.StopTimeout}}')
[[ "$db_uid" == 70 ]]
[[ "$db_readonly" == true ]]
[[ "$db_cap_drop" == *'ALL'* ]]
for capability in CHOWN DAC_OVERRIDE FOWNER SETGID SETUID; do
  [[ "$db_cap_add" == *"$capability"* ]]
done
[[ "$db_security_opt" == *'no-new-privileges'* ]]
[[ "$db_pids_limit" == 256 ]]
[[ "$db_stop_signal" == SIGINT ]]
[[ "$db_stop_timeout" == 30 ]]

docker run --detach --name "$app" --network "$network" \
  --publish 127.0.0.1::8787 \
  --env OPENAI_API_KEY=test-only-not-a-secret \
  --env "DATABASE_URL=${db_url}" \
  --env LISTEN_ADDR=:8787 \
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid,size=64m \
  --tmpfs /data:rw,noexec,nosuid,size=64m,uid=65532,gid=65532 \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --pids-limit 256 \
  "$app_image" >/dev/null

for _ in {1..30}; do
  health=$(docker inspect "$app" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}')
  case "$health" in
    healthy) break ;;
    unhealthy) fail_with_logs 'Application container became unhealthy' ;;
  esac
  sleep 1
done
[[ "$(docker inspect "$app" --format '{{.State.Health.Status}}')" == healthy ]] || fail_with_logs 'Application container did not become healthy'

readonly=$(docker inspect "$app" --format '{{.HostConfig.ReadonlyRootfs}}')
cap_drop=$(docker inspect "$app" --format '{{json .HostConfig.CapDrop}}')
security_opt=$(docker inspect "$app" --format '{{json .HostConfig.SecurityOpt}}')
app_pids_limit=$(docker inspect "$app" --format '{{.HostConfig.PidsLimit}}')
[[ "$readonly" == true ]]
[[ "$cap_drop" == *'ALL'* ]]
[[ "$security_opt" == *'no-new-privileges'* ]]
[[ "$app_pids_limit" == 256 ]]

published=$(docker port "$app" 8787/tcp)
port=${published##*:}
status=$(curl --fail --silent --show-error --max-time 5 "http://127.0.0.1:${port}/api/status")
actual_revision=$(jq -r '.source_commit // empty' <<<"$status")
versioned=$(jq -r '.versioned // false' <<<"$status")
[[ "$actual_revision" == "$revision" && "$versioned" == true ]]

docker stop "$db" >/dev/null
[[ "$(docker inspect "$db" --format '{{.State.ExitCode}}')" == 0 ]] || fail_with_logs 'PostgreSQL did not stop cleanly on SIGINT'
db_logs=$(docker logs "$db" 2>&1)
[[ "$db_logs" == *'database system is shut down'* && "$db_logs" != *'database system was not properly shut down'* ]] || \
  fail_with_logs 'PostgreSQL fast shutdown did not produce a clean checkpointed exit'

docker stop --time 10 "$app" >/dev/null
[[ "$(docker inspect "$app" --format '{{.State.ExitCode}}')" == 0 ]] || fail_with_logs 'Application did not stop cleanly on SIGTERM'

printf 'Container hardening passed: revision=%s app_user=%s app_read_only=true app_cap_drop=ALL app_pids=256 db_user=%s db_read_only=true db_cap_drop=ALL db_pids=256 db_stop=SIGINT/30s\n' \
  "$revision" "$runtime_user" "$db_uid"
