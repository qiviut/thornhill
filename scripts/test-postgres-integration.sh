#!/usr/bin/env bash
set -euo pipefail

image=${POSTGRES_TEST_IMAGE:-thornhill-postgres:ci}
suffix=$(openssl rand -hex 16)
user="u_${suffix}"
database="d_${suffix}"
schema="s_${suffix}"
password=$(openssl rand -hex 32)
container="thornhill-pgtest-${suffix}"

cleanup() {
  docker rm --force "${container}" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

docker run --detach --name "${container}" \
  --tmpfs /var/lib/postgresql:rw,noexec,nosuid,size=512m \
  --env "POSTGRES_USER=${user}" \
  --env "POSTGRES_PASSWORD=${password}" \
  --env "POSTGRES_DB=${database}" \
  --publish 127.0.0.1::5432 \
  "${image}" >/dev/null

ready=false
for _ in $(seq 1 60); do
  if docker exec --env "PGPASSWORD=${password}" "${container}" \
    psql --username "${user}" --dbname "${database}" --tuples-only --no-align \
      --command 'SELECT 1' 2>/dev/null | grep -qx 1; then
    ready=true
    break
  fi
  sleep 0.25
done
if [[ "${ready}" != true ]]; then
  docker logs "${container}" >&2
  exit 1
fi

docker exec --env "PGPASSWORD=${password}" "${container}" \
  psql --username "${user}" --dbname "${database}" \
  --set ON_ERROR_STOP=1 \
  --command "CREATE SCHEMA \"${schema}\" AUTHORIZATION \"${user}\"" >/dev/null

port=$(docker inspect "${container}" --format '{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}')
export THORNHILL_TEST_SCHEMA="${schema}"
export THORNHILL_TEST_DATABASE_URL="postgres://${user}:${password}@127.0.0.1:${port}/${database}?sslmode=disable&search_path=${schema}"

go test -tags=integration -count=1 -run '^TestPostgres(MigrationAndAtomicApprovalClaim|AttentionClaimAckAndPushOutbox)$' ./internal/store
go test -tags=integration -count=1 -run '^Test(DispatchAndResumeCommitWithQueueDelivery|CancelledDeliveryAndSynchronousAnswerCannotResurrectTerminalState)$' ./internal/dispatch
