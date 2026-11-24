"""Check HTTPS enrollment and independent probes with two actual Linux hbbr processes."""

import os
from pathlib import Path
import subprocess
import tempfile
import uuid

from native_wire import ROOT, SERVER_IMAGE, cargo, target_directory


RUNNER = """#!/usr/bin/env bash
set -euo pipefail
port="$2"
exec docker run --rm --name "$DESKREALM_TEST_RELAY_LABEL-$port" \\
  --label "deskrealm.relay-test=$DESKREALM_TEST_RELAY_LABEL" \\
  --publish "127.0.0.1:$port:$port" --user "$(id -u):$(id -g)" \\
  --mount "type=bind,src=$DESKREALM_TEST_NATIVE_HBBR,dst=/hbbr,readonly" \\
  --mount "type=bind,src=$DESKREALM_RELAY_STATE,dst=/relay-state,readonly" \\
  --env DESKREALM_RELAY_STATE=/relay-state --entrypoint /hbbr \\
  "$DESKREALM_TEST_RELAY_IMAGE" "$@"
"""


def verify(binary, scratch):
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
    try:
        subprocess.run(["bash", "scripts/deskrealm-m1-test", "relay-enrollment"], cwd=ROOT, env=env, check=True)
    finally:
        containers = subprocess.check_output(
            ["docker", "ps", "-aq", "--filter", f"label=deskrealm.relay-test={label}"], text=True
        ).split()
        if containers:
            subprocess.run(["docker", "rm", "--force", *containers], check=True)


def main():
    with tempfile.TemporaryDirectory(prefix="deskrealm-relay-enrollment-") as directory:
        scratch = Path(directory)
        source = scratch / "source"
        subprocess.run([ROOT / "scripts/deskrealm-managed-source", "prepare", source, "--apply-patches"], check=True)
        server = source / "server"
        cargo(server, scratch / "cargo", SERVER_IMAGE, "build", "--locked", "--bin", "hbbr")
        verify(target_directory(server) / "debug/hbbr", scratch)


if __name__ == "__main__":
    main()
