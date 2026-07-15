#!/usr/bin/env bash
# Run deterministic static analysis plus current vulnerability and
# misconfiguration scans. Scanner images/rule engines are pinned in
# .github/scanners/compose.yml and refreshed by Dependabot.
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

command -v docker >/dev/null || {
  printf '%s\n' 'docker is required for container security scans' >&2
  exit 1
}
docker compose version >/dev/null

umask 077
export SCANNER_UID=${SCANNER_UID:-$(id -u)}
export SCANNER_GID=${SCANNER_GID:-$(id -g)}
export SCAN_OUTPUT_DIR=${SCAN_OUTPUT_DIR:-$(mktemp -d -t thornhill-scan.XXXXXX)}
export TRIVY_CACHE_DIR=${TRIVY_CACHE_DIR:-"${XDG_CACHE_HOME:-$HOME/.cache}/thornhill/trivy"}
mkdir -p "$SCAN_OUTPUT_DIR" "$TRIVY_CACHE_DIR"

compose=(docker compose --file "$root/.github/scanners/compose.yml" --project-directory "$root")

printf '%s\n' '==> Hadolint: Dockerfiles'
"${compose[@]}" run --rm --no-deps hadolint Dockerfile Dockerfile.postgres

printf '%s\n' '==> ShellCheck: tracked shell scripts'
mapfile -d '' shell_files < <(git ls-files -z -- '*.sh')
if ((${#shell_files[@]})); then
  "${compose[@]}" run --rm --no-deps shellcheck "${shell_files[@]}"
fi

printf '%s\n' '==> Trivy: configuration misconfigurations'
# Dockerfile.postgres is intentionally a digest-pinned upstream wrapper. Its
# entrypoint must begin as root to initialize a fresh named volume, then drops
# to PostgreSQL's non-root user; applying Trivy's static USER rule would either
# be inaccurate or break first-boot initialization. Hadolint, digest pinning,
# and a runtime image vulnerability scan still cover that image.
"${compose[@]}" run --rm --no-deps trivy config \
  --exit-code 1 \
  --severity HIGH,CRITICAL \
  --skip-files Dockerfile.postgres \
  --timeout 10m \
  .

printf '%s\n' '==> Trivy: filesystem dependency vulnerabilities'
"${compose[@]}" run --rm --no-deps trivy fs \
  --scanners vuln \
  --ignore-unfixed \
  --exit-code 1 \
  --severity HIGH,CRITICAL \
  --skip-dirs .git \
  --skip-dirs web/node_modules \
  --timeout 10m \
  .

for image in "$@"; do
  safe_name=${image//[^A-Za-z0-9_.-]/_}
  archive="$SCAN_OUTPUT_DIR/${safe_name}.tar"
  sbom="$SCAN_OUTPUT_DIR/${safe_name}.cdx.json"

  printf '%s\n' "==> Trivy: image vulnerabilities for ${image}"
  docker image inspect "$image" >/dev/null
  docker image save --output "$archive" "$image"
  image_scan=(
    --input "/output/$(basename -- "$archive")"
    --scanners vuln
    --ignore-unfixed
    --exit-code 1
    --severity "HIGH,CRITICAL"
    --timeout 10m
  )
  if [[ "$image" == *postgres* ]]; then
    # The upstream gosu helper is only a UID-switching launcher. Trivy's binary
    # matcher reports every Go stdlib advisory regardless of reachability, so
    # gate this third-party image on its OS packages and scan Thornhill's own Go
    # binary with the full language-aware scanner above.
    image_scan+=(--pkg-types os)
  fi
  "${compose[@]}" run --rm --no-deps trivy image "${image_scan[@]}"

  printf '%s\n' "==> Trivy: CycloneDX SBOM for ${image}"
  "${compose[@]}" run --rm --no-deps trivy image \
    --input "/output/$(basename -- "$archive")" \
    --format cyclonedx \
    --scanners vuln \
    --output "/output/$(basename -- "$sbom")" \
    --timeout 10m
  rm --force -- "$archive"
done

printf 'Security scans passed. SBOMs: %s\n' "$SCAN_OUTPUT_DIR"
