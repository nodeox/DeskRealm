"""Replay the native patches and check both generated signaling schemas."""

import argparse
import json
import os
from pathlib import Path
import re
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
    clipboard = b"deskrealm_clipboard { epoch: 3 state: true }"
    if protobuf(peer_proto, "encode", "Message", clipboard) != bytes.fromhex("ca020408031001"):
        raise RuntimeError("managed clipboard state field numbers changed")
    file_cancel = b"deskrealm_file { id: 7 cancel: true }"
    if protobuf(peer_proto, "encode", "Message", file_cancel) != bytes.fromhex("d2020408074801"):
        raise RuntimeError("managed file cancel field numbers changed")
    print("Native signaling schemas agree on direct and relay; managed peer fields verified.", flush=True)


def target_directory(repo):
    root = os.environ.get("DESKREALM_NATIVE_CACHE_ROOT")
    if root:
        return (Path(root).resolve() / f"{repo.name}-target").resolve()
    return repo / "target"


def in_source(repo, cache, image, command, required=None):
    root = os.environ.get("DESKREALM_NATIVE_CACHE_ROOT")
    offline = os.environ.get("DESKREALM_NATIVE_OFFLINE") == "1"
    if offline and not root:
        raise RuntimeError("offline native tests require DESKREALM_NATIVE_CACHE_ROOT")
    args = ["docker", "run", "--rm", "--volume", f"{repo}:/source"]
    if root:
        cache = (Path(root).resolve() / f"{repo.name}-cache").resolve()
        target = target_directory(repo)
        target.mkdir(parents=True, exist_ok=True)
        args += ["--volume", f"{target}:/source/target"]
    if offline:
        args += ["--network", "none", "--env", "CARGO_NET_OFFLINE=true"]
    cache.mkdir(parents=True, exist_ok=True)
    run = subprocess.run(
        [*args, "--volume", f"{cache}:/cargo-cache",
         "--env", "CARGO_HOME=/cargo-cache", "--env", "CARGO_NET_GIT_FETCH_WITH_CLI=true",
         "--workdir", "/source", image, *command],
        text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
    )
    if run.returncode or (required and required not in run.stdout):
        print(run.stdout[-12000:], flush=True)
        raise RuntimeError(f"{repo.name}: locked native check did not pass")
    print(run.stdout[-1800:], flush=True)
    return run.stdout


def cargo(repo, cache, image, *args, required=None):
    return in_source(repo, cache, image, ["cargo", *args], required=required)


def interoperability_binary(client, cache, test_name="deskrealm_interop"):
    output = cargo(client, cache, CLIENT_IMAGE, "test", "--locked", "-p", "base", "--test",
                   test_name, "--no-run", "--message-format=json")
    executables = []
    for line in output.splitlines():
        if not line.startswith("{"):
            continue
        record = json.loads(line)
        if (record.get("reason") == "compiler-artifact"
                and record.get("target", {}).get("name") == test_name
                and record.get("executable")):
            executable = Path(record["executable"]).relative_to("/source/target")
            executables.append(target_directory(client) / executable)
    if len(executables) != 1 or not executables[0].is_file():
        raise RuntimeError("expected exactly one built interoperability test executable")
    return executables[0]


def run_fixture(server, client, admission, executable=None):
    binary = target_directory(server) / "debug"
    args = ["docker", "run", "--rm", "--network", "none",
            "--volume", f"{ROOT / 'managed/tests'}:/fixture:ro",
            "--volume", f"{binary}:/server:ro"]
    if admission:
        if executable is None:
            candidates = sorted(path for path in (target_directory(client) / "debug/deps").glob("deskrealm_interop-*")
                                if path.is_file() and os.access(path, os.X_OK))
            if len(candidates) != 1:
                raise RuntimeError("expected exactly one native interoperability test executable")
            executable = candidates[0]
        args += ["--volume", f"{executable}:/endpoint:ro", SERVER_IMAGE,
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
X11_REQUIRED_TESTS = {
    "managed_view_does_not_dispatch_legacy_or_other_capabilities",
    "managed_view_dispatches_only_the_authorized_message_scope",
    "managed_view_captures_and_releases_monitor",
    "managed_input_discards_stale_queued_events_and_releases_held_buttons",
    "managed_input_x11_injects_and_cleans_up_after_permission_loss",
    "managed_audio_requires_independent_permission_and_valid_format",
    "managed_audio_keeps_format_with_each_bounded_packet_and_drops_stale_audio",
    "managed_clipboard_validates_text_and_discards_stale_epochs",
    "managed_clipboard_x11_applies_text_only_under_current_permission",
    "managed_file_paths_and_publication_fail_closed",
    "managed_file_driver_moves_bytes_and_audits_cancel_and_failure",
    "managed_local_consent_preserves_only_selected_actions",
    "managed_local_consent_cancels_partial_approval",
    "managed_local_consent_rejects_invalid_permission_masks",
}


def check_desktop(client, cache, scratch):
    cargo(client, cache, desktop_image(scratch), "check", "--locked", "--lib",
          "--no-default-features", "--features", DESKTOP_FEATURES)


def check_x11(client, cache, scratch, selector, expected, required_tests=()):
    image = desktop_image(scratch)
    print(f"Linux desktop test image: {image}", flush=True)
    result = in_source(
        client, cache, image,
        ["env", "RUSTDESK_FORCED_DISPLAY_SERVER=x11", "XDG_SESSION_TYPE=x11", "sh", "-ec",
         'xvfb-run -a -s "-screen 0 640x480x24 -noreset" "$@"\ntrue', "desktop",
         "cargo", "test", "--locked", "--lib",
         "--no-default-features", "--features", DESKTOP_FEATURES, selector,
         "--", "--include-ignored", "--nocapture"],
        required=f"test result: ok. {expected} passed; 0 failed; 0 ignored",
    )
    passed = set(re.findall(r"^test (?:[^\s]+::)?([^:\s]+) \.\.\. ok$", result, re.MULTILINE))
    missing = set(required_tests) - passed
    if missing:
        raise RuntimeError(f"missing required desktop regressions: {', '.join(sorted(missing))}")
    if required_tests:
        print(f"Required Linux desktop regressions passed: {', '.join(sorted(passed))}", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument("--coordination", action="store_true", help="build and run the managed server pair")
    modes.add_argument("--admission", action="store_true", help="run managed endpoint admission on both paths")
    modes.add_argument("--desktop", action="store_true", help="check desktop routing and run native interoperability")
    modes.add_argument("--view", action="store_true", help="run the managed capture and message tests on X11")
    modes.add_argument("--input", action="store_true", help="run the managed input dispatch and cleanup tests on X11")
    modes.add_argument("--audio", action="store_true", help="run managed audio permission and Opus queue tests")
    modes.add_argument("--clipboard", action="store_true", help="run managed text clipboard tests on X11")
    modes.add_argument("--files", action="store_true", help="run managed file and cleanup tests")
    modes.add_argument("--endpoint", action="store_true", help="run admission, leases and all managed desktop tests")
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
        if arguments.audio:
            cargo(source / "client", scratch / "client-cache", desktop_image(scratch),
                  "test", "--locked", "--lib", "--no-default-features", "--features",
                  DESKTOP_FEATURES, "managed_audio_", required="test result: ok. 2 passed")
            return
        if arguments.files:
            cargo(source / "client", scratch / "client-cache", desktop_image(scratch),
                  "test", "--locked", "--lib", "--no-default-features", "--features",
                  DESKTOP_FEATURES, "managed_file_", required="test result: ok. 2 passed")
            return
        if arguments.clipboard:
            check_x11(source / "client", scratch / "client-cache", scratch, "managed_clipboard_", 2)
            return
        if arguments.view or arguments.input:
            selector, expected = ("managed_input_", 2) if arguments.input else ("managed_view_", 3)
            check_x11(source / "client", scratch / "client-cache", scratch, selector, expected)
            return
        if arguments.coordination or arguments.admission or arguments.desktop or arguments.endpoint:
            admission = arguments.admission or arguments.desktop or arguments.endpoint
            executable = None
            if admission:
                executable = interoperability_binary(source / "client", scratch / "client-cache")
                cargo(source / "client", scratch / "client-cache", CLIENT_IMAGE, "test", "--locked",
                      "-p", "base", "--lib", "deskrealm_endpoint::lease::tests::managed_lease_",
                      required="test result: ok. 4 passed; 0 failed; 0 ignored")
                cargo(source / "client", scratch / "client-cache", CLIENT_IMAGE, "test", "--locked",
                      "-p", "base", "--lib", "resume_requires_revalidation_before_any_access",
                      required="resume_requires_revalidation_before_any_access ... ok")
            cargo(source / "server", scratch / "server-cache", SERVER_IMAGE, "build", "--locked",
                  "--bin", "hbbs", "--bin", "hbbr")
            run_fixture(source / "server", source / "client", admission, executable)
            if arguments.endpoint:
                check_x11(source / "client", scratch / "client-cache", scratch, "managed_",
                          len(X11_REQUIRED_TESTS), X11_REQUIRED_TESTS)
            if arguments.desktop:
                check_desktop(source / "client", scratch / "client-cache", scratch)
            return
        cargo(source / "client", scratch / "client-cache", CLIENT_IMAGE, "test", "--locked", "-p", "hbb_common",
              "--lib", "managed_channel_binding_direct_and_forwarded", required="managed_channel_binding_direct_and_forwarded ... ok")
        cargo(source / "client", scratch / "client-cache", CLIENT_IMAGE, "check", "--locked", "-p", "base", "--lib")
        cargo(source / "server", scratch / "server-cache", SERVER_IMAGE, "check", "--locked", "--bins")


if __name__ == "__main__":
    main()
