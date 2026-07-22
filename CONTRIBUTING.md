# Contributing

Thank you for helping improve Thornhill.

## Before opening a pull request

- Start from an up-to-date `main` branch and keep the change focused.
- Add or update tests and documentation with every behavior change.
- Never commit credentials, personal data, private hostnames, deployment URLs, local absolute paths, database dumps, logs, or generated runtime state.
- Keep pull-request code secretless. Tests must not call live OpenAI, Hermes, Tailnet, or deployment services.
- Preserve fail-closed approval, interruption, and recovery semantics.

Run the same core checks used by CI:

```bash
scripts/check-ci-policy.sh
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
FUZZTIME=5s scripts/test-fuzz.sh
go test -tags=integration -count=1 -run '^TestProviderProcessConformance$' ./internal/dummyopenai
(
  cd web
  npm ci --ignore-scripts
  npm run check
  npm run lint
  npm test
  npm run build
  npm audit --audit-level=high
)
THORNHILL_DB_PASSWORD="$(printf '%064d' 0)" \
  THORNHILL_ENV_FILE=.env.example docker compose config --quiet
```

Container and PostgreSQL integration checks also run in GitHub Actions. See [`docs/ci-security.md`](docs/ci-security.md) for the trust model.

## Security reports

Do not open a public issue for a suspected vulnerability. Follow [`SECURITY.md`](SECURITY.md).
