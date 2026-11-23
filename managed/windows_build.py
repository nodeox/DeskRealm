#!/usr/bin/env python3
"""Build a native managed Windows bundle and verify candidate-bound evidence."""

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shutil
import stat
import struct
import subprocess
import sys
from zipfile import BadZipFile, ZipFile, ZIP_DEFLATED


ROOT = Path(__file__).resolve().parents[1]
TARGETS = {
    "x86_64-pc-windows-msvc": {
        "arch": "x64", "host": "AMD64", "machine": 0x8664,
        "flutter": "3.24.5", "bridge": "3.22.3", "llvm": "15.0.6",
    },
    "aarch64-pc-windows-msvc": {
        "arch": "arm64", "host": "ARM64", "machine": 0xAA64,
        "flutter": "3.44.9", "bridge": "3.44.8", "llvm": "20.1.8",
    },
}
RUST_VERSION = "1.75.0"
VCPKG_COMMIT = "9e593bb18ea69cc5095e012465dcd675a822ed0d"
BRIDGE_FILES = (
    "src/bridge_generated.rs",
    "src/bridge_generated.io.rs",
    "flutter/lib/generated_bridge.dart",
    "flutter/lib/generated_bridge.freezed.dart",
)
PE_FILES = ("rustdesk.exe", "librustdesk.dll", "flutter_windows.dll", "dylib_virtual_display.dll")
FLUTTER_DATA = ("data/icudtl.dat", "data/app.so", "data/flutter_assets/AssetManifest.bin")
CLIENT_TESTS = (
    "managed_view_does_not_dispatch_legacy_or_other_capabilities",
    "managed_view_dispatches_only_the_authorized_message_scope",
    "managed_input_discards_stale_queued_events_and_releases_held_buttons",
    "managed_audio_requires_independent_permission_and_valid_format",
    "managed_audio_keeps_format_with_each_bounded_packet_and_drops_stale_audio",
    "managed_clipboard_validates_text_and_discards_stale_epochs",
    "managed_file_paths_and_publication_fail_closed",
    "managed_file_driver_moves_bytes_and_audits_cancel_and_failure",
)
LEASE_TESTS = (
    "managed_lease_checks_each_action_and_denies_all_at_expiry",
    "managed_lease_window_expires_during_suspend_and_ignores_clock_rollback",
    "managed_lease_renewal_preserves_first_attempt_bound_and_rejects_late_reply",
    "managed_lease_renewal_requires_exact_identity_sequence_and_scope",
)
RESUME_TESTS = ("resume_requires_revalidation_before_any_access",)
LOGS = ("vcpkg-build.log", "cargo-build.log", "desktop-tests.log", "lease-tests.log", "resume-tests.log", "flutter-build.log")
EVIDENCE_FILES = ("client.zip", "bridge.json", *LOGS)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def command_line(args):
    first, *rest = args
    return [shutil.which(str(first)) or first, *rest]


def output(args, cwd=ROOT):
    return subprocess.check_output(command_line(args), cwd=cwd, stderr=subprocess.STDOUT).decode("utf-8").strip()


def git(*args, cwd=ROOT):
    return output(["git", *args], cwd)


def sha_stream(data):
    digest = hashlib.sha256()
    for block in iter(lambda: data.read(1024 * 1024), b""):
        digest.update(block)
    return digest.hexdigest()


def sha_file(path):
    with path.open("rb") as data:
        return sha_stream(data)


def sha_format(value):
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value)


def source_identity():
    tracked = [
        "managed/upstream.lock", "managed/patches", "managed/native/deskrealm_protocol.rs",
        "managed/windows_build.py", "managed/tests/windows_build_test.py", "scripts/deskrealm-managed-source", "scripts/deskrealm-m2-test",
        ".github/workflows/deskrealm-windows.yml",
    ]
    require(not git("status", "--porcelain=v1", "--untracked-files=all", "--", *tracked),
            "commit the Windows build inputs before recording or verifying evidence")
    files = git("ls-files", "--", *tracked).splitlines()
    # Hash committed bytes and also require the bytes used for the build to match.
    digests = {}
    for name in files:
        blob = subprocess.check_output(["git", "show", f"HEAD:{name}"], cwd=ROOT)
        digest = hashlib.sha256(blob).hexdigest()
        require(sha_file(ROOT / name) == digest, f"build input bytes differ from Git: {name}")
        digests[name] = digest
    return {"commit": git("rev-parse", "HEAD"), "inputs": digests}


def upstream_identity():
    client = json.loads((ROOT / "managed/upstream.lock").read_text(encoding="utf-8"))["repositories"]["client"]
    return {"client": client["commit"], "hbb_common": client["submodules"]["libs/hbb_common"]["commit"]}


def prepare(work):
    subprocess.run([sys.executable, ROOT / "scripts/deskrealm-managed-source",
                    "prepare", work, "--apply-patches"], check=True)
    return work / "client"


def json_file(path):
    return json.loads(path.read_text(encoding="utf-8"))


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def copy_files(source, destination, names):
    for name in names:
        require((source / name).is_file() and (source / name).stat().st_size > 0,
                f"required generated file is missing: {name}")
        path = destination / name
        path.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source / name, path)


def flutter_version():
    # Flutter may print first-run notices even in machine mode.
    stdout = subprocess.check_output(command_line(["flutter", "--version", "--machine"]),
                                     cwd=ROOT).decode("utf-8")
    decoder = json.JSONDecoder()
    for start in re.finditer(r"^\s*\{", stdout, re.MULTILINE):
        try:
            report, _ = decoder.raw_decode(stdout, start.end() - 1)
        except json.JSONDecodeError:
            continue
        if isinstance(report, dict) and report.get("frameworkVersion") and report.get("engineRevision"):
            return report
    raise ValueError("Flutter did not report its framework version and engine revision")


def apply_flutter_source(client, target):
    if TARGETS[target]["arch"] == "arm64":
        subprocess.run(["bash", ".github/patches/apply_flutter_3.44_source_patches.sh"],
                       cwd=client, check=True)


def build_bridge(target, work, destination):
    source = source_identity()
    version = flutter_version()["frameworkVersion"]
    require(version == TARGETS[target]["bridge"], "wrong Flutter SDK for bridge generation")
    client = prepare(work)
    if TARGETS[target]["arch"] == "x64":
        # The pinned RustDesk bridge recipe uses the older compatible extended_text with Dart 3.4.
        spec = client / "flutter/pubspec.yaml"
        text = spec.read_text(encoding="utf-8")
        require(text.count("extended_text: 14.0.0") == 1, "upstream bridge dependency changed")
        spec.write_text(text.replace("extended_text: 14.0.0", "extended_text: 13.0.0"), encoding="utf-8")
    else:
        apply_flutter_source(client, target)
    subprocess.run(["flutter", "pub", "get"], cwd=client / "flutter", check=True)
    subprocess.run(["flutter_rust_bridge_codegen", "--rust-input", "src/flutter_ffi.rs", "--dart-output",
                    "flutter/lib/generated_bridge.dart", "--c-output", "flutter/macos/Runner/bridge_generated.h"],
                   cwd=client, check=True)
    destination.mkdir(parents=True, exist_ok=False)
    copy_files(client, destination, BRIDGE_FILES)
    write_json(destination / "bridge.json", {
        "schema": 1, "source": source, "target": target, "flutter": version,
        "generator": output(["flutter_rust_bridge_codegen", "--version"]),
        "files": {name: sha_file(destination / name) for name in BRIDGE_FILES},
    })


def check_bridge(report, target, source):
    require(report["schema"] == 1 and report["source"] == source and report["target"] == target,
            "bridge belongs to another source commit or Windows target")
    require(report["flutter"] == TARGETS[target]["bridge"] and "1.80.1" in report["generator"],
            "bridge toolchain does not match the pinned recipe")
    require(set(report["files"]) == set(BRIDGE_FILES) and all(map(sha_format, report["files"].values())),
            "bridge manifest does not cover the required files")


def pe_machine(stream, label):
    header = stream.read(64)
    require(len(header) == 64 and header[:2] == b"MZ", f"{label}: missing DOS header")
    (offset,) = struct.unpack_from("<I", header, 60)
    require(64 <= offset <= 1024 * 1024, f"{label}: invalid PE offset")
    require(len(stream.read(offset - 64)) == offset - 64, f"{label}: truncated PE header")
    pe = stream.read(26)
    require(len(pe) == 26 and pe[:4] == b"PE\0\0" and pe[24:] == b"\x0b\x02",
            f"{label}: missing PE32+ header")
    return struct.unpack_from("<H", pe, 4)[0]


def archive_tree(directory, destination):
    with ZipFile(destination, "x", compression=ZIP_DEFLATED, compresslevel=3) as archive:
        for path in sorted(directory.rglob("*")):
            require(not path.is_symlink(), f"symbolic link in Windows bundle: {path.name}")
            if path.is_file():
                archive.write(path, path.relative_to(directory).as_posix())


def check_bundle(path, target, rust_library_sha256):
    with ZipFile(path) as archive:
        contents = set()
        for info in archive.infolist():
            name = PurePosixPath(info.filename)
            require(not name.is_absolute() and ".." not in name.parts and "\\" not in info.filename
                    and ":" not in info.filename and name.as_posix().casefold() not in contents
                    and not stat.S_ISLNK(info.external_attr >> 16),
                    f"unsafe or duplicate bundle entry: {info.filename}")
            contents.add(name.as_posix().casefold())
        for name in (*PE_FILES, *FLUTTER_DATA):
            require(name.casefold() in contents and archive.getinfo(name).file_size > 0,
                    f"Windows bundle is missing {name}")
        for name in PE_FILES:
            with archive.open(name) as binary:
                require(pe_machine(binary, name) == TARGETS[target]["machine"],
                        f"{name}: PE architecture does not match {target}")
        with archive.open("librustdesk.dll") as library:
            require(sha_stream(library) == rust_library_sha256,
                    "packaged Rust library differs from the managed build")


def check_toolchains(tools, target):
    spec = TARGETS[target]
    require(tools["windows_host"] == spec["host"] and tools["rust_host"] == target
            and tools["rust_release"] == RUST_VERSION, "wrong Windows or Rust host")
    require(tools["flutter"]["frameworkVersion"] == spec["flutter"]
            and tools["flutter"].get("engineRevision")
            and tools["flutter"].get("dartSdkVersion")
            and f"windows_{spec['arch']}" in tools["dart"], "wrong Flutter or Dart SDK")
    require(tools["vcpkg_commit"] == VCPKG_COMMIT and tools["triplet"] == f"{spec['arch']}-windows-static",
            "wrong vcpkg checkout or triplet")
    require(f"clang version {spec['llvm']}" in tools["clang"]
            and tools["msvc"] and tools["windows_sdk"], "missing native LLVM/MSVC/SDK version")


def inspect_toolchains(target):
    spec = TARGETS[target]
    require(platform.system() == "Windows", "a native Windows host is required for this build")
    require(platform.machine().upper() == spec["host"], f"build requires native {spec['host']} Windows")
    rust = dict(line.split(": ", 1) for line in output(["rustc", "-vV"]).splitlines() if ": " in line)
    vcpkg = Path(os.environ["VCPKG_ROOT"])
    clang = Path(os.environ["LIBCLANG_PATH"])
    with (clang / "libclang.dll").open("rb") as library:
        require(pe_machine(library, "libclang.dll") == spec["machine"], "libclang must be native")
    tools = {
        "windows_host": platform.machine().upper(),
        "rust_host": rust["host"], "rust_release": rust["release"], "rust_commit": rust["commit-hash"],
        "flutter": flutter_version(), "dart": output(["dart", "--version"]),
        "clang": output([clang / "clang.exe", "--version"]),
        "msvc": os.environ.get("VCToolsVersion"), "windows_sdk": os.environ.get("WindowsSDKVersion"),
        "vcpkg_commit": git("rev-parse", "HEAD", cwd=vcpkg),
        "triplet": f"{spec['arch']}-windows-static",
    }
    check_toolchains(tools, target)
    return tools


def log_command(args, cwd, path, env=None):
    command = subprocess.list2cmdline([str(arg) for arg in args])
    print(command, flush=True)
    with path.open("wb") as log:
        log.write(f"Command: {command}\n".encode("utf-8"))
        log.flush()
        result = subprocess.run(command_line(args), cwd=cwd, stdout=log, stderr=subprocess.STDOUT, env=env)
        log.write(f"\nExit code: {result.returncode}\n".encode("utf-8"))
    if result.returncode:
        print(path.read_text(encoding="utf-8", errors="replace")[-12000:], file=sys.stderr)
        result.check_returncode()


def check_build_log(path):
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        if not line.startswith("{"):
            continue
        artifact = json.loads(line)
        if (artifact.get("reason") == "compiler-artifact"
                and artifact["target"]["name"] == "librustdesk"
                and "deskrealm-managed" in artifact["features"]
                and any(Path(file).name == "librustdesk.dll" or file.endswith("\\librustdesk.dll")
                        for file in artifact.get("filenames", []))):
            return
    raise ValueError("Cargo log has no managed librustdesk.dll build")


def check_tests(path, names):
    log = path.read_text(encoding="utf-8", errors="replace")
    require(re.search(r"test result: ok\. [1-9][0-9]* passed; 0 failed", log),
            f"{path.name}: missing successful test summary")
    for name in names:
        require(re.search(rf"^test (?:[\w]+::)*{re.escape(name)} \.\.\. ok\s*$", log, re.MULTILINE),
                f"{path.name}: missing passing test {name}")


def build_windows(target, work, bridge, evidence):
    tools = inspect_toolchains(target)
    source = source_identity()
    generated = json_file(bridge / "bridge.json")
    check_bridge(generated, target, source)
    for name, digest in generated["files"].items():
        require(sha_file(bridge / name) == digest, f"wrong generated bridge bytes: {name}")

    destination = evidence / target / source["commit"]
    destination.mkdir(parents=True, exist_ok=False)
    client = prepare(work)
    upstream = upstream_identity()
    require(git("rev-parse", "HEAD", cwd=client) == upstream["client"]
            and git("rev-parse", "HEAD", cwd=client / "libs/hbb_common") == upstream["hbb_common"],
            "prepared source does not match the upstream lock")
    cargo_lock = sha_file(client / "Cargo.lock")
    apply_flutter_source(client, target)
    copy_files(bridge, client, BRIDGE_FILES)
    if TARGETS[target]["arch"] == "x64":
        flutter_bat = shutil.which("flutter.bat")
        require(flutter_bat, "Flutter SDK is missing from PATH")
        patch = client / ".github/patches/flutter_3.24.4_dropdown_menu_enableFilter.diff"
        git("apply", str(patch), cwd=Path(flutter_bat).parents[1])

    vcpkg = Path(os.environ["VCPKG_ROOT"])
    triplet = tools["triplet"]
    env = {**os.environ, "VCPKG_DEFAULT_HOST_TRIPLET": triplet, "CARGO_NET_GIT_FETCH_WITH_CLI": "true"}
    env.pop("CARGO_BUILD_TARGET", None)
    env["CARGO_TARGET_DIR"] = str(client / "target")
    if TARGETS[target]["arch"] == "arm64":
        env["SODIUM_LIB_DIR"] = str(vcpkg / "installed" / triplet / "lib")
    log_command([vcpkg / "vcpkg.exe", "install", "--triplet", triplet,
                 f"--x-install-root={vcpkg / 'installed'}"], client, destination / "vcpkg-build.log", env)

    build = [sys.executable, "build.py", "--portable", "--flutter", "--skip-portable-pack", "--hwcodec"]
    if TARGETS[target]["arch"] == "x64":
        build.append("--vram")
    base_features = subprocess.check_output([*build, "--print-features"], cwd=client).decode("utf-8").strip()
    require(re.fullmatch(r"[\w,-]+", base_features) and "flutter" in base_features.split(","),
            "unexpected upstream Flutter build features")
    features = base_features + ",deskrealm-managed"
    cargo = ["cargo", "build", "--locked", "--lib", "--release", "--features", features]
    log_command([*cargo, "--message-format=json-render-diagnostics"], client, destination / "cargo-build.log", env)
    check_build_log(destination / "cargo-build.log")
    rust_library = client / "target/release/librustdesk.dll"
    require(rust_library.is_file(), "expected a native Windows Rust library")
    log_command(["cargo", "test", "--locked", "--lib", "--release", "--features", features, "managed_"],
                client, destination / "desktop-tests.log", env)
    log_command(["cargo", "test", "--locked", "-p", "base", "--lib", "managed_lease_"],
                client, destination / "lease-tests.log", env)
    log_command(["cargo", "test", "--locked", "-p", "base", "--lib", RESUME_TESTS[0]],
                client, destination / "resume-tests.log", env)
    log_command([*build, "--skip-cargo"], client, destination / "flutter-build.log", env)

    bundle = client / f"flutter/build/windows/{TARGETS[target]['arch']}/runner/Release"
    require(sha_file(bundle / "librustdesk.dll") == sha_file(rust_library),
            "Flutter packaged a different Rust library")
    require(sha_file(client / "Cargo.lock") == cargo_lock, "locked Rust dependencies changed during the build")
    archive_tree(bundle, destination / "client.zip")
    shutil.copyfile(bridge / "bridge.json", destination / "bridge.json")
    write_json(destination / "manifest.json", {
        "schema": 1, "source": source, "upstream": upstream, "target": target, "toolchains": tools,
        "cargo_lock_sha256": cargo_lock, "flutter_lock_sha256": sha_file(client / "flutter/pubspec.lock"),
        "rust_library_sha256": sha_file(rust_library),
        "files": {name: sha_file(destination / name) for name in EVIDENCE_FILES},
    })
    verify_target(destination, target, source)


def verify_target(directory, target, source):
    report = json_file(directory / "manifest.json")
    require(report["schema"] == 1 and report["source"] == source
            and report["upstream"] == upstream_identity() and report["target"] == target,
            "manifest is for different source or target")
    check_toolchains(report["toolchains"], target)
    require(sha_format(report["cargo_lock_sha256"]) and sha_format(report["flutter_lock_sha256"]),
            "missing dependency lock digests")
    require(set(report["files"]) == set(EVIDENCE_FILES), "incomplete build or test evidence")
    for name, digest in report["files"].items():
        require(sha_file(directory / name) == digest, f"artifact digest mismatch: {name}")
    for name in LOGS:
        with (directory / name).open("rb") as log:
            log.seek(-len(b"\nExit code: 0\n"), os.SEEK_END)
            require(log.read() == b"\nExit code: 0\n", f"{name}: missing zero exit code")
    check_bridge(json_file(directory / "bridge.json"), target, source)
    check_bundle(directory / "client.zip", target, report["rust_library_sha256"])
    check_build_log(directory / "cargo-build.log")
    check_tests(directory / "desktop-tests.log", CLIENT_TESTS)
    check_tests(directory / "lease-tests.log", LEASE_TESTS)
    check_tests(directory / "resume-tests.log", RESUME_TESTS)
    print(f"{target}: native client bundle, architecture and managed test evidence verified", flush=True)


def verify_matrix(evidence):
    source = source_identity()
    failed = []
    for target in TARGETS:
        try:
            verify_target(evidence / target / source["commit"], target, source)
        except (OSError, ValueError, KeyError, TypeError, BadZipFile) as error:
            failed.append(target)
            print(f"{target}: {error}", file=sys.stderr)
    return 1 if failed else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_subparsers(dest="mode", required=True)
    for name in ("bridge", "build"):
        command = modes.add_parser(name)
        command.add_argument("target", choices=TARGETS)
        command.add_argument("work", type=Path)
        command.add_argument("output", type=Path)
        if name == "build":
            command.add_argument("--bridge", required=True, type=Path)
    modes.add_parser("verify").add_argument("directory", type=Path)
    args = parser.parse_args()
    try:
        if args.mode == "bridge":
            build_bridge(args.target, args.work.absolute(), args.output.absolute())
        elif args.mode == "build":
            build_windows(args.target, args.work.absolute(), args.bridge.absolute(), args.output.absolute())
        else:
            return verify_matrix(args.directory.absolute())
    except (OSError, ValueError, KeyError, TypeError, BadZipFile, subprocess.CalledProcessError) as error:
        print(f"DeskRealm Windows build: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
