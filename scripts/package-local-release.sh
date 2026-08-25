#!/usr/bin/env bash
set -euo pipefail
umask 022

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
ARTIFACT_DIR=""
OUTPUT_DIR=""
REVISION=${THORNHILL_RELEASE_REVISION:-${GITHUB_SHA:-}}
CI_RUN_ID=${THORNHILL_RELEASE_CI_RUN_ID:-${GITHUB_RUN_ID:-0}}
CI_URL=${THORNHILL_RELEASE_CI_URL:-}
SCAN_OUTPUT_DIR=${SCAN_OUTPUT_DIR:-}

usage() {
  cat <<'USAGE'
Usage: package-local-release.sh --artifact-dir DIR --output-dir DIR --revision SHA [options]

Assemble a self-contained, operator-installed OCI release bundle from the exact
image archives and manifest produced by the CI image lane. This script never
pushes images, contacts GitHub, or reads credentials.

Options:
  --artifact-dir DIR  CI image artifact directory
  --output-dir DIR    Destination bundle directory; must not already exist
  --revision SHA      Expected 40-character source revision
  --ci-run-id ID      Optional CI run identifier for release metadata
  --ci-url URL        Optional CI run URL for release metadata
  --scan-dir DIR      Optional directory containing CycloneDX SBOMs
  --help              Show this help
USAGE
}

while (($#)); do
  case "$1" in
    --artifact-dir) ARTIFACT_DIR=${2:?missing value for --artifact-dir}; shift 2 ;;
    --output-dir) OUTPUT_DIR=${2:?missing value for --output-dir}; shift 2 ;;
    --revision) REVISION=${2:?missing value for --revision}; shift 2 ;;
    --ci-run-id) CI_RUN_ID=${2:?missing value for --ci-run-id}; shift 2 ;;
    --ci-url) CI_URL=${2:?missing value for --ci-url}; shift 2 ;;
    --scan-dir) SCAN_OUTPUT_DIR=${2:?missing value for --scan-dir}; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) printf 'Unknown option: %s\n' "$1" >&2; usage >&2; exit 64 ;;
  esac
done

[[ -n "${ARTIFACT_DIR}" ]] || { printf '%s\n' '--artifact-dir is required' >&2; exit 64; }
[[ -n "${OUTPUT_DIR}" ]] || { printf '%s\n' '--output-dir is required' >&2; exit 64; }
[[ "${REVISION}" =~ ^[0-9a-f]{40}$ ]] || {
  printf 'Invalid release revision: %s\n' "${REVISION@Q}" >&2
  exit 64
}
[[ "${CI_RUN_ID}" =~ ^[0-9]+$ ]] || {
  printf 'Invalid CI run identifier: %s\n' "${CI_RUN_ID@Q}" >&2
  exit 64
}
[[ -d "${ARTIFACT_DIR}" ]] || { printf 'Artifact directory is missing: %s\n' "${ARTIFACT_DIR}" >&2; exit 1; }
[[ ! -e "${OUTPUT_DIR}" ]] || { printf 'Output directory already exists: %s\n' "${OUTPUT_DIR}" >&2; exit 1; }

manifest="${ARTIFACT_DIR}/manifest.json"
app_archive="${ARTIFACT_DIR}/app.tar.gz"
db_archive="${ARTIFACT_DIR}/postgres.tar.gz"
for path in "${manifest}" "${app_archive}" "${db_archive}"; do
  [[ -s "${path}" ]] || { printf 'Required artifact is missing or empty: %s\n' "${path}" >&2; exit 1; }
done

jq -e --arg expected "${REVISION}" '
  .version == 1 and
  .source_commit == $expected and
  .images.app.archive == "app.tar.gz" and
  .images.app.local_tag == "thornhill:ci" and
  .images.postgres.archive == "postgres.tar.gz" and
  .images.postgres.local_tag == "thornhill-postgres:ci" and
  (.images.app.id | test("^sha256:[0-9a-f]{64}$")) and
  (.images.postgres.id | test("^sha256:[0-9a-f]{64}$")) and
  .images.app.revision == $expected and
  .images.postgres.revision == $expected
' "${manifest}" >/dev/null

staging=$(mktemp -d "${TMPDIR:-/tmp}/thornhill-release.XXXXXX")
cleanup() { rm -rf -- "${staging}"; }
trap cleanup EXIT
mkdir -p "${staging}/images" "${staging}/compose" "${staging}/scripts" "${staging}/sbom"

install -m 0644 "${manifest}" "${staging}/images/manifest.json"
install -m 0644 "${app_archive}" "${staging}/images/app.tar.gz"
install -m 0644 "${db_archive}" "${staging}/images/postgres.tar.gz"
install -m 0644 "${ROOT}/docker-compose.yml" "${staging}/compose/docker-compose.yml"
install -m 0644 "${ROOT}/docker-compose.host.yml" "${staging}/compose/docker-compose.host.yml"
install -m 0644 "${ROOT}/.env.example" "${staging}/compose/.env.example"
install -m 0755 "${ROOT}/scripts/install-local-release.sh" "${staging}/install-release.sh"
install -m 0755 "${ROOT}/scripts/local-recovery.sh" "${staging}/scripts/local-recovery.sh"
install -m 0755 "${ROOT}/scripts/capped-copy.py" "${staging}/scripts/capped-copy.py"
install -m 0755 "${ROOT}/scripts/rotate-postgres-role-password.sh" "${staging}/scripts/rotate-postgres-role-password.sh"
install -m 0644 "${ROOT}/docs/rollback-compatibility.json" "${staging}/compose/rollback-compatibility.json"

if [[ -n "${SCAN_OUTPUT_DIR}" && -d "${SCAN_OUTPUT_DIR}" ]]; then
  while IFS= read -r -d '' sbom; do
    install -m 0644 "${sbom}" "${staging}/sbom/$(basename -- "${sbom}")"
  done < <(find "${SCAN_OUTPUT_DIR}" -maxdepth 1 -type f -name '*.cdx.json' -print0 | sort -z)
fi

jq -n \
  --arg source_commit "${REVISION}" \
  --arg ci_run_id "${CI_RUN_ID}" \
  --arg ci_url "${CI_URL}" \
  '{version:1,source_commit:$source_commit,ci_run_id:($ci_run_id|tonumber),ci_url:$ci_url,
    package_type:"thornhill-local-oci-bundle",
    images_manifest:"images/manifest.json",
    install_entrypoint:"install-release.sh"}' \
  >"${staging}/release.json"

(
  cd "${staging}"
  find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS
  sha256sum -c SHA256SUMS >/dev/null
)

mkdir -p "$(dirname -- "${OUTPUT_DIR}")"
mv -- "${staging}" "${OUTPUT_DIR}"
trap - EXIT
printf 'Created local release bundle: %s\n' "${OUTPUT_DIR}"
printf 'source_commit=%s\n' "${REVISION}"
printf 'file_count=%s\n' "$(wc -l <"${OUTPUT_DIR}/SHA256SUMS")"
