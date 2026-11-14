#!/usr/bin/env python3
"""Regression tests for portable Docker archive config identity."""
import hashlib
import importlib.util
import io
import json
import tarfile
import tempfile
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location("image_config_id", Path(__file__).with_name("image-config-id.py"))
assert spec is not None and spec.loader is not None
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
REVISION = "0123456789abcdef0123456789abcdef01234567"
TAG = "thornhill:ci"


class ImageConfigTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="thornhill-image-id-")
        self.archive = Path(self.tmp.name) / "image.tar.gz"
        self.addCleanup(self.tmp.cleanup)

    def make_archive(self, *, legacy=False, config=None, wrong_hash=False, tag=TAG,
                     duplicate_manifest=False, symlink_config=False, extra_image=False):
        if config is None:
            config = {"architecture": "amd64", "os": "linux",
                      "config": {"Labels": {"org.opencontainers.image.revision": REVISION}},
                      "rootfs": {"type": "layers", "diff_ids": ["sha256:" + "1" * 64]}}
        config_bytes = json.dumps(config, separators=(",", ":")).encode()
        digest = hashlib.sha256(config_bytes).hexdigest()
        actual_name = "0" * 64 if wrong_hash else digest
        config_path = actual_name + ".json" if legacy else "blobs/sha256/" + actual_name
        manifest = [{"Config": config_path, "RepoTags": [tag], "Layers": []}]
        if extra_image:
            manifest.append(manifest[0])
        manifest_bytes = json.dumps(manifest).encode()
        with tarfile.open(self.archive, "w:gz") as tar:
            def add(name, content):
                member = tarfile.TarInfo(name)
                member.size = len(content)
                tar.addfile(member, io.BytesIO(content))
            add("manifest.json", manifest_bytes)
            if duplicate_manifest:
                add("manifest.json", manifest_bytes)
            if symlink_config:
                member = tarfile.TarInfo(config_path)
                member.type = tarfile.SYMTYPE
                member.linkname = "manifest.json"
                tar.addfile(member)
            else:
                add(config_path, config_bytes)
        return "sha256:" + digest

    def test_oci_archive_config_digest(self):
        expected = self.make_archive()
        self.assertEqual(module.config_id(self.archive, TAG, REVISION), expected)

    def test_legacy_archive_config_digest(self):
        expected = self.make_archive(legacy=True)
        self.assertEqual(module.config_id(self.archive, TAG, REVISION), expected)

    def test_wrong_tag_is_rejected(self):
        self.make_archive(tag="thornhill:other")
        with self.assertRaises(ValueError):
            module.config_id(self.archive, TAG, REVISION)

    def test_wrong_revision_is_rejected(self):
        self.make_archive()
        with self.assertRaises(ValueError):
            module.config_id(self.archive, TAG, "a" * 40)

    def test_config_digest_mismatch_is_rejected(self):
        self.make_archive(wrong_hash=True)
        with self.assertRaises(ValueError):
            module.config_id(self.archive, TAG, REVISION)

    def test_duplicate_manifest_is_rejected(self):
        self.make_archive(duplicate_manifest=True)
        with self.assertRaises(ValueError):
            module.config_id(self.archive, TAG, REVISION)

    def test_nonregular_config_is_rejected(self):
        self.make_archive(symlink_config=True)
        with self.assertRaises(ValueError):
            module.config_id(self.archive, TAG, REVISION)

    def test_multiple_images_are_rejected(self):
        self.make_archive(extra_image=True)
        with self.assertRaises(ValueError):
            module.config_id(self.archive, TAG, REVISION)

    def test_invalid_rootfs_is_rejected(self):
        self.make_archive(config={"config": {"Labels": {"org.opencontainers.image.revision": REVISION}},
                                  "rootfs": {"type": "layers", "diff_ids": ["bad"]}})
        with self.assertRaises(ValueError):
            module.config_id(self.archive, TAG, REVISION)

    def test_malformed_archive_is_rejected(self):
        self.archive.write_bytes(b"not a tar")
        with self.assertRaises(tarfile.TarError):
            module.config_id(self.archive, TAG, REVISION)


if __name__ == "__main__":
    unittest.main()
