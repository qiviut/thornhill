# --- web build ---
FROM node:26.12.1-alpine@sha256:40cd011b55415bc894e45b02fdd5a5dfde056921b7d23a64c74f80497af48797 AS web
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
FROM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
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
FROM cgr.dev/chainguard/static:latest@sha256:fc42af33e643f20d08baf03cddcc1c02ba57052dfb02edb0d79f3fc47bcea8d9
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
