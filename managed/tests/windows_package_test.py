"""Exercise the signed-delivery boundary with independent developer keys."""

import base64
import copy
import os
from pathlib import Path
import struct
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
from zipfile import ZipFile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import windows_package as package
import windows_build_test


def tiny_icon():
    # One 1x1 32-bit DIB and its AND mask.
    pixels = struct.pack("<IiiHHIIiiII", 40, 1, 2, 1, 32, 0, 4, 0, 0, 0, 0)
    pixels += bytes.fromhex("2364baff00000000")
    return struct.pack("<HHHBBBBHHII", 0, 1, 1, 1, 1, 0, 0, 1, 32, len(pixels), 22) + pixels


class SignedPackageTests(unittest.TestCase):
    def setUp(self):
        self.evidence = windows_build_test.WindowsEvidenceTests(methodName="test_requires_both_current_architectures")
        self.evidence.setUp()
        self.addCleanup(self.evidence.doCleanups)
        self.root = self.evidence.root
        self.destination = self.root / "package"
        self.icon = self.root / "brand.ico"
        self.icon.write_bytes(tiny_icon())
        self.approval = {
            "schema": 1,
            "version": "0.1.0-dev.1",
            "brand": {"id": "deskrealm", "revision": 1, "name": "端域", "icon_sha256": package.sha_file(self.icon)},
            "center": {
                "public_origin": "https://center.example.test",
                "id_server": "center.example.test:21116",
                "rendezvous_public_key": base64.b64encode(bytes(range(1, 33))).decode(),
                "signing_key_id": "center-fixture",
                "signing_public_key": base64.urlsafe_b64encode(bytes(range(2, 34))).decode().rstrip("="),
            },
            "group_ids": ["10000000-0000-4000-8000-000000000001"],
            "source_commit": self.evidence.source["commit"],
            "builds": {
                arch: {
                    "manifest_sha256": package.sha_file(self.evidence.path(target) / "manifest.json"),
                    "client_sha256": package.sha_file(self.evidence.path(target) / package.CLIENT),
                }
                for arch, target in package.ARCH.items()
            },
        }
        self.openssl = os.environ.get("DESKREALM_TEST_OPENSSL", "openssl")
        self.key_dir = self.root / "signer"
        self.key_dir.mkdir()
        self.private, self.public = self.keys("release")
        self.signature = self.key_dir / package.SIGNATURE

    def keys(self, label):
        private, public = [self.key_dir / f"{label}-{purpose}.pem" for purpose in ("private", "public")]
        subprocess.run([self.openssl, "genpkey", "-algorithm", "ED25519", "-out", private], check=True)
        subprocess.run([self.openssl, "pkey", "-in", private, "-pubout", "-out", public], check=True)
        return private, public

    def prepare(self, destination=None):
        with patch.object(package, "build_source", return_value=self.evidence.source):
            package.prepare(self.approval, self.root, self.icon, destination or self.destination, self.root)

    def sign(self, key=None):
        package.sign(self.approval, self.destination / package.MANIFEST, key or self.private, self.signature, self.openssl)

    def verify(self, approval=None, key=None):
        package.verify(approval or self.approval, self.destination, self.signature, key or self.public, self.openssl)

    def test_deterministic_retry_and_separate_signer(self):
        self.prepare()
        with self.assertRaisesRegex(ValueError, "signer unavailable"):
            self.sign(self.key_dir / "unavailable.pem")
        self.assertFalse(self.signature.exists())
        other = self.root / "second-attempt"
        first = (self.destination / package.MANIFEST).read_bytes()
        self.prepare()
        self.prepare(other)
        self.assertEqual(first, (other / package.MANIFEST).read_bytes())
        self.sign()
        raw = self.signature.read_bytes()
        self.sign()
        self.assertEqual(raw, self.signature.read_bytes())
        self.verify()
        for arch in package.ARCH:
            with ZipFile(self.destination / package.package_name(arch)) as archive:
                self.assertEqual(archive.namelist(), [package.CLIENT, package.PROFILE, package.ICON])
                config = package.parse_json(archive.read(package.PROFILE))
                self.assertEqual(config["center"], self.approval["center"])
                self.assertNotIn("group_ids", config)

    def test_rejects_signature_tampering_and_wrong_trust_root(self):
        self.prepare()
        self.sign()
        _, wrong = self.keys("untrusted")
        with self.assertRaisesRegex(ValueError, "signature invalid"):
            self.verify(key=wrong)
        self.signature.write_bytes(b"0" * 64)
        with self.assertRaisesRegex(ValueError, "signature invalid"):
            self.verify()

    def test_rejects_package_swap_and_keeps_verified_prior_candidate(self):
        self.prepare()
        self.sign()
        before = (self.destination / package.MANIFEST).read_bytes()
        alternate = self.root / "alternate"
        self.prepare(alternate)
        source = alternate / package.package_name("arm64")
        destination = alternate / package.package_name("x64")
        destination.write_bytes(source.read_bytes())
        with self.assertRaisesRegex(ValueError, "package digest mismatch"):
            self.prepare(alternate)
        manifest = alternate / package.MANIFEST
        records = package.check_manifest(manifest.read_bytes(), self.approval)
        records["x64"].update({"sha256": package.sha_file(destination), "bytes": destination.stat().st_size})
        manifest.write_bytes(package.manifest_bytes(self.approval, records))
        with self.assertRaisesRegex(ValueError, "packaged client differs from approved build"):
            self.prepare(alternate)
        self.verify()
        self.assertEqual((self.destination / package.MANIFEST).read_bytes(), before)

    def test_rejects_changed_approval_or_embedded_secret(self):
        self.prepare()
        self.sign()
        changed = copy.deepcopy(self.approval)
        changed["group_ids"] = ["10000000-0000-4000-8000-000000000002"]
        with self.assertRaisesRegex(ValueError, "differs from the approved release"):
            self.verify(approval=changed)
        changed["center"]["enrollment_token"] = "fixture-secret"
        with self.assertRaisesRegex(ValueError, "public center fields"):
            package.check_approval(changed)

    def test_stops_wrong_build_provenance_before_candidate_creation(self):
        with patch.object(package, "build_source", return_value={"commit": "c" * 40}):
            with self.assertRaisesRegex(ValueError, "approved source does not match"):
                package.prepare(self.approval, self.root, self.icon, self.destination, self.root)
        approved = self.evidence.path(package.ARCH["arm64"]) / "manifest.json"
        approved.write_bytes(approved.read_bytes() + b" ")
        with self.assertRaisesRegex(ValueError, "approved build manifest digest mismatch"):
            self.prepare()
        self.assertFalse(self.destination.exists())

    def test_rejects_unsafe_windows_zip_members(self):
        for name in ("NUL.txt", "dir /file", "../outside", "two\x00names", "drive:thing"):
            path = self.root / "unsafe.zip"
            with ZipFile(path, "w") as archive:
                archive.writestr(name, b"test")
            if "\x00" in name:
                # zipfile truncates embedded NUL when writing. Patch raw names
                # of equal byte width to exercise the parser's orig_filename.
                data = path.read_bytes().replace(b"two", b"t\x00o")
                path.write_bytes(data)
            with self.assertRaisesRegex(ValueError, "unsafe Windows client entry"):
                package.safe_build_zip(path)
            path.unlink()


if __name__ == "__main__":
    unittest.main()
