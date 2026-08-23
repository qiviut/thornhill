#!/usr/bin/env bash
# Protected-main, spend-bounded live canary for the browser surface and provider.
set -euo pipefail

BASE_URL=${THORNHILL_CANARY_BASE_URL:?set THORNHILL_CANARY_BASE_URL}
EXPECTED_SHA=${THORNHILL_CANARY_EXPECTED_SHA:?set THORNHILL_CANARY_EXPECTED_SHA}
MAX_SECONDS=${THORNHILL_CANARY_MAX_SECONDS:-30}
PROVIDER_URL=${THORNHILL_CANARY_PROVIDER_URL:-}
PROVIDER_TOKEN=${THORNHILL_CANARY_PROVIDER_TOKEN:-}
BROWSER_REQUIRED=${THORNHILL_CANARY_BROWSER_REQUIRED:-0}

[[ "${EXPECTED_SHA}" =~ ^[0-9a-f]{40}$ ]] || {
  echo 'canary expected SHA must be a full lowercase commit SHA' >&2
  exit 2
}
[[ "${BASE_URL}" =~ ^https://[^/]+/?$ ]] || {
  echo 'canary base URL must be an HTTPS origin without a path, query, or fragment' >&2
  exit 2
}
[[ "${MAX_SECONDS}" =~ ^[1-9][0-9]*$ && "${MAX_SECONDS}" -le 120 ]] || {
  echo 'canary timeout must be between 1 and 120 seconds' >&2
  exit 2
}
if [[ -n "${PROVIDER_URL}" && -z "${PROVIDER_TOKEN}" ]] ||
  [[ -z "${PROVIDER_URL}" && -n "${PROVIDER_TOKEN}" ]]; then
  echo 'provider URL and provider token must be configured together' >&2
  exit 2
fi

BASE_URL=${BASE_URL%/}
STATUS_URL="${BASE_URL}/api/status"
TMP_DIR=$(mktemp -d)
cleanup() { rm -rf -- "${TMP_DIR}"; }
trap cleanup EXIT INT TERM

run_bounded() {
  timeout --signal=TERM --kill-after=5 "${MAX_SECONDS}s" "$@"
}

run_bounded curl --fail --silent --show-error --location --max-time "${MAX_SECONDS}" \
  --proto '=https' --output "${TMP_DIR}/index.html" "${BASE_URL}/"
run_bounded curl --fail --silent --show-error --location --max-time "${MAX_SECONDS}" \
  --proto '=https' --output "${TMP_DIR}/status.json" "${STATUS_URL}"
jq -e --arg expected "${EXPECTED_SHA}" \
  '.status == "ok" and .versioned == true and .source_commit == $expected' \
  "${TMP_DIR}/status.json" >/dev/null
grep -q 'id="root"' "${TMP_DIR}/index.html"

browser_bin=${THORNHILL_CANARY_BROWSER_BIN:-}
if [[ -z "${browser_bin}" ]]; then
  for candidate in chromium chromium-browser google-chrome google-chrome-stable; do
    if command -v "${candidate}" >/dev/null 2>&1; then
      browser_bin=$(command -v "${candidate}")
      break
    fi
  done
fi
if [[ -n "${browser_bin}" ]]; then
  run_bounded "${browser_bin}" --headless --disable-gpu --no-sandbox \
    --dump-dom "${BASE_URL}/" >"${TMP_DIR}/browser.html"
  grep -q 'id="root"' "${TMP_DIR}/browser.html"
  if grep -Eqi 'application error|uncaught exception' "${TMP_DIR}/browser.html"; then
    echo 'browser surface reported an application error' >&2
    exit 1
  fi
  echo "browser_surface=passed browser=${browser_bin}"
elif [[ "${BROWSER_REQUIRED}" == 1 ]]; then
  echo 'browser canary required but no supported headless browser is installed' >&2
  exit 1
else
  echo 'browser_surface=not-configured'
fi

if [[ -n "${PROVIDER_URL}" ]]; then
  [[ "${PROVIDER_URL}" =~ ^https://[^/]+(/.*)?$ ]] || {
    echo 'provider canary URL must use HTTPS' >&2
    exit 2
  }
  provider_models="${PROVIDER_URL%/}/v1/models"
  run_bounded curl --fail --silent --show-error --location --max-time "${MAX_SECONDS}" \
    --proto '=https' --header "Authorization: Bearer ${PROVIDER_TOKEN}" \
    --output "${TMP_DIR}/provider.json" "${provider_models}"
  jq -e 'type == "object"' "${TMP_DIR}/provider.json" >/dev/null
  echo 'provider_surface=passed'
else
  echo 'provider_surface=not-configured'
fi

printf 'canary=passed revision=%s\n' "${EXPECTED_SHA}"
