#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
revision=0123456789abcdef0123456789abcdef01234567
tmp=$(mktemp -d "${TMPDIR:-/tmp}/thornhill-local-release-test.XXXXXX")
cleanup() { rm -rf -- "${tmp}"; }
trap cleanup EXIT

artifact="${tmp}/artifact"
bundle="${tmp}/bundle"
mkdir -p "${artifact}"
printf 'synthetic app archive\n' >"${artifact}/app.tar.gz"
printf 'synthetic postgres archive\n' >"${artifact}/postgres.tar.gz"
jq -n --arg revision "${revision}" \
  '{version:1,source_commit:$revision,images:{app:{archive:"app.tar.gz",local_tag:"thornhill:ci",id:"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",revision:$revision},postgres:{archive:"postgres.tar.gz",local_tag:"thornhill-postgres:ci",id:"sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",revision:$revision}}}' \
  >"${artifact}/manifest.json"

THORNHILL_RELEASE_CI_RUN_ID=123 \
THORNHILL_RELEASE_CI_URL=https://github.example.invalid/actions/runs/123 \
  "${root}/scripts/package-local-release.sh" \
    --artifact-dir "${artifact}" --output-dir "${bundle}" --revision "${revision}"

(
  cd "${bundle}"
  sha256sum -c SHA256SUMS >/dev/null
)
[[ -x "${bundle}/install-release.sh" ]]
[[ -f "${bundle}/compose/env.example" ]]
[[ ! -e "${bundle}/compose/.env.example" ]]
[[ ! -e "${bundle}/compose/.env" ]]
[[ ! -e "${bundle}/.env" ]]
python3 - "${bundle}/SHA256SUMS" <<'PY'
from pathlib import Path
import sys

for line in Path(sys.argv[1]).read_text(encoding="utf-8").splitlines():
    _, path = line.split(maxsplit=1)
    if path.startswith("./"):
        path = path[2:]
    if any(part.startswith(".") for part in path.split("/")):
        raise SystemExit(f"hidden bundle path cannot survive artifact upload: {path}")
PY

"${bundle}/install-release.sh" \
  --bundle "${bundle}" \
  --env-file "${root}/.env.example" \
  --expected-sha "${revision}" \
  --check-only

printf '%s\n' 'unhashed override must be rejected' >"${tmp}/outside-override.yml"
if "${bundle}/install-release.sh" \
  --bundle "${bundle}" \
  --env-file "${root}/.env.example" \
  --expected-sha "${revision}" \
  --compose-override "${tmp}/outside-override.yml" \
  --check-only >/dev/null 2>&1; then
  printf '%s\n' 'unhashed Compose override was accepted' >&2
  exit 1
fi

printf 'tampered bundle must be rejected\n' >>"${bundle}/images/app.tar.gz"
if "${bundle}/install-release.sh" \
  --bundle "${bundle}" \
  --env-file "${root}/.env.example" \
  --expected-sha "${revision}" \
  --check-only >/dev/null 2>&1; then
  printf '%s\n' 'tampered bundle was accepted' >&2
  exit 1
fi
printf '%s\n' 'Local release bundle checks passed'
