# --- web build ---
FROM node:26.8.1-alpine@sha256:2d984a15c9b54fd0aeb608b8e0d0d83529eb34d2966db27a1fb4f1edc3d298a3 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN set -eu; \
    for attempt in 1 2 3; do \
      rm -rf node_modules; \
      if npm ci --include=optional --ignore-scripts --no-audit --no-fund \
        && ./node_modules/.bin/tsc --version >/dev/null \
        && ./node_modules/.bin/biome --version >/dev/null \
        && node --input-type=module -e "await import('rolldown')"; then \
        exit 0; \
      fi; \
      [ "$attempt" -eq 3 ] || sleep "$attempt"; \
    done; \
    exit 1
COPY web/ ./
RUN npm run check && npm run lint && npm run build

# --- Go build ---
FROM golang:1.27.0-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build
ARG THORNHILL_REVISION=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN mkdir -p /out/data \
 && CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X thornhill/internal/buildinfo.Commit=${THORNHILL_REVISION}" \
    -o /out/thornhill ./cmd/thornhill

# --- runtime ---
# Chainguard static supplies CA certificates and a non-root identity without a
# shell or package manager, keeping the runtime surface deliberately small.
FROM cgr.dev/chainguard/static:latest@sha256:f51c2493951313c3ad4069080b2814ffb6ed6fe3909dabeb84a9482f42d5600b
ARG THORNHILL_REVISION=unknown
ARG THORNHILL_SOURCE=https://github.com/qiviut/thornhill
LABEL org.opencontainers.image.title="Thornhill" \
      org.opencontainers.image.description="Durable approval parking for Hermes Agent" \
      org.opencontainers.image.source="${THORNHILL_SOURCE}" \
      org.opencontainers.image.revision="${THORNHILL_REVISION}" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
WORKDIR /app
COPY --chown=65532:65532 --from=build /out/thornhill /app/thornhill
COPY --chown=65532:65532 --from=web /src/web/dist /app/web/dist
COPY --chown=65532:65532 --from=build /out/data/ /data/
COPY --chown=65532:65532 LICENSE /licenses/thornhill/LICENSE
ENV STATIC_DIR=/app/web/dist \
    PREBAKE_DIR=/data/prebaked \
    HEALTHCHECK_URL=http://127.0.0.1:8787/api/status
VOLUME /data
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/app/thornhill", "healthcheck"]
USER 65532:65532
ENTRYPOINT ["/app/thornhill"]
