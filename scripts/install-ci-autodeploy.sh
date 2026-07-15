#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ "${ROOT}" == *[' ']* ]]; then
  echo "Repository path with spaces is not supported: ${ROOT@Q}" >&2
  exit 1
fi
for command in git gh jq docker curl flock timeout python3 systemctl; do
  command -v "${command}" >/dev/null || { echo "Missing required command: ${command}" >&2; exit 1; }
done
docker buildx version >/dev/null || {
  echo "Missing required Docker Buildx CLI plugin (install docker-buildx before enabling deployment)" >&2
  exit 1
}
if [[ ! -f "${ROOT}/.env" ]]; then
  echo "Missing host-local ${ROOT}/.env" >&2
  exit 1
fi
: "${PUBLIC_APP_URL:?set the externally reachable Thornhill URL}"
: "${PUBLIC_STATUS_URL:?set its /api/status URL}"

unit_dir=${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user
mkdir -p "${unit_dir}"
python3 - "${ROOT}" "${unit_dir}" "${PUBLIC_APP_URL}" "${PUBLIC_STATUS_URL}" <<'PY'
import os
import pathlib
import sys
import urllib.parse

root, destination, public_app_url, public_status_url = sys.argv[1:]
source = pathlib.Path(root) / "systemd"
destination = pathlib.Path(destination)

app = urllib.parse.urlsplit(public_app_url)
status = urllib.parse.urlsplit(public_status_url)
for label, parsed in (("PUBLIC_APP_URL", app), ("PUBLIC_STATUS_URL", status)):
    if parsed.scheme != "https" or not parsed.hostname:
        raise SystemExit(f"{label} must be an absolute HTTPS URL")
    if parsed.username or parsed.password or parsed.fragment:
        raise SystemExit(f"{label} must not contain userinfo or a fragment")
if (app.scheme, app.netloc) != (status.scheme, status.netloc):
    raise SystemExit("PUBLIC_APP_URL and PUBLIC_STATUS_URL must use the same origin")
if status.path.rstrip("/") != "/api/status":
    raise SystemExit("PUBLIC_STATUS_URL path must be /api/status")

for name in ("thornhill-ci-deploy.service", "thornhill-ci-deploy.timer"):
    data = (source / name).read_text()
    data = data.replace("@ROOT@", root)
    temporary = destination / (name + ".tmp")
    temporary.write_text(data)
    os.chmod(temporary, 0o644)
    os.replace(temporary, destination / name)

def unit_quote(value):
    return '"' + value.replace("\\", "\\\\").replace('"', '\\"').replace("%", "%%") + '"'

dropin = destination / "thornhill-ci-deploy.service.d"
dropin.mkdir(mode=0o700, exist_ok=True)
endpoints = dropin / "10-endpoints.conf"
temporary = dropin / "10-endpoints.conf.tmp"
temporary.write_text(
    "[Service]\n"
    f"Environment={unit_quote('PUBLIC_APP_URL=' + public_app_url)}\n"
    f"Environment={unit_quote('PUBLIC_STATUS_URL=' + public_status_url)}\n"
)
os.chmod(temporary, 0o600)
os.replace(temporary, endpoints)
PY
chmod 0755 "${ROOT}/scripts/deploy-passed-main.sh"
systemctl --user daemon-reload
systemd-analyze --user verify "${unit_dir}/thornhill-ci-deploy.service" "${unit_dir}/thornhill-ci-deploy.timer"
if [[ "${ENABLE_TIMER:-1}" != 1 ]]; then
  echo "Installed and verified Thornhill deployment units without enabling them"
  exit 0
fi
systemctl --user enable --now thornhill-ci-deploy.timer
if [[ "${START_NOW:-1}" == 1 ]]; then
  systemctl --user start thornhill-ci-deploy.service
fi
systemctl --user --no-pager status thornhill-ci-deploy.timer
