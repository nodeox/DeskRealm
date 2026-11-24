"""Check HTTPS enrollment and independent probes with two actual Linux hbbr processes."""

import argparse
import os
from pathlib import Path
import subprocess
import tempfile
import uuid

from native_wire import CLIENT_IMAGE, ROOT, SERVER_IMAGE, cargo, interoperability_binary, target_directory
from native_coord_fixture import tls_context


RUNNER = """#!/usr/bin/env bash
set -euo pipefail
port="$2"
exec docker run --rm --name "$DESKREALM_TEST_RELAY_LABEL-$port" \\
  --label "deskrealm.relay-test=$DESKREALM_TEST_RELAY_LABEL" \\
  --publish "127.0.0.1:$port:$port" --user "$(id -u):$(id -g)" \\
  --mount "type=bind,src=$DESKREALM_TEST_NATIVE_HBBR,dst=/hbbr,readonly" \\
  --mount "type=bind,src=$(dirname "$DESKREALM_RELAY_STATE"),dst=/relay-state,readonly" \\
  --env "DESKREALM_RELAY_STATE=/relay-state/$(basename "$DESKREALM_RELAY_STATE")" \\
  --env DESKREALM_REGIONAL_CENTER_PROBE_KEY --entrypoint /hbbr \\
  "$DESKREALM_TEST_RELAY_IMAGE" "$@"
"""


CLIENT_RUNNER = """#!/usr/bin/env bash
set -euo pipefail
exec docker run --rm --network host \\
  --label "deskrealm.relay-test=$DESKREALM_TEST_RELAY_LABEL" \\
  --user "$(id -u):$(id -g)" \\
  --mount "type=bind,src=$DESKREALM_TEST_RUST_RELAY_CLIENT,dst=/fixture/endpoint,readonly" \\
  --mount "type=bind,src=$1,dst=/fixture/input.json,readonly" \\
  --mount "type=bind,src=$2,dst=/fixture/ca.pem,readonly" \\
  --env DESKREALM_RELAY_PROBE_FIXTURE=/fixture/input.json --env DESKREALM_RELAY_PROBE_CA=/fixture/ca.pem \\
  --entrypoint /fixture/endpoint "$DESKREALM_TEST_RUST_RELAY_IMAGE" \\
  --exact managed_native_endpoints_report_both_regional_relays
"""


def verify(binary, scratch, endpoint=None):
    cli = scratch / "deskrealm-relay"
    subprocess.run(["go", "build", "-o", cli, "./cmd/deskrealm-relay"], cwd=ROOT, check=True)
    runner = scratch / "run-hbbr"
    runner.write_text(RUNNER)
    runner.chmod(0o700)
    label = "deskrealm-relay-" + uuid.uuid4().hex[:12]
    env = {
        **os.environ,
        "GOFLAGS": os.environ.get("GOFLAGS", "") + " -race",
        "DESKREALM_RELAY_TEST_BINARY": str(cli),
        "DESKREALM_TEST_HBBR_RUNNER": str(runner),
        "DESKREALM_TEST_NATIVE_HBBR": str(binary),
        "DESKREALM_TEST_RELAY_IMAGE": SERVER_IMAGE,
        "DESKREALM_TEST_RELAY_LABEL": label,
    }
    if endpoint is not None:
        tls_dir = scratch / "tls"
        tls_dir.mkdir(mode=0o700)
        tls_context(tls_dir)
        client_runner = scratch / "run-native-relay-probes"
        client_runner.write_text(CLIENT_RUNNER)
        client_runner.chmod(0o700)
        env.update({
            "DESKREALM_TEST_RUST_RELAY_RUNNER": str(client_runner),
            "DESKREALM_TEST_RUST_RELAY_CLIENT": str(endpoint),
            "DESKREALM_TEST_RUST_RELAY_IMAGE": CLIENT_IMAGE,
            "DESKREALM_TEST_RELAY_TLS_DIR": str(tls_dir),
        })
    try:
        subprocess.run(["bash", "scripts/deskrealm-m1-test", "relay-enrollment"], cwd=ROOT, env=env, check=True)
    finally:
        containers = subprocess.check_output(
            ["docker", "ps", "-aq", "--filter", f"label=deskrealm.relay-test={label}"], text=True
        ).split()
        if containers:
            subprocess.run(["docker", "rm", "--force", *containers], check=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--client-probes", action="store_true")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="deskrealm-relay-enrollment-") as directory:
        scratch = Path(directory)
        source = scratch / "source"
        subprocess.run([ROOT / "scripts/deskrealm-managed-source", "prepare", source, "--apply-patches"], check=True)
        server = source / "server"
        cargo(server, scratch / "cargo", SERVER_IMAGE, "build", "--locked", "--bin", "hbbr")
        endpoint = interoperability_binary(source / "client", scratch / "cargo-client", "deskrealm_relay_probes") if args.client_probes else None
        verify(target_directory(server) / "debug/hbbr", scratch, endpoint)


if __name__ == "__main__":
    main()
