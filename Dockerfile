# --- web build ---
FROM node:26.7.0-alpine@sha256:aadf416b2cdce311a8811ba3f0608a61b77dbf997500e2eafe781b51f6a0b019 AS web
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
FROM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build
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
FROM cgr.dev/chainguard/static:latest@sha256:f68e3a8244c7d0f4cd56635aaff8e6a533cf6cc3850d8fb339567a5782d6a0b0
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
