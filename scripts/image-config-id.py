#!/usr/bin/env python3
"""Return a verified image-config digest from a single-tag Docker save archive.

Docker image inspect's .Id can name an OCI index on one image store and the
image configuration on another. Docker save's manifest names the actual config
blob, whose digest binds the image configuration and rootfs diff IDs.
"""
import argparse
import hashlib
import json
import re
import sys
import tarfile

MAX_MANIFEST = 1024 * 1024
MAX_CONFIG = 4 * 1024 * 1024
DIGEST = re.compile(r"[0-9a-f]{64}\Z")


def unique_regular(archive, name, limit):
    matches = [member for member in archive.getmembers() if member.name == name]
    if len(matches) != 1 or not matches[0].isfile() or matches[0].size > limit:
        raise ValueError(f"expected exactly one bounded regular archive member: {name}")
    stream = archive.extractfile(matches[0])
    if stream is None:
        raise ValueError(f"cannot read archive member: {name}")
    return stream.read()


def config_id(path, tag, revision):
    with tarfile.open(path, mode="r:*") as archive:
        manifest = json.loads(unique_regular(archive, "manifest.json", MAX_MANIFEST))
        if not isinstance(manifest, list) or len(manifest) != 1:
            raise ValueError("archive must contain exactly one image")
        image = manifest[0]
        if not isinstance(image, dict) or image.get("RepoTags") != [tag]:
            raise ValueError("archive tag does not match the expected image")
        config_path = image.get("Config")
        if not isinstance(config_path, str):
            raise ValueError("archive config path is missing")
        if config_path.startswith("blobs/sha256/"):
            digest = config_path.removeprefix("blobs/sha256/")
        elif config_path.endswith(".json"):
            digest = config_path.removesuffix(".json")
        else:
            raise ValueError("unsupported image config path")
        if not DIGEST.fullmatch(digest):
            raise ValueError("invalid image config digest path")
        config_bytes = unique_regular(archive, config_path, MAX_CONFIG)
        if hashlib.sha256(config_bytes).hexdigest() != digest:
            raise ValueError("image config digest does not match its contents")
        config = json.loads(config_bytes)
        if not isinstance(config, dict):
            raise ValueError("image config is not an object")
        labels = config.get("config", {}).get("Labels", {})
        if not isinstance(labels, dict) or labels.get("org.opencontainers.image.revision") != revision:
            raise ValueError("image config revision does not match")
        rootfs = config.get("rootfs", {})
        diff_ids = rootfs.get("diff_ids") if isinstance(rootfs, dict) else None
        if rootfs.get("type") != "layers" or not isinstance(diff_ids, list) or not all(
            isinstance(item, str) and item.startswith("sha256:") and DIGEST.fullmatch(item[7:])
            for item in diff_ids
        ):
            raise ValueError("image config rootfs diff IDs are invalid")
        return "sha256:" + digest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive")
    parser.add_argument("--tag", required=True)
    parser.add_argument("--revision", required=True)
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9a-f]{40}", args.revision):
        parser.error("revision must be a 40-character lowercase commit")
    try:
        print(config_id(args.archive, args.tag, args.revision))
    except (OSError, ValueError, TypeError, AttributeError, KeyError, tarfile.TarError, json.JSONDecodeError) as exc:
        print(f"invalid image archive: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
