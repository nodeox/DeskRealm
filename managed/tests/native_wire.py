"""Replay the native patches and check both generated signaling schemas."""

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
    for operation in ('direct_request { id: "10700002" }', 'relay_request { id: "10700002" }'):
        request = f"""deskrealm_signal {{
            route {{ grant_frame: {frame} grant_signature: {signature} connection_uuid: {connection} }}
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


def cargo(repo, cache, image, *args, required=None):
    cache.mkdir(exist_ok=True)
    run = subprocess.run(
        ["docker", "run", "--rm", "--volume", f"{repo}:/source", "--volume", f"{cache}:/cargo-cache",
         "--env", "CARGO_HOME=/cargo-cache", "--env", "CARGO_NET_GIT_FETCH_WITH_CLI=true",
         "--workdir", "/source", image, "cargo", *args],
        text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
    )
    if run.returncode or (required and required not in run.stdout):
        print(run.stdout[-12000:], flush=True)
        raise RuntimeError(f"{repo.name}: locked Cargo command did not pass")
    print(run.stdout[-1800:], flush=True)


def main():
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
        cargo(source / "client", scratch / "client-cache", CLIENT_IMAGE, "test", "--locked", "-p", "hbb_common",
              "--lib", "managed_channel_binding_direct_and_forwarded", required="managed_channel_binding_direct_and_forwarded ... ok")
        cargo(source / "client", scratch / "client-cache", CLIENT_IMAGE, "check", "--locked", "-p", "base", "--lib")
        cargo(source / "server", scratch / "server-cache", SERVER_IMAGE, "check", "--locked", "--bins")


if __name__ == "__main__":
    main()
