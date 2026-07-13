# --- web build ---
FROM node:26.5.0-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN set -eu; \
    for attempt in 1 2 3; do \
      rm -rf node_modules; \
      if npm ci --include=optional --ignore-scripts --no-audit --no-fund \
        && ./node_modules/.bin/tsc --version >/dev/null \
        && node --input-type=module -e "await import('rolldown')"; then \
        exit 0; \
      fi; \
      [ "$attempt" -eq 3 ] || sleep "$attempt"; \
    done; \
    exit 1
COPY web/ ./
RUN npm run check && npm run build

# --- Go build ---
FROM golang:1.26.5-alpine AS build
ARG THORNHILL_REVISION=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X thornhill/internal/buildinfo.Commit=${THORNHILL_REVISION}" \
    -o /out/thornhill ./cmd/thornhill

# --- runtime ---
FROM debian:stable-slim
ARG THORNHILL_REVISION=unknown
LABEL org.opencontainers.image.revision="${THORNHILL_REVISION}"
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --system thornhill \
 && useradd --system --gid thornhill --home-dir /app --no-create-home thornhill \
 && install -d -o thornhill -g thornhill /app /app/web /data
WORKDIR /app
COPY --chown=thornhill:thornhill --from=build /out/thornhill /app/thornhill
COPY --chown=thornhill:thornhill --from=web /src/web/dist /app/web/dist
ENV STATIC_DIR=/app/web/dist \
    PREBAKE_DIR=/data/prebaked
VOLUME /data
EXPOSE 8787
USER thornhill
ENTRYPOINT ["/app/thornhill"]
