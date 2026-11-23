"""Replay the native patches and check both generated signaling schemas."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
CLIENT_IMAGE = "docker.io/library/rust@sha256:87f3b2f93b82995443a1a558c234212dafe79cfdc3af956539610560369ddcd0"
SERVER_IMAGE = "docker.io/library/rust@sha256:ade39a710ef85f3506cffcfec3e8a24ba090badef1dff2fb798f71d878833fea"


def protobuf(schema, flag, kind, payload):
    return subprocess.run(
        ["protoc", f"--proto_path={schema.parent}", f"--{flag}=hbb.{kind}", schema.name],
        input=payload, stdout=subprocess.PIPE, check=True,
    ).stdout


def text_bytes(raw):
    return '"' + "".join(f"\\{b:03o}" for b in raw) + '"'


def verify_schemas(client, server):
    client_proto = client / "libs/hbb_common/protos/rendezvous.proto"
    server_proto = server / "libs/hbb_common/protos/rendezvous.proto"
    vectors = json.loads((ROOT / "managed/tests/vectors.json").read_text())
    grant = next(item for item in vectors["vectors"] if item["name"] == "center-session-grant")
    frame = text_bytes(bytes.fromhex(grant["frame_hex"]))
    signature = text_bytes(bytes.fromhex(grant["signature_hex"]))
    connection = text_bytes(bytes(range(16)))
    for operation in (
        'direct_request { id: "10700002" }', 'relay_request { id: "10700002" }',
        'transport_challenge: "challenge"', 'transport_proof { public_key: "key" signature: "proof" }',
        'relay_join: 1', 'relay_ready: true',
    ):
        request = f"""deskrealm_signal {{
            route {{ grant_frame: {frame} grant_signature: {signature} connection_uuid: {connection} target_rustdesk_id: "10700002" }}
            {operation}
        }}""".encode()
        left = protobuf(client_proto, "encode", "RendezvousMessage", request)
        right = protobuf(server_proto, "encode", "RendezvousMessage", request)
        if left != right or protobuf(client_proto, "decode", "RendezvousMessage", right) != protobuf(
            server_proto, "decode", "RendezvousMessage", left
        ):
            raise RuntimeError("client and server rendezvous messages disagree")
    peer_proto = client / "libs/base/protos/message.proto"
    admission = b"deskrealm_handshake { admission { allowed_mask: 1 lease_sequence: 1 lease_until_ms: 123 } }"
    encoded = protobuf(peer_proto, "encode", "Message", admission)
    if encoded != bytes.fromhex("c202082a0608011001187b"):
        # The fixture pins the oneof tag and the nested admission field numbers.
        raise RuntimeError(f"unexpected managed peer admission bytes: {encoded.hex()}")
    print("Native signaling schemas agree on direct and relay; peer admission field numbers verified.", flush=True)


def in_source(repo, cache, image, command, required=None):
    cache.mkdir(exist_ok=True)
    run = subprocess.run(
        ["docker", "run", "--rm", "--volume", f"{repo}:/source", "--volume", f"{cache}:/cargo-cache",
         "--env", "CARGO_HOME=/cargo-cache", "--env", "CARGO_NET_GIT_FETCH_WITH_CLI=true",
         "--workdir", "/source", image, *command],
        text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
    )
    if run.returncode or (required and required not in run.stdout):
        print(run.stdout[-12000:], flush=True)
        raise RuntimeError(f"{repo.name}: locked native check did not pass")
    print(run.stdout[-1800:], flush=True)


def cargo(repo, cache, image, *args, required=None):
    in_source(repo, cache, image, ["cargo", *args], required=required)


def run_fixture(server, client, admission):
    binary = server / "target/debug"
    args = ["docker", "run", "--rm", "--network", "none",
            "--volume", f"{ROOT / 'managed/tests'}:/fixture:ro",
            "--volume", f"{binary}:/server:ro"]
    if admission:
        candidates = sorted(path for path in (client / "target/debug/deps").glob("deskrealm_interop-*")
                            if path.is_file() and os.access(path, os.X_OK))
        if len(candidates) != 1:
            raise RuntimeError("expected exactly one native interoperability test executable")
        args += ["--volume", f"{candidates[0]}:/endpoint:ro", SERVER_IMAGE,
                 "python3", "/fixture/native_endpoint_fixture.py", "/server", "/endpoint"]
    else:
        args += [SERVER_IMAGE, "python3", "/fixture/native_coord_fixture.py", "/server"]
    subprocess.run(args, check=True)


def desktop_image(scratch):
    iid = scratch / "desktop-image"
    dockerfile = ROOT / "managed/tests/desktop-linux.Dockerfile"
    context = scratch / "desktop-context"
    context.mkdir()
    subprocess.run(
        ["docker", "build", "--quiet", "--iidfile", iid, "--file", dockerfile, context],
        check=True,
    )
    return iid.read_text().strip()


DESKTOP_FEATURES = "deskrealm-managed,linux-pkg-config,use_dasp"


def check_desktop(client, cache, scratch):
    cargo(client, cache, desktop_image(scratch), "check", "--locked", "--lib",
          "--no-default-features", "--features", DESKTOP_FEATURES)


def check_view(client, cache, scratch):
    in_source(
        client, cache, desktop_image(scratch),
        ["env", "RUSTDESK_FORCED_DISPLAY_SERVER=x11", "XDG_SESSION_TYPE=x11", "sh", "-ec",
         'xvfb-run -a -s "-screen 0 640x480x24" "$@"\ntrue', "view",
         "cargo", "test", "--locked", "--lib",
         "--no-default-features", "--features", DESKTOP_FEATURES, "managed_view_",
         "--", "--include-ignored", "--nocapture"],
        required="test result: ok. 3 passed",
    )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument("--coordination", action="store_true", help="build and run the managed server pair")
    modes.add_argument("--admission", action="store_true", help="run managed endpoint admission on both paths")
    modes.add_argument("--desktop", action="store_true", help="check desktop routing and run native interoperability")
    modes.add_argument("--view", action="store_true", help="run the managed capture and message tests on X11")
    arguments = parser.parse_args()
    # Use a Docker-mountable path on macOS, even when TMPDIR is a per-user path.
    directory = os.environ.get("DESKREALM_NATIVE_TMPDIR", "/tmp")
    with tempfile.TemporaryDirectory(prefix="deskrealm-native-wire-", dir=Path(directory).resolve()) as tmp:
        scratch = Path(tmp)
        source = scratch / "source"
        subprocess.run([ROOT / "scripts/deskrealm-managed-source", "prepare", source, "--apply-patches"], check=True)
        canonical = (ROOT / "managed/native/deskrealm_protocol.rs").read_bytes()
        for target in ("client", "server"):
            actual = source / target / "libs/hbb_common/src/deskrealm_protocol.rs"
            if actual.read_bytes() != canonical:
                raise RuntimeError(f"{target}: managed protocol differs from the reviewed source")
        verify_schemas(source / "client", source / "server")
        if arguments.view:
            check_view(source / "client", scratch / "client-cache", scratch)
            return
        if arguments.coordination or arguments.admission or arguments.desktop:
            admission = arguments.admission or arguments.desktop
            if admission:
                cargo(source / "client", scratch / "client-cache", CLIENT_IMAGE, "test", "--locked",
                      "-p", "base", "--test", "deskrealm_interop", "--no-run")
                cargo(source / "client", scratch / "client-cache", CLIENT_IMAGE, "test", "--locked",
                      "-p", "base", "--lib", "deskrealm_endpoint::lease::tests::managed_lease_",
                      required="test result: ok. 4 passed")
                cargo(source / "client", scratch / "client-cache", CLIENT_IMAGE, "test", "--locked",
                      "-p", "base", "--lib", "resume_requires_revalidation_before_any_access",
                      required="resume_requires_revalidation_before_any_access ... ok")
            cargo(source / "server", scratch / "server-cache", SERVER_IMAGE, "build", "--locked",
                  "--bin", "hbbs", "--bin", "hbbr")
            run_fixture(source / "server", source / "client", admission)
            if arguments.desktop:
                check_desktop(source / "client", scratch / "client-cache", scratch)
            return
        cargo(source / "client", scratch / "client-cache", CLIENT_IMAGE, "test", "--locked", "-p", "hbb_common",
              "--lib", "managed_channel_binding_direct_and_forwarded", required="managed_channel_binding_direct_and_forwarded ... ok")
        cargo(source / "client", scratch / "client-cache", CLIENT_IMAGE, "check", "--locked", "-p", "base", "--lib")
        cargo(source / "server", scratch / "server-cache", SERVER_IMAGE, "check", "--locked", "--bins")


if __name__ == "__main__":
    main()
