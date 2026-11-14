#!/usr/bin/env bash
# Exercise installer identity checks without changing live Docker or PostgreSQL.
set -euo pipefail
bundle=${1:?bundle required}
artifact=${2:?artifact required}
revision=${3:?revision required}
tmp=$(mktemp -d "${TMPDIR:-/tmp}/thornhill-release-identity-test.XXXXXX")
cleanup() { rm -rf -- "${tmp}"; }
trap cleanup EXIT
mkdir -p "${tmp}/bin"
python3 - "${tmp}/wrong-app.tar.gz" "${revision}" <<'PY'
import hashlib
import io
import json
from pathlib import Path
import sys
import tarfile

path, revision = Path(sys.argv[1]), sys.argv[2]
config = json.dumps({"config": {"Labels": {"org.opencontainers.image.revision": revision},
                                "Env": ["DIFFERENT_IMAGE=1"]},
                     "rootfs": {"type": "layers", "diff_ids": []}},
                    separators=(",", ":")).encode()
name = "blobs/sha256/" + hashlib.sha256(config).hexdigest()
manifest = json.dumps([{"Config": name, "RepoTags": ["thornhill:ci"], "Layers": []}]).encode()
with tarfile.open(path, "w:gz") as archive:
    for member, payload in (("manifest.json", manifest), (name, config)):
        info = tarfile.TarInfo(member)
        info.size = len(payload)
        archive.addfile(info, io.BytesIO(payload))
PY
printf 'THORNHILL_DB_PASSWORD=not-used\n' >"${tmp}/env"
chmod 0600 "${tmp}/env"
cat >"${tmp}/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${FAKE_DOCKER_LOG}"
case "${1:-} ${2:-}" in
  'compose version') exit 0 ;;
  'compose --project-name') exit 0 ;;
  'load --input') exit 0 ;;
  'image save')
    [[ "${3:-}" == --output ]]
    case "${5:-}" in
      thornhill:ci)
        if [[ "${FAKE_DOCKER_BAD:-0}" == 1 ]]; then
          cp "${FAKE_BAD_ARCHIVE}" "$4"
        else
          cp "${FAKE_ARTIFACT}/app.tar.gz" "$4"
        fi ;;
      thornhill-postgres:ci) cp "${FAKE_ARTIFACT}/postgres.tar.gz" "$4" ;;
      *) exit 2 ;;
    esac ;;
  'image inspect')
    case "${*}" in
      *'{{.Id}}'*) printf 'sha256:%064d\n' 0 ;;
      *'org.opencontainers.image.revision'*) printf '%s\n' "${FAKE_REVISION}" ;;
      *) exit 2 ;;
    esac ;;
  'run --rm') printf 'thornhill %s\n' "${FAKE_REVISION}" ;;
  tag\ *) exit 0 ;;
  *) printf 'unexpected docker command: %s\n' "$*" >&2; exit 2 ;;
esac
SH
cat >"${tmp}/bin/curl" <<'SH'
#!/usr/bin/env bash
# Stop after identity checks and before any database/container changes.
exit 22
SH
chmod 0755 "${tmp}/bin/docker" "${tmp}/bin/curl"
export PATH="${tmp}/bin:${PATH}"
export FAKE_ARTIFACT="${artifact}" FAKE_REVISION="${revision}"
export FAKE_BAD_ARCHIVE="${tmp}/wrong-app.tar.gz"
export FAKE_DOCKER_LOG="${tmp}/docker.log"

# .Id is deliberately the wrong OCI-index-style digest. The config-ID path
# must still reach the safe stopping point after both image tags are prepared.
if "${bundle}/install-release.sh" --bundle "${bundle}" --env-file "${tmp}/env" \
  --expected-sha "${revision}" --state-dir "${tmp}/state-good" \
  >"${tmp}/good.out" 2>&1; then
  printf '%s\n' 'fake installer unexpectedly succeeded' >&2
  exit 1
fi
if ! grep -Fq 'tag thornhill:ci thornhill-app:' "${FAKE_DOCKER_LOG}"; then
  printf '%s\n' 'installer did not pass portable config identity validation' >&2
  cat "${tmp}/good.out" >&2
  exit 1
fi
if grep -Fq 'exec ' "${FAKE_DOCKER_LOG}"; then
  printf '%s\n' 'fake installer reached a database or container exec' >&2
  exit 1
fi

# A different loaded archive must fail before tags or runtime work.
: >"${FAKE_DOCKER_LOG}"
export FAKE_DOCKER_BAD=1
if "${bundle}/install-release.sh" --bundle "${bundle}" --env-file "${tmp}/env" \
  --expected-sha "${revision}" --state-dir "${tmp}/state-bad" \
  >"${tmp}/bad.out" 2>&1; then
  printf '%s\n' 'installer accepted a mismatched loaded image' >&2
  exit 1
fi
if grep -Fq 'tag thornhill:ci thornhill-app:' "${FAKE_DOCKER_LOG}"; then
  printf '%s\n' 'installer tagged a mismatched loaded image' >&2
  exit 1
fi
grep -Fq 'Loaded image IDs do not match the bundle manifest' "${tmp}/bad.out"
printf '%s\n' 'Portable image identity installer checks passed'
