#!/usr/bin/env bash
# Exercise the image identity contract on a real Docker save/load round trip.
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tag=${1:?image tag required}
revision=${2:?source revision required}
tmp=$(mktemp -d "${TMPDIR:-/tmp}/thornhill-image-roundtrip.XXXXXX")
cleanup() { rm -rf -- "${tmp}"; }
trap cleanup EXIT

docker image save --output "${tmp}/before.tar" "${tag}"
before=$(python3 "${root}/scripts/image-config-id.py" "${tmp}/before.tar" --tag "${tag}" --revision "${revision}")
docker load --input "${tmp}/before.tar" >/dev/null
docker image save --output "${tmp}/after.tar" "${tag}"
after=$(python3 "${root}/scripts/image-config-id.py" "${tmp}/after.tar" --tag "${tag}" --revision "${revision}")
[[ "${before}" == "${after}" ]] || {
  printf 'Docker save/load changed the image configuration identity for %s\n' "${tag}" >&2
  exit 1
}
printf 'Docker save/load config identity verified: %s %s\n' "${tag}" "${before}"
