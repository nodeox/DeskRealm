#!/usr/bin/env python3
"""Build and independently verify Authenticode DeskRealm installers from approved ZIPs."""

import argparse
from contextlib import redirect_stdout
import hashlib
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from zipfile import BadZipFile, ZipFile

import windows_build as build
import windows_package as delivery
from windows_build import require


SETUP = "./cmd/deskrealm-windows-setup"
MAX_SIGNED_INSTALLER = delivery.MAX_ZIP + (80 << 20)
ATTESTATION_PREFIX = b"DeskRealm/windows-installer-attestation/v1\x00"
STATEMENT = "installer-verification.json"
SIGNATURE = "installer-verification.sig"
INPUTS = (
    "managed/windows_installer.py", "managed/windows_package.py", "managed/windows_build.py",
    "cmd/deskrealm-windows-setup", "internal/windowssetup", "internal/windowsdelivery", "go.mod", "go.sum",
)


def setup_name(arch):
    return f"DeskRealm-Setup-{arch}.exe"


def source_commit():
    require(not build.git("status", "--porcelain=v1", "--untracked-files=all", "--", *INPUTS),
            "commit the installer inputs before building or verifying")
    return build.git("rev-parse", "HEAD")


def verify_client(package, arch):
    spec = build.TARGETS[delivery.ARCH[arch]]
    with delivery.open_regular(package) as file, ZipFile(file) as outer, outer.open(delivery.CLIENT) as packed:
        delivery.safe_build_zip(packed)
        with ZipFile(packed) as inner:
            for name in build.PE_FILES:
                try:
                    with inner.open(name) as binary:
                        require(build.pe_machine(binary, name) == spec["machine"], f"{arch}: wrong client PE architecture")
                except KeyError as error:
                    raise ValueError(f"{arch}: installer payload is missing {name}") from error


def snapshot(source, destination, limit):
    digest = hashlib.sha256()
    with delivery.open_regular(source) as incoming, destination.open("xb") as out:
        size = os.fstat(incoming.fileno()).st_size
        require(0 < size <= limit, f"invalid file size: {source.name}")
        remaining = size
        for chunk in iter(lambda: incoming.read(min(remaining + 1, 1 << 20)), b""):
            remaining -= len(chunk)
            require(remaining >= 0, f"file grew during verification: {source.name}")
            digest.update(chunk)
            out.write(chunk)
        require(remaining == 0, f"file shrank during verification: {source.name}")
    return size, digest.hexdigest()


def matches_file(path, size, digest):
    with delivery.open_regular(path) as present:
        return os.fstat(present.fileno()).st_size == size and build.sha_stream(present) == digest


def prepare(approval, candidate, signature, key, stage, work, openssl):
    with redirect_stdout(sys.stderr):
        release, records = delivery.verify(approval, candidate, signature, key, openssl)
    env = {**os.environ, "GOTOOLCHAIN": "local", "GOWORK": "off", "GOFLAGS": "", "CGO_ENABLED": "0"}
    for name in ("GOOS", "GOARCH", "GOAMD64", "GOARM64", "GOEXPERIMENT"):
        env.pop(name, None)
    version = subprocess.check_output(["go", "env", "GOVERSION"], cwd=build.ROOT, env=env, text=True).strip()
    require(version == build.GO_VERSION, f"installers require {build.GO_VERSION}")
    host = work / "assemble"
    if os.name == "nt":
        host = host.with_suffix(".exe")
    args = ["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false"]
    subprocess.run([*args, "-o", str(host), SETUP], cwd=build.ROOT, env=env, check=True)
    for arch, record in records.items():
        package = work / record["file"]
        size, digest = snapshot(candidate / record["file"], package, record["bytes"])
        require(size == record["bytes"] and digest == record["sha256"],
                f"{arch}: package changed during preparation")
        verify_client(package, arch)
        stub = work / f"stub-{arch}.exe"
        target = {"x64": "amd64", "arm64": "arm64"}[arch]
        subprocess.run([*args, "-o", str(stub), SETUP], cwd=build.ROOT,
                       env={**env, "GOOS": "windows", "GOARCH": target}, check=True)
        subprocess.run([str(host), "assemble", arch, str(stub), str(package), record["sha256"],
                        str(stage / setup_name(arch))], check=True)
    return host, hashlib.sha256(release).hexdigest()


def verify(signed, unsigned, host, ca, signer, authenticode, openssl):
    der = subprocess.check_output([openssl, "x509", "-in", str(signer), "-outform", "DER"])
    signer_sha = hashlib.sha256(der).hexdigest()
    installers = {}
    with tempfile.TemporaryDirectory(prefix="deskrealm-installer-verified-") as work:
        for arch in delivery.ARCH:
            name = setup_name(arch)
            submitted = signed / name
            checked = Path(work) / name
            size, digest = snapshot(submitted, checked, MAX_SIGNED_INSTALLER)
            subprocess.run([str(host), "check", arch, str(unsigned / name), str(checked)], check=True)
            verified = subprocess.run([authenticode, "verify", "-CAfile", str(ca), "-require-leaf-hash", f"sha256:{signer_sha}",
                                       "-in", str(checked)], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                                      encoding="utf-8", errors="replace")
            print(verified.stdout, file=sys.stderr, end="")
            verified.check_returncode()
            digests = re.findall(r"^[ \t]*Message digest algorithm[ \t]*:[ \t]*(\w+)[ \t]*$", verified.stdout, re.MULTILINE)
            require(len(digests) >= 2 and set(digests) == {"SHA256"},
                    f"{arch}: Authenticode and image digests must use SHA-256")
            require(matches_file(submitted, size, digest), f"{arch}: installer changed during verification")
            installers[arch] = {"file": name, "sha256": digest, "bytes": size}
    return {"signer_sha256": signer_sha, "installers": installers}


def stage_attestation(statement, signed, key, release_key, directory, openssl):
    public = subprocess.check_output([openssl, "pkey", "-in", str(key), "-pubout", "-outform", "DER"])
    release_public = subprocess.check_output([openssl, "pkey", "-pubin", "-in", str(release_key), "-outform", "DER"])
    require(len(public) == 44 and public[:12] == bytes.fromhex("302a300506032b6570032100")
            and public != release_public, "installer attestation requires a separate Ed25519 key")
    raw = delivery.json_bytes(statement)
    expected = hashlib.sha256(raw).hexdigest()
    destination = directory / expected
    with tempfile.TemporaryDirectory(prefix=".deskrealm-attestation-", dir=directory) as work:
        work = Path(work)
        stage = work / "stage"
        stage.mkdir()
        for pkg in statement["installers"].values():
            size, digest = snapshot(signed / pkg["file"], stage / pkg["file"], pkg["bytes"])
            require(size == pkg["bytes"] and digest == pkg["sha256"], "installer changed after verification")
        message = work / "message"
        message.write_bytes(ATTESTATION_PREFIX + raw)
        signature = subprocess.check_output([openssl, "pkeyutl", "-sign", "-rawin", "-inkey", str(key), "-in", str(message)])
        require(len(signature) == 64, "invalid installer attestation signature")
        (stage / STATEMENT).write_bytes(raw)
        (stage / SIGNATURE).write_bytes(signature)
        try:
            stage.rename(destination)
        except OSError:
            require(destination.is_dir() and not destination.is_symlink()
                    and delivery.read_small(destination / STATEMENT) == raw
                    and delivery.read_small(destination / SIGNATURE, 64) == signature
                    and all(matches_file(destination / p["file"], p["bytes"], p["sha256"])
                            for p in statement["installers"].values()), "attestation already exists with different contents")
    return expected


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--openssl", default="openssl")
    modes = parser.add_subparsers(dest="mode", required=True)
    for mode in ("prepare", "verify"):
        command = modes.add_parser(mode)
        for field in ("approval", "candidate", "signature", "release_key", "directory"):
            command.add_argument(field, type=Path)
        if mode == "verify":
            command.add_argument("code_signing_ca", type=Path)
            command.add_argument("code_signing_cert", type=Path)
            command.add_argument("--osslsigncode", default="osslsigncode")
            command.add_argument("--attestation-key", type=Path)
            command.add_argument("--attestation-directory", type=Path)
    args = parser.parse_args()
    try:
        commit = source_commit()
        approval = delivery.approved(args.approval)
        destination = args.directory.absolute()
        if args.mode == "prepare":
            require(not destination.exists() and not destination.is_symlink(), "installer output directory already exists")
        parent = destination.parent if args.mode == "prepare" else None
        with tempfile.TemporaryDirectory(prefix="deskrealm-installer-", dir=parent) as tmp:
            work = Path(tmp)
            stage = work / "unsigned"
            stage.mkdir()
            host, release_sha = prepare(approval, args.candidate.absolute(), args.signature.absolute(),
                                        args.release_key.absolute(), stage, work, args.openssl)
            require(source_commit() == commit, "installer source changed during preparation")
            if args.mode == "prepare":
                stage.rename(destination)
            else:
                require(bool(args.attestation_key) == bool(args.attestation_directory),
                        "provide the attestation key and output directory together")
                evidence = verify(destination, stage, host, args.code_signing_ca.absolute(),
                                  args.code_signing_cert.absolute(), args.osslsigncode, args.openssl)
                statement = {"schema": 1, "scope": "authenticode-development", "source_commit": commit,
                             "release_sha256": release_sha, **evidence}
                if args.attestation_key:
                    expected = stage_attestation(statement, destination, args.attestation_key.absolute(),
                                                 args.release_key.absolute(), args.attestation_directory.absolute(), args.openssl)
                    print(delivery.json_bytes({"evidence_sha256": expected, **statement}).decode().rstrip())
                else:
                    print(delivery.json_bytes(statement).decode().rstrip())
    except (OSError, ValueError, TypeError, KeyError, BadZipFile, subprocess.CalledProcessError) as error:
        print(f"DeskRealm Windows installer: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
