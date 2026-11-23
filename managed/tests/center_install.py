"""Build and exercise the center installation in a disposable Docker project."""

import base64
from contextlib import ExitStack
import hashlib
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import tempfile
import time

from native_coord_fixture import tls_context
from native_wire import ROOT, SERVER_IMAGE, cargo, target_directory


REGISTRY = "docker.io/library/registry@sha256:82f6ff4a7f982960c8aa51617fea3aabc485df6e55cce80d22deb1de36f1bc3c"
POSTGRES = "docker.io/library/postgres:18.7-bookworm@sha256:194e55feb2597b1465dbee81cc9fd8d204566cd2eb42c8d22fd6c931a38781b1"
HOST = "center.deskrealm.test"
LOOPBACK = "127.0.0.1"


def run(*args, **kwargs):
    return subprocess.run(args, cwd=ROOT, check=True, **kwargs)


def output(*args):
    return run(*args, text=True, stdout=subprocess.PIPE).stdout.strip()


def cleanup(*args):
    subprocess.run(args, cwd=ROOT, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def available_ports():
    with ExitStack() as stack:
        ports = []
        for _ in range(3):
            tcp = stack.enter_context(socket.socket())
            tcp.bind((LOOPBACK, 0))
            ports.append(tcp.getsockname()[1])
        udp = stack.enter_context(socket.socket(type=socket.SOCK_DGRAM))
        udp.bind((LOOPBACK, ports[-1]))
        return ports


def publish_images(scratch, resources):
    architecture = output("docker", "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
    if architecture not in {"linux/arm64", "linux/amd64"}:
        raise RuntimeError(f"unsupported development engine: {architecture}")
    context = scratch / "images"
    context.mkdir()
    run("go", "build", "-trimpath", "-o", context / "deskrealm", "./cmd/deskrealm",
        env={**os.environ, "GOOS": "linux", "GOARCH": architecture.split("/")[1], "CGO_ENABLED": "0"})
    run("npm", "--prefix", "web", "run", "build", "--", "--emptyOutDir", "--outDir", context / "web")
    source = scratch / "source"
    run(ROOT / "scripts/deskrealm-managed-source", "prepare", source, "--apply-patches")
    server = source / "server"
    cargo(server, scratch / "cargo", SERVER_IMAGE, "build", "--locked", "--bin", "hbbs")
    run("docker", "run", "--rm", "--network", "none",
        "--mount", f"type=bind,src={target_directory(server) / 'debug' / 'hbbs'},dst=/hbbs,readonly",
        "--mount", f"type=bind,src={context},dst=/output",
        SERVER_IMAGE, "strip", "--strip-debug", "-o", "/output/hbbs", "/hbbs")

    registry = output("docker", "run", "--detach", "--volume", "/var/lib/registry",
                      "--publish", f"{LOOPBACK}::5000", REGISTRY)
    resources.callback(cleanup, "docker", "rm", "--force", "--volumes", registry)
    address = output("docker", "port", registry, "5000/tcp")
    references = {"postgresql": POSTGRES}
    for name in ("center", "rendezvous"):
        repository = f"{address}/deskrealm-{name}"
        tag = f"{repository}:dev"
        resources.callback(cleanup, "docker", "image", "rm", tag)
        run("docker", "build", "--quiet", "--platform", architecture,
            "--target", name, "--tag", tag,
            "--file", ROOT / "internal/m3/install/runtime.Dockerfile", context)
        push = output("docker", "push", tag)
        digest = re.search(r"^dev: digest: (sha256:[a-f0-9]{64}) size:", push, re.MULTILINE)
        if not digest:
            raise RuntimeError(f"local registry did not report a digest for {name}")
        references[name] = f"{repository}@{digest[1]}"
        resources.callback(cleanup, "docker", "image", "rm", references[name])
        print(f"Built {name} for {architecture} at {digest[1]}", flush=True)
    return references


def https_status(origin, port, ca):
    response = subprocess.run(
        ["curl", "--noproxy", "*", "--silent", "--show-error", "--fail", "--max-time", "2",
         "--resolve", f"{HOST}:{port}:{LOOPBACK}", "--cacert", ca, f"{origin}/v1/bootstrap/status"],
        text=True, capture_output=True,
    )
    if response.returncode:
        raise RuntimeError(response.stderr.strip())
    initialized = json.loads(response.stdout).get("initialized")
    if not isinstance(initialized, bool):
        raise RuntimeError("unexpected center bootstrap status")
    return initialized


def wait_for_center(origin, port, ca, initialized):
    deadline = time.monotonic() + 35
    last_error = "deadline elapsed"
    while time.monotonic() < deadline:
        try:
            state = https_status(origin, port, ca)
        except (RuntimeError, ValueError) as error:
            last_error = str(error)
            time.sleep(0.3)
            continue
        if state != initialized:
            raise RuntimeError("center initialized state did not match expected state")
        return
    raise RuntimeError(f"HTTPS readiness failed: {last_error}")


def browser_bootstrap(scratch, origin, certificate, ticket):
    key = run("openssl", "x509", "-in", certificate, "-pubkey", "-noout", stdout=subprocess.PIPE)
    der = run("openssl", "pkey", "-pubin", "-outform", "DER", input=key.stdout, stdout=subprocess.PIPE)
    spki = base64.b64encode(hashlib.sha256(der.stdout).digest()).decode()
    report = scratch / "chromium.json"
    run("npm", "--prefix", "web", "run", "test:install", env={
        **os.environ,
        "DESKREALM_BROWSER_BASE": origin,
        "DESKREALM_BROWSER_TICKET": ticket,
        "DESKREALM_BROWSER_TLS_SPKI": spki,
        "DESKREALM_BROWSER_LOCAL_HOST": HOST,
        "DESKREALM_BROWSER_OUTPUT": str(scratch / "playwright"),
        "DESKREALM_BROWSER_REPORT": str(report),
    })
    suites = json.loads(report.read_text())["suites"]
    specs = [spec for suite in suites for spec in suite.get("specs", [])]
    if len(specs) != 1 or specs[0]["title"] != "HTTPS 中心首次管理员初始化和登录":
        raise RuntimeError("required Chromium bootstrap regression is missing")
    tests = specs[0]["tests"]
    if not tests or any(t["expectedStatus"] != "passed" or not t["results"] or
                        any(r["status"] != "passed" for r in t["results"]) for t in tests):
        raise RuntimeError("required Chromium bootstrap regression did not pass")


def exercise(scratch, images, resources):
    api_port, check_port, route_port = available_ports()
    origin = f"https://{HOST}:{api_port}"
    ca, _ = tls_context(scratch, f"DNS:{HOST}")
    certificate, private_key = scratch / "server.pem", scratch / "server.key"
    installation = scratch / "deployment"
    config = scratch / "center.json"
    config.write_text(json.dumps({
        "version": 1, "directory": str(installation), "public_origin": origin,
        "id_server": f"{LOOPBACK}:{route_port}",
        "https": {"mode": "provided", "certificate": str(certificate),
                  "private_key": str(private_key), "ca_certificate": str(ca)},
        "images": images,
        "ports": {"rendezvous": route_port, "rendezvous_check": check_port, "bind_host": LOOPBACK},
    }))
    installer = scratch / "deskrealm-install"
    run("go", "build", "-o", installer, "./cmd/deskrealm-install")
    flags = ("--config", config)
    run(installer, "preflight", *flags)
    prepared = json.loads(output(installer, "prepare", *flags))
    compose = ("docker", "compose", "--env-file", "/dev/null", "-f", prepared["compose_file"])
    resources.callback(cleanup, *compose, "down", "--volumes", "--remove-orphans")
    if json.loads(output(installer, "status", *flags))["phase"] != "prepared":
        raise RuntimeError("status did not identify a prepared deployment")
    installed = json.loads(output(installer, "install", *flags))
    if installed["phase"] != "ready" or installed["initialized"] is not False:
        raise RuntimeError("installation did not reach ready before issuing a ticket")
    if any(installed.get(key) != value for key, value in prepared.items()):
        raise RuntimeError("starting services changed the persisted deployment identity")
    wait_for_center(origin, api_port, ca, initialized=False)
    run(*compose, "exec", "-T", "rendezvous", "getent", "ahostsv4", HOST, stdout=subprocess.DEVNULL)
    print("Compose migration, CA-verified HTTPS and native hbbs readiness passed.", flush=True)

    ticket = output(installer, "bootstrap-ticket", *flags)
    if not re.fullmatch(re.escape(origin) + r"/initialize#ticket=[A-Za-z0-9_-]{43}", ticket):
        raise RuntimeError("host-side command did not produce a valid initialization link")
    browser_bootstrap(scratch, origin, certificate, ticket)
    wait_for_center(origin, api_port, ca, initialized=True)
    print("Chromium first-administrator initialization and login passed.", flush=True)

    container_ids = output(*compose, "ps", "--all", "--quiet")
    assert_initialized(installer, flags, prepared)
    if output(*compose, "ps", "--all", "--quiet") != container_ids:
        raise RuntimeError("re-running install replaced a healthy service")
    denied = subprocess.run([installer, "bootstrap-ticket", *flags], capture_output=True, text=True)
    if not denied.returncode or denied.stdout or "already initialized" not in denied.stderr:
        raise RuntimeError("installer allowed another initialization link after bootstrap")

    run(*compose, "down")
    if json.loads(output(installer, "status", *flags))["phase"] != "stopped":
        raise RuntimeError("status did not identify a stopped deployment")
    interrupted_rebuild(scratch, installer, flags, ca)
    run(*compose, "stop", "rendezvous")
    assert_initialized(installer, flags, prepared)
    if output(*compose, "ps", "--all", "--quiet") == container_ids:
        raise RuntimeError("the test did not rebuild the Compose services")
    print("Interrupted rebuild retained the database, deployment ID and both public keys.", flush=True)

    run(*compose, "down")
    record = installation / "database-ready"
    original_record = record.read_bytes()
    try:
        record.write_text(f"1 {prepared['deployment_id']} 1\n")
        assert_storage_refusal(installer, flags, compose, "different cluster")
    finally:
        record.write_bytes(original_record)

    database = installation / "postgres"
    original = installation / "postgres-original"
    database.rename(original)
    try:
        database.mkdir(mode=0o700)
        assert_storage_refusal(installer, flags, compose, "missing or incomplete")
    finally:
        database.rmdir()
        original.rename(database)
    print("A missing database or mismatched cluster ID was rejected before starting services.", flush=True)


def assert_storage_refusal(installer, flags, compose, reason):
    failed = subprocess.run([installer, "install", *flags], capture_output=True, text=True)
    if (not failed.returncode or failed.stdout or reason not in failed.stderr or
            output(*compose, "ps", "--all", "--quiet")):
        raise RuntimeError(f"installer did not stop on PostgreSQL storage conflict: {reason}")


def assert_initialized(installer, flags, prepared):
    for command in ("install", "status"):
        status = json.loads(output(installer, command, *flags))
        if (status["phase"] != "ready" or status["initialized"] is not True or
                any(status.get(key) != value for key, value in prepared.items())):
            raise RuntimeError(f"{command} did not report the initialized deployment and original identity")


def interrupted_rebuild(scratch, installer, flags, ca):
    alternate = scratch / "alternate-trust"
    alternate.mkdir()
    unrelated, _ = tls_context(alternate, f"DNS:{HOST}")
    trusted = ca.read_bytes()
    ca.write_bytes(unrelated.read_bytes())
    # Break HTTPS readiness after the database and migration are already up.
    try:
        with subprocess.Popen([installer, "install", *flags], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE) as process:
            try:
                deadline = time.monotonic() + 60
                while process.poll() is None and time.monotonic() < deadline:
                    status = json.loads(output(installer, "status", *flags))
                    if status["phase"] == "https" and "unknown authority" in status["detail"]:
                        break
                    time.sleep(0.3)
                else:
                    raise RuntimeError("did not reach the expected post-migration TLS failure during rebuild")
            finally:
                if process.poll() is None:
                    process.terminate()
                process.wait(timeout=15)
            if process.returncode == 0:
                raise RuntimeError("interrupted installer reported success")
    finally:
        ca.write_bytes(trusted)


def main():
    with tempfile.TemporaryDirectory(prefix="deskrealm-center-", dir="/tmp") as directory, ExitStack() as resources:
        scratch = Path(directory).resolve()
        exercise(scratch, publish_images(scratch, resources), resources)


if __name__ == "__main__":
    main()
