.PHONY: run test web build up down logs

run: ## run the gateway (expects DATABASE_URL + OPENAI_API_KEY in env)
	go run ./cmd/thornhill

test:
	go vet ./... && go test ./...

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
