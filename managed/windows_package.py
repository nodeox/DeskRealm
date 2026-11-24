#!/usr/bin/env python3
"""Prepare, sign and independently verify a candidate Windows delivery bundle."""

import argparse
import base64
import binascii
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import stat
import struct
import subprocess
import sys
import tempfile
import unicodedata
from urllib.parse import urlsplit
from uuid import UUID
from zipfile import BadZipFile, ZipFile, ZipInfo, ZIP_STORED

import windows_build as build
from windows_build import require, sha_file, sha_format, sha_stream


ARCH = {spec["arch"]: target for target, spec in build.TARGETS.items()}
PUBLIC_FIELDS = ("public_origin", "id_server", "rendezvous_public_key", "signing_key_id", "signing_public_key")
PLATFORM = re.compile(r"[a-z0-9][a-z0-9_-]{0,31}")
VERSION = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[a-z0-9]+(?:[.-][a-z0-9]+)*)?")
LABEL = re.compile(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?")
COMMIT = re.compile(r"[0-9a-f]{40}")
MANIFEST = "release.json"
SIGNATURE = "release.sig"
CLIENT = "client.zip"
PROFILE = "deskrealm-public.json"
ICON = "deskrealm-brand.ico"
SMALL = 64 * 1024
MAX_ICON = 1024 * 1024
MAX_ZIP = 1024 * 1024 * 1024
SIGNING_PREFIX = b"DeskRealm/windows-delivery/v1\x00"


def json_bytes(value):
    return (json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n").encode()


def strict_object(pairs):
    result = {}
    for name, value in pairs:
        require(name not in result, f"duplicate JSON field: {name}")
        result[name] = value
    return result


def parse_json(raw):
    return json.loads(raw.decode("utf-8"), object_pairs_hook=strict_object)


def open_regular(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    stream = os.fdopen(fd, "rb")
    if not stat.S_ISREG(os.fstat(fd).st_mode):
        stream.close()
        raise ValueError(f"{path.name}: expected regular file")
    return stream


def read_small(path, size=SMALL):
    with open_regular(path) as stream:
        content = stream.read(size + 1)
    require(len(content) <= size, f"{path.name}: file is too large")
    return content


def check_fields(value, fields, label):
    require(isinstance(value, dict) and set(value) == set(fields), f"invalid {label} fields")


def key_bytes(text, urlsafe=False):
    require(isinstance(text, str) and len(text) == (43 if urlsafe else 44), "invalid center public key")
    raw = base64.b64decode(text + ("=" if urlsafe else ""), altchars=b"-_" if urlsafe else None, validate=True)
    canonical = base64.urlsafe_b64encode(raw).rstrip(b"=") if urlsafe else base64.b64encode(raw)
    require(len(raw) == 32 and any(raw) and canonical.decode() == text, "invalid center public key")
    return raw


def valid_host(host):
    if not host or len(host) > 253:
        return False
    try:
        return not ipaddress.ip_address(host).is_unspecified
    except ValueError:
        return all(LABEL.fullmatch(part) for part in host.split("."))


def check_address(text, origin=False):
    require(isinstance(text, str) and text.isascii() and not any(ord(c) <= 32 or c in "?#\\" for c in text),
            "invalid public center address")
    parsed = urlsplit(text if origin else "//" + text)
    host, port = parsed.hostname, parsed.port
    require(parsed.scheme == ("https" if origin else "") and valid_host(host)
            and not parsed.path and not parsed.query and not parsed.fragment
            and parsed.username is None and parsed.password is None
            and ((origin and port is None) or (port is not None and port > 0)),
            "expected HTTPS origin or host:port")
    authority = f"[{host}]" if ":" in host else host
    if port is not None:
        authority += f":{port}"
    require(parsed.netloc == authority, "public center address is not canonical")


def check_center(center):
    require(isinstance(center, dict), "invalid center configuration")
    fields = list(PUBLIC_FIELDS)
    if "default_relay" in center:
        fields.append("default_relay")
    check_fields(center, fields, "public center")
    check_address(center["public_origin"], origin=True)
    check_address(center["id_server"])
    require(isinstance(center["signing_key_id"], str) and PLATFORM.fullmatch(center["signing_key_id"]),
            "invalid center signing key ID")
    rendezvous = key_bytes(center["rendezvous_public_key"])
    signing = key_bytes(center["signing_public_key"], urlsafe=True)
    require(rendezvous != signing, "center and rendezvous public keys must be distinct")
    if "default_relay" in center:
        relay = center["default_relay"]
        check_fields(relay, ("id", "address"), "public relay")
        require(isinstance(relay["id"], str) and PLATFORM.fullmatch(relay["id"]), "invalid default relay ID")
        check_address(relay["address"])


def check_approval(approval):
    check_fields(approval, ("schema", "version", "brand", "center", "group_ids", "source_commit", "builds"), "approval")
    require(type(approval["schema"]) is int and approval["schema"] == 1, "unknown approval schema")
    require(isinstance(approval["version"], str) and len(approval["version"]) <= 64
            and VERSION.fullmatch(approval["version"]), "invalid delivery version")
    brand = approval["brand"]
    check_fields(brand, ("id", "revision", "name", "icon_sha256"), "brand")
    require(isinstance(brand["id"], str) and PLATFORM.fullmatch(brand["id"])
            and type(brand["revision"]) is int and 1 <= brand["revision"] <= (1 << 53) - 1
            and isinstance(brand["name"], str) and 1 <= len(brand["name"]) <= 64
            and brand["name"].strip() == brand["name"]
            and not any(unicodedata.category(c).startswith("C") for c in brand["name"])
            and sha_format(brand["icon_sha256"]), "invalid brand revision")
    check_center(approval["center"])
    groups = approval["group_ids"]
    require(isinstance(groups, list) and 1 <= len(groups) <= 50
            and all(isinstance(group, str) and str(UUID(group)) == group and UUID(group).int for group in groups)
            and groups == sorted(set(groups)), "expected sorted unique target group IDs")
    require(isinstance(approval["source_commit"], str) and COMMIT.fullmatch(approval["source_commit"]), "invalid build source commit")
    targets = approval["builds"]
    check_fields(targets, ARCH, "Windows architecture")
    for target in targets.values():
        check_fields(target, ("manifest_sha256", "client_sha256"), "build digest")
        require(all(sha_format(value) for value in target.values()), "invalid build digest")
    return approval


def approved(path):
    return check_approval(parse_json(read_small(path)))


def icon_bytes(path, expected):
    icon = read_small(path, MAX_ICON)
    check_icon(icon, expected)
    return icon


def check_icon(icon, expected):
    require(len(icon) >= 22 and icon[:4] == b"\x00\x00\x01\x00", "invalid Windows icon header")
    count = struct.unpack_from("<H", icon, 4)[0]
    table_end = 6 + 16 * count
    require(1 <= count <= 64 and table_end <= len(icon), "invalid Windows icon directory")
    for index in range(count):
        size, offset = struct.unpack_from("<II", icon, 6 + 16 * index + 8)
        require(size > 0 and table_end <= offset and offset + size <= len(icon), "invalid Windows icon image")
    require(hashlib.sha256(icon).hexdigest() == expected, "brand icon digest mismatch")


def profile(approval):
    return json_bytes({"schema": 1, "version": approval["version"],
                       "brand": approval["brand"], "center": approval["center"]})


def package_name(arch):
    return f"DeskRealm-{arch}.zip"


def manifest_bytes(approval, packages):
    return json_bytes({"schema": 1, "signature_scope": "development", "approval": approval, "packages": packages})


def check_manifest(raw, approval):
    manifest = parse_json(raw)
    check_fields(manifest, ("schema", "signature_scope", "approval", "packages"), "release manifest")
    require(type(manifest["schema"]) is int and manifest["schema"] == 1
            and manifest["signature_scope"] == "development", "unsupported signing scope")
    check_approval(manifest["approval"])
    require(manifest["approval"] == approval, "manifest differs from the approved release")
    packages = manifest["packages"]
    check_fields(packages, ARCH, "package architecture")
    for arch, package in packages.items():
        check_fields(package, ("file", "sha256", "bytes"), "package")
        require(package["file"] == package_name(arch) and sha_format(package["sha256"])
                and type(package["bytes"]) is int and 0 < package["bytes"] <= MAX_ZIP, "invalid package record")
    require(raw == manifest_bytes(approval, packages), "release manifest is not canonical")
    return packages


def safe_build_zip(path):
    """Reject entry names that can alias or escape when extracted on Windows."""
    blocked = re.compile(r"(?:con|prn|aux|nul|com[1-9¹²³]|lpt[1-9¹²³])", re.I)
    names = {}
    with ZipFile(path) as archive:
        entries = archive.infolist()
        require(0 < len(entries) <= 10_000 and sum(entry.file_size for entry in entries) <= 3 * MAX_ZIP,
                "Windows client archive exceeds extraction bounds")
        for entry in entries:
            raw = entry.orig_filename.removesuffix("/")
            parts = raw.split("/")
            for part in parts:
                require(part and part not in (".", "..") and part[-1] not in " ."
                        and not any(c in '\\:<>"|?*' or ord(c) < 32 for c in part)
                        and not blocked.fullmatch(part.split(".")[0].rstrip()), "unsafe Windows client entry")
            mode = stat.S_IFMT(entry.external_attr >> 16)
            require(mode in (0, stat.S_IFREG, stat.S_IFDIR), "unsupported Windows client entry type")
            folded = raw.casefold()
            require(folded not in names, "duplicate Windows client entry")
            names[folded] = entry.is_dir()
    for name in names:
        parents = name.split("/")[:-1]
        require(all(names.get("/".join(parents[:i]), True) for i in range(1, len(parents)+1)),
                "Windows client file conflicts with a directory")


def archive_info(name):
    info = ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
    info.create_system = 3
    info.external_attr = (stat.S_IFREG | 0o644) << 16
    return info


def copy_bundle(source, destination, expected, public, icon):
    with open_regular(source) as client, ZipFile(destination, "x", compression=ZIP_STORED) as package:
        require(0 < os.fstat(client.fileno()).st_size < MAX_ZIP, "Windows client bundle exceeds size limit")
        with package.open(archive_info(CLIENT), "w", force_zip64=True) as out:
            shutil.copyfileobj(client, out, length=1024 * 1024)
        package.writestr(archive_info(PROFILE), public)
        package.writestr(archive_info(ICON), icon)
    require(destination.stat().st_size <= MAX_ZIP, "Windows delivery package exceeds size limit")
    check_package(destination, expected, public, hashlib.sha256(icon).hexdigest())


def check_package(path, expected, public, icon):
    with open_regular(path) as archive:
        check_package_contents(archive, expected, public, icon)


def check_package_contents(archive, expected, public, icon):
    with ZipFile(archive) as package:
        require(package.namelist() == [CLIENT, PROFILE, ICON], "unexpected delivery package contents")
        require(all(entry.orig_filename == entry.filename and entry.compress_type == ZIP_STORED
                    and stat.S_IFMT(entry.external_attr >> 16) == stat.S_IFREG and entry.flag_bits & 1 == 0
                    for entry in package.infolist()), "invalid delivery package entry attributes")
        require(package.getinfo(PROFILE).file_size == len(public) and package.read(PROFILE) == public,
                "packaged public profile differs from approval")
        require(package.getinfo(ICON).file_size <= MAX_ICON, "packaged icon exceeds size limit")
        check_icon(package.read(ICON), icon)
        require(0 < package.getinfo(CLIENT).file_size < MAX_ZIP, "invalid packaged client size")
        with package.open(CLIENT) as client:
            require(sha_stream(client) == expected, "packaged client differs from approved build")


def build_source(source_tree):
    require(not build.git("status", "--porcelain=v1", "--untracked-files=all", "--", *build.WINDOWS_INPUTS, cwd=source_tree),
            "Windows source inputs differ from the approved checkout")
    commit = build.git("rev-parse", "HEAD", cwd=source_tree)
    files = build.git("ls-tree", "-r", "--name-only", commit, "--", *build.WINDOWS_INPUTS, cwd=source_tree).splitlines()
    inputs = {}
    for filename in files:
        blob = subprocess.check_output(["git", "show", f"{commit}:{filename}"], cwd=source_tree)
        digest = hashlib.sha256(blob).hexdigest()
        require(sha_file(source_tree / filename) == digest, f"build input differs from Git: {filename}")
        inputs[filename] = digest
    return {"commit": commit, "inputs": inputs}


def prepare(approval, evidence, icon_path, destination, source_tree):
    icon = icon_bytes(icon_path, approval["brand"]["icon_sha256"])
    if destination.exists():
        require(destination.is_dir() and not destination.is_symlink(), "candidate destination must be a real directory")
        raw = read_small(destination / MANIFEST)
        check_artifacts(destination, approval, check_manifest(raw, approval))
        return
    # The verifier uses this repository for locked source inputs. The explicit
    # checkout permits an approved historical or CI build without treating it as HEAD.
    original_root = build.ROOT
    try:
        build.ROOT = source_tree
        source = build_source(source_tree)
        require(source["commit"] == approval["source_commit"], "approved source does not match its checkout")
        with tempfile.TemporaryDirectory(prefix=".deskrealm-package-", dir=destination.parent) as tmp:
            stage = Path(tmp) / "candidate"
            stage.mkdir()
            records = {}
            public = profile(approval)
            for arch, target in ARCH.items():
                directory = evidence / target / source["commit"]
                expected = approval["builds"][arch]
                require(sha_file(directory / "manifest.json") == expected["manifest_sha256"],
                        f"{arch}: approved build manifest digest mismatch")
                build.verify_target(directory, target, source)
                safe_build_zip(directory / CLIENT)
                path = stage / package_name(arch)
                copy_bundle(directory / CLIENT, path, expected["client_sha256"], public, icon)
                records[arch] = {"file": path.name, "sha256": sha_file(path), "bytes": path.stat().st_size}
            raw = manifest_bytes(approval, records)
            (stage / MANIFEST).write_bytes(raw)
            # An already completed concurrent attempt can only be reused if the
            # selected approval and every byte of both artifacts match.
            try:
                stage.rename(destination)
            except OSError:
                if not destination.is_dir() or read_small(destination / MANIFEST) != raw:
                    raise
                check_artifacts(destination, approval, records)
    finally:
        build.ROOT = original_root


def check_artifacts(directory, approval, records):
    public = profile(approval)
    for arch, record in records.items():
        path = directory / record["file"]
        with open_regular(path) as package:
            require(os.fstat(package.fileno()).st_size == record["bytes"] and sha_stream(package) == record["sha256"],
                    f"{arch}: delivery package digest mismatch")
            package.seek(0)
            check_package_contents(package, approval["builds"][arch]["client_sha256"], public, approval["brand"]["icon_sha256"])


def openssl_signature(openssl, operation, raw, key, signature=None):
    args = [openssl, "pkeyutl", operation, "-rawin", "-inkey", str(key)]
    with tempfile.TemporaryDirectory(prefix="deskrealm-signing-") as tmp:
        payload = Path(tmp) / "statement"
        payload.write_bytes(SIGNING_PREFIX + raw)
        args += ["-in", str(payload)]
        if signature is not None:
            sig = Path(tmp) / "signature"
            sig.write_bytes(signature)
            args += ["-pubin", "-sigfile", str(sig)]
        run = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
    require(run.returncode == 0, "isolated signer unavailable or development signature invalid")
    return run.stdout


def sign(approval, manifest, key, destination, openssl):
    raw = read_small(manifest)
    check_manifest(raw, approval)
    signature = openssl_signature(openssl, "-sign", raw, key)
    require(len(signature) == 64, "expected a raw Ed25519 signature")
    with tempfile.TemporaryDirectory(prefix=".deskrealm-signature-", dir=destination.parent) as tmp:
        output = Path(tmp) / SIGNATURE
        output.write_bytes(signature)
        try:
            os.link(output, destination)
        except FileExistsError:
            require(read_small(destination, 64) == signature, "signature destination already contains a different signature")


def verify(approval, directory, signature_path, key, openssl):
    raw = read_small(directory / MANIFEST)
    records = check_manifest(raw, approval)
    signature = read_small(signature_path, 64)
    require(len(signature) == 64, "expected a raw Ed25519 signature")
    with tempfile.TemporaryDirectory(prefix="deskrealm-public-key-") as tmp:
        pinned_key = Path(tmp) / "public.pem"
        pinned_key.write_bytes(read_small(key))
        public = subprocess.run([openssl, "pkey", "-pubin", "-in", str(pinned_key), "-outform", "DER"],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
        require(public.returncode == 0 and len(public.stdout) == 44
                and public.stdout[:12] == bytes.fromhex("302a300506032b6570032100"),
                "verification key must be Ed25519")
        openssl_signature(openssl, "-verify", raw, pinned_key, signature)
    check_artifacts(directory, approval, records)
    print(f"DeskRealm {approval['version']}: x64 and ARM64 development packages verified")
    return raw, records


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--openssl", default="openssl", help="OpenSSL 3 or later")
    modes = parser.add_subparsers(dest="mode", required=True)
    prepare_cmd = modes.add_parser("prepare")
    for field in ("approval", "evidence", "icon", "destination"):
        prepare_cmd.add_argument(field, type=Path)
    prepare_cmd.add_argument("--source-tree", type=Path, default=build.ROOT)
    sign_cmd = modes.add_parser("sign")
    for field in ("approval", "manifest", "private_key", "destination"):
        sign_cmd.add_argument(field, type=Path)
    verify_cmd = modes.add_parser("verify")
    for field in ("approval", "directory", "signature", "public_key"):
        verify_cmd.add_argument(field, type=Path)
    args = parser.parse_args()
    try:
        approval = approved(args.approval)
        if args.mode == "prepare":
            prepare(approval, args.evidence.resolve(), args.icon, args.destination.absolute(), args.source_tree.resolve())
        elif args.mode == "sign":
            sign(approval, args.manifest, args.private_key, args.destination, args.openssl)
        else:
            verify(approval, args.directory, args.signature, args.public_key, args.openssl)
    except (OSError, ValueError, TypeError, KeyError, binascii.Error, BadZipFile, subprocess.CalledProcessError) as error:
        print(f"DeskRealm Windows package: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
