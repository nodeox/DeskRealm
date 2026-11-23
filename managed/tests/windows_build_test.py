"""Exercise the evidence verifier with disposable synthetic bundles."""

import contextlib
import hashlib
import io
import json
from pathlib import Path
import struct
import sys
import tempfile
import unittest
from unittest.mock import patch
from zipfile import ZipFile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import windows_build as build


def pe_header(machine):
    dos = bytearray(64)
    dos[:2] = b"MZ"
    struct.pack_into("<I", dos, 60, 64)
    return dos + struct.pack("<4sH18xH", b"PE\0\0", machine, 0x20B)


class FlutterVersionTests(unittest.TestCase):
    def test_accepts_machine_json_among_flutter_notices(self):
        report = {"frameworkVersion": "3.44.8", "engineRevision": "example"}
        output = f"First-run notice\n{json.dumps(report)}\nSDK notice\n".encode()
        with patch.object(build.subprocess, "check_output", return_value=output):
            self.assertEqual(build.flutter_version(), report)

    def test_selects_bash_from_git_for_windows(self):
        with tempfile.TemporaryDirectory() as tmp:
            git_root = Path(tmp) / "Git"
            (git_root / "bin").mkdir(parents=True)
            (git_root / "cmd").mkdir()
            git = git_root / "cmd" / "git.exe"
            bash = git_root / "bin" / "bash.exe"
            git.touch()
            bash.touch()
            with patch.object(build.platform, "system", return_value="Windows"):
                with patch.object(build.shutil, "which", return_value=str(git)):
                    self.assertEqual(build.flutter_patch_shell(), bash.resolve())


class WindowsTestProfileTests(unittest.TestCase):
    def test_arm64_workaround_is_local_to_desktop_test_process(self):
        env = {"CARGO_TARGET_DIR": "target", "CARGO_PROFILE_RELEASE_LTO": "true"}
        arm64 = build.desktop_test_environment("aarch64-pc-windows-msvc", env)
        self.assertEqual(arm64["CARGO_PROFILE_RELEASE_OPT_LEVEL"], "0")
        self.assertEqual(arm64["CARGO_PROFILE_RELEASE_LTO"], "false")
        self.assertEqual(arm64["CARGO_PROFILE_RELEASE_CODEGEN_UNITS"], "16")
        self.assertEqual(arm64["CARGO_TARGET_DIR"], "target")
        self.assertEqual(env["CARGO_PROFILE_RELEASE_LTO"], "true")
        self.assertIs(build.desktop_test_environment("x86_64-pc-windows-msvc", env), env)


class WindowsEvidenceTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.source = {"commit": "a" * 40, "inputs": {"test": "b" * 64}}
        self.targets = tuple(build.TARGETS)
        self.patch_upstream = patch.object(build, "upstream_identity", return_value={"client": "c" * 40})
        self.upstream = self.patch_upstream.start().return_value
        self.addCleanup(self.patch_upstream.stop)
        for target in self.targets:
            self.create_evidence(target)

    def path(self, target):
        return self.root / target / self.source["commit"]

    def pack(self, path, machine):
        with ZipFile(path, "w") as archive:
            for name in build.PE_FILES:
                archive.writestr(name, pe_header(machine))
            for name in build.FLUTTER_DATA:
                archive.writestr(name, b"synthetic data")

    def create_evidence(self, target):
        spec = build.TARGETS[target]
        directory = self.path(target)
        directory.mkdir(parents=True)
        self.pack(directory / "client.zip", spec["machine"])
        build.write_json(directory / "bridge.json", {
            "schema": 1, "source": self.source, "target": target,
            "flutter": spec["bridge"], "generator": "flutter_rust_bridge_codegen 1.80.1",
            "files": dict.fromkeys(build.BRIDGE_FILES, "a" * 64),
        })
        artifact = {"reason": "compiler-artifact", "target": {"name": "librustdesk"},
                    "features": ["flutter", "deskrealm-managed"], "filenames": ["C:\\s\\librustdesk.dll"]}
        (directory / "cargo-build.log").write_text(json.dumps(artifact) + "\nExit code: 0\n", encoding="utf-8")
        for name, tests in (("desktop-tests.log", build.CLIENT_TESTS), ("lease-tests.log", build.LEASE_TESTS),
                            ("resume-tests.log", build.RESUME_TESTS)):
            log = "".join(f"test path::tests::{case} ... ok\n" for case in tests)
            log += f"test result: ok. {len(tests)} passed; 0 failed\n\nExit code: 0\n"
            (directory / name).write_text(log, encoding="utf-8")
        for name in ("flutter-build.log", "vcpkg-build.log"):
            (directory / name).write_bytes(b"synthetic log\nExit code: 0\n")
        build.write_json(directory / "manifest.json", {
            "schema": 1, "source": self.source, "upstream": self.upstream, "target": target,
            "cargo_lock_sha256": "c" * 64, "flutter_lock_sha256": "d" * 64,
            "rust_library_sha256": hashlib.sha256(pe_header(spec["machine"])).hexdigest(),
            "toolchains": {
                "windows_host": spec["host"], "rust_host": target, "rust_release": build.RUST_VERSION,
                "flutter": {"frameworkVersion": spec["flutter"], "engineRevision": "example", "dartSdkVersion": "x"},
                "dart": f"windows_{spec['arch']}", "vcpkg_commit": build.VCPKG_COMMIT,
                "triplet": f"{spec['arch']}-windows-static", "clang": f"clang version {spec['llvm']}",
                "msvc": "example", "windows_sdk": "example",
            },
            "files": {name: build.sha_file(directory / name) for name in build.EVIDENCE_FILES},
        })

    def rehash(self, target, name):
        directory = self.path(target)
        manifest = directory / "manifest.json"
        report = build.json_file(manifest)
        report["files"][name] = build.sha_file(directory / name)
        build.write_json(manifest, report)

    def verify(self, expected, reason=""):
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch.object(build, "source_identity", return_value=self.source):
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                self.assertEqual(build.verify_matrix(self.root), expected)
        self.assertIn(reason, stderr.getvalue())

    def test_requires_both_current_architectures(self):
        self.verify(0)
        (self.path(self.targets[1]) / "manifest.json").unlink()
        self.verify(1, self.targets[1])

    def test_rejects_wrong_architecture_even_with_updated_digest(self):
        target = self.targets[1]
        self.pack(self.path(target) / "client.zip", build.TARGETS[self.targets[0]]["machine"])
        self.rehash(target, "client.zip")
        self.verify(1, "PE architecture does not match")

    def test_rejects_modified_artifact_and_missing_named_test(self):
        target = self.targets[0]
        (self.path(target) / "desktop-tests.log").write_text("test result: ok. 8 passed; 0 failed\n\nExit code: 0\n")
        self.verify(1, "artifact digest mismatch")
        self.rehash(target, "desktop-tests.log")
        self.verify(1, "missing passing test")

    def test_rejects_bridge_from_different_candidate(self):
        target = self.targets[0]
        bridge = self.path(target) / "bridge.json"
        report = build.json_file(bridge)
        report["source"]["commit"] = "e" * 40
        build.write_json(bridge, report)
        self.rehash(target, "bridge.json")
        self.verify(1, "bridge belongs to another source commit")


if __name__ == "__main__":
    unittest.main()
