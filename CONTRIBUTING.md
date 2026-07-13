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
  npm ci
  npm run check
  npm run build
)
cp .env.example .env
docker compose config --quiet
```

Container and PostgreSQL integration checks also run in GitHub Actions. See [`docs/ci-security.md`](docs/ci-security.md) for the trust model.

## Security reports

Do not open a public issue for a suspected vulnerability. Follow [`SECURITY.md`](SECURITY.md).
