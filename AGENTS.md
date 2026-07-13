# Thornhill agent guide

## Project

Thornhill is a tailnet-deployed, single-operator voice desk for Hermes Agent work. The OpenAI API key stays server-side. Treat browser-origin policy, webhook inputs, and external agent responses as security boundaries.

## Working conventions

- Track work with `br`; `.beads/issues.jsonl` is the committed source of truth. Run `br sync --flush-only` after mutations.
- Before editing, use Agent Mail file reservations when more than one agent is active.
- Do not commit `.env`, API keys, generated web assets, `web/node_modules`, `prebaked`, or `.beads/beads.db*`.
- The default Compose exposure is loopback. Set `THORNHILL_BIND_ADDR` only to an intentional Tailscale address; do not publish the service to the public internet.
- Keep WebSocket origins same-origin by default. Add narrow `ALLOWED_ORIGINS` entries only for deliberate development hosts such as `localhost:5173`.

## Verification

Run before every commit:

```sh
gofmt -w .
go vet ./...
go test -race ./...
(cd web && npm ci && npm run check && npm run build)
docker build --pull --tag thornhill:local .
```

GitHub Actions repeats these checks on pushes and pull requests. Dependabot maintains Go, npm, Docker, and Actions dependencies.
