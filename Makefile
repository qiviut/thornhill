.DEFAULT_GOAL := build

.PHONY: run test static security web build up down logs

run: ## run the gateway (expects DATABASE_URL + OPENAI_API_KEY in env)
	go run ./cmd/thornhill

test:
	go vet ./... && go test ./...

static: ## run maintained Go, workflow, and web static analysis
	go tool staticcheck ./...
	go tool actionlint .github/workflows/*.yml
	cd web && npm run check && npm run lint

security: ## run Go vulnerability checks plus Trivy, Hadolint, and ShellCheck
	go tool govulncheck ./...
	scripts/run-security-scans.sh

web: ## dev UI with HMR, proxying to :8787
	cd web && npm run dev

build:
	cd web && npm run build
	CGO_ENABLED=0 go build -o thornhill ./cmd/thornhill

up:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f app
