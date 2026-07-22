# Thornhill agent guide

## Project

Thornhill is a tailnet-deployed, single-operator voice desk for Hermes Agent work. The OpenAI API key stays server-side. Treat browser-origin policy, webhook inputs, and external agent responses as security boundaries.

## Working conventions

- Track work with `br`; `.beads/issues.jsonl` is the committed source of truth. Run `br sync --flush-only` after mutations.
- Before editing, use Agent Mail file reservations when more than one agent is active.
- Do not commit `.env`, API keys, generated web assets, `web/node_modules`, `prebaked`, or `.beads/beads.db*`.
- The default Compose exposure is loopback. Set `THORNHILL_BIND_ADDR` only to an intentional Tailscale address; do not publish the service to the public internet.
- Keep WebSocket origins same-origin by default. Add narrow `ALLOWED_ORIGINS` entries only for deliberate development hosts such as `localhost:5173`.
- Before changing job/run concurrency, approval behavior, protocol boundary types,
  SQL query bounds, or deployment sequencing, read
  [`docs/architecture/reliability-boundaries.md`](docs/architecture/reliability-boundaries.md).
  It records the invariants that preserve explicit consent, durable ownership,
  complete operator visibility, and rollback safety.

## Verification

Run before every commit:

```sh
gofmt -w .
go vet ./...
go test -race ./...
go tool staticcheck ./...
go tool govulncheck ./...
go tool actionlint .github/workflows/*.yml
(cd web && npm ci --ignore-scripts && npm run check && npm run lint && npm run build && npm audit --audit-level=high)
docker buildx build --check .
docker buildx build --pull --load --build-arg THORNHILL_REVISION=0123456789abcdef0123456789abcdef01234567 --tag thornhill:local .
docker buildx build --pull --load --file Dockerfile.postgres --tag thornhill-postgres:ci .
scripts/test-container-hardening.sh thornhill:local thornhill-postgres:ci
scripts/run-security-scans.sh thornhill:local thornhill-postgres:ci
```

GitHub Actions repeats these checks on pushes and pull requests. Dependabot maintains Go modules and tool rules, npm/Biome rules, Dockerfile and Compose images, scanner rule engines, and Actions dependencies. See `docs/container-security.md` for scope and exceptions.
