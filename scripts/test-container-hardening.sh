#!/usr/bin/env bash
# Exercise the built image under the same hardening controls used by Compose.
set -euo pipefail

app_image=${1:-thornhill:ci}
db_image=${2:-thornhill-postgres:ci}
suffix="${GITHUB_RUN_ID:-local}-$$"
network="thornhill-hardening-${suffix}"
db="thornhill-hardening-db-${suffix}"
app="thornhill-hardening-app-${suffix}"
db_url="postgres://thornhill:thornhill-test-only@${db}:5432/thornhill?sslmode=disable"

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
  # Requiring PID 1 to be postgres prevents a transient pg_isready success from
  # racing that intentional shutdown.
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
  "$db_image" >/dev/null

for _ in {1..60}; do
  if postgres_ready; then
    break
  fi
  sleep 1
done
postgres_ready || fail_with_logs 'PostgreSQL did not become ready'

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
[[ "$readonly" == true ]]
[[ "$cap_drop" == *'ALL'* ]]
[[ "$security_opt" == *'no-new-privileges'* ]]

published=$(docker port "$app" 8787/tcp)
port=${published##*:}
status=$(curl --fail --silent --show-error --max-time 5 "http://127.0.0.1:${port}/api/status")
actual_revision=$(jq -r '.source_commit // empty' <<<"$status")
versioned=$(jq -r '.versioned // false' <<<"$status")
[[ "$actual_revision" == "$revision" && "$versioned" == true ]]

docker stop --time 10 "$app" >/dev/null
[[ "$(docker inspect "$app" --format '{{.State.ExitCode}}')" == 0 ]] || fail_with_logs 'Application did not stop cleanly on SIGTERM'

printf 'Container hardening passed: revision=%s user=%s health=healthy read_only=true cap_drop=ALL\n' "$revision" "$runtime_user"
