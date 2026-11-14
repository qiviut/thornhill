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
PYTHONDONTWRITEBYTECODE=1 python3 "${root}/scripts/test-image-config-id.py"
python3 - "${artifact}" "${revision}" <<'PY'
import hashlib
import io
import json
from pathlib import Path
import sys
import tarfile

artifact, revision = Path(sys.argv[1]), sys.argv[2]
for name, tag in (("app", "thornhill:ci"), ("postgres", "thornhill-postgres:ci")):
    config = json.dumps({"config": {"Labels": {"org.opencontainers.image.revision": revision}},
                         "rootfs": {"type": "layers", "diff_ids": []}},
                        separators=(",", ":")).encode()
    digest = hashlib.sha256(config).hexdigest()
    config_path = f"blobs/sha256/{digest}"
    manifest = json.dumps([{"Config": config_path, "RepoTags": [tag], "Layers": []}]).encode()
    with tarfile.open(artifact / f"{name}.tar.gz", "w:gz") as tar:
        for path, data in (("manifest.json", manifest), (config_path, config)):
            entry = tarfile.TarInfo(path)
            entry.size = len(data)
            tar.addfile(entry, io.BytesIO(data))
PY
app_id=$(python3 "${root}/scripts/image-config-id.py" "${artifact}/app.tar.gz" --tag thornhill:ci --revision "${revision}")
db_id=$(python3 "${root}/scripts/image-config-id.py" "${artifact}/postgres.tar.gz" --tag thornhill-postgres:ci --revision "${revision}")
jq -n --arg revision "${revision}" --arg app_id "${app_id}" --arg db_id "${db_id}" \
  '{version:2,source_commit:$revision,images:{app:{archive:"app.tar.gz",local_tag:"thornhill:ci",id:$app_id,revision:$revision},postgres:{archive:"postgres.tar.gz",local_tag:"thornhill-postgres:ci",id:$db_id,revision:$revision}}}' \
  >"${artifact}/manifest.json"

# A manifest with the wrong (but syntactically valid) ID must not be packaged.
cp "${artifact}/manifest.json" "${tmp}/good-manifest.json"
jq '.images.app.id = "sha256:0000000000000000000000000000000000000000000000000000000000000000"' \
  "${tmp}/good-manifest.json" >"${artifact}/manifest.json"
if "${root}/scripts/package-local-release.sh" \
  --artifact-dir "${artifact}" --output-dir "${tmp}/bad-bundle" --revision "${revision}" >/dev/null 2>&1; then
  printf '%s\n' 'Mismatched image config ID was accepted for packaging' >&2
  exit 1
fi
cp "${tmp}/good-manifest.json" "${artifact}/manifest.json"

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

"${root}/scripts/test-local-release-runtime.sh" "${bundle}" "${artifact}" "${revision}"

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
