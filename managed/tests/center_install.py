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

from native_coord_fixture import field, fields, receive, send, tls_context
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


def wait_for_rendezvous(port):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        try:
            with socket.create_connection((LOOPBACK, port), timeout=1) as connection:
                # RustDesk RendezvousMessage TestNatRequest and TestNatResponse tags.
                send(connection, field(20, b""))
                response = fields(receive(connection)[21])
                if not 0 < response.get(1, 0) < 65536:
                    raise RuntimeError("hbbs returned an invalid NAT probe port")
                return
        except (OSError, EOFError):
            time.sleep(0.3)
    raise RuntimeError("hbbs NAT probe timed out")


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
        "images": images, "ports": {"rendezvous": route_port, "rendezvous_check": check_port},
    }))
    installer = scratch / "deskrealm-install"
    run("go", "build", "-o", installer, "./cmd/deskrealm-install")
    run(installer, "preflight", "--config", config)
    prepared = json.loads(output(installer, "prepare", "--config", config))
    override = scratch / "loopback.yaml"
    override.write_text(
        "networks:\n  control:\n    driver_opts:\n"
        f"      com.docker.network.bridge.host_binding_ipv4: {LOOPBACK}\n"
    )
    compose = ("docker", "compose", "--env-file", "/dev/null", "-f", prepared["compose_file"], "-f", override)
    resources.callback(cleanup, *compose, "down", "--volumes", "--remove-orphans")
    run(*compose, "up", "--detach", "--wait", "--wait-timeout", "90")
    wait_for_center(origin, api_port, ca, initialized=False)
    wait_for_rendezvous(route_port)
    with socket.create_connection((LOOPBACK, check_port), timeout=2):
        pass
    run(*compose, "exec", "-T", "rendezvous", "getent", "ahostsv4", HOST, stdout=subprocess.DEVNULL)
    if json.loads(output(installer, "prepare", "--config", config)) != prepared:
        raise RuntimeError("starting services changed the persisted deployment identity")
    print("Compose migration, CA-verified HTTPS and native hbbs readiness passed.", flush=True)

    ticket = output(*compose, "exec", "-T", "center", "/usr/local/bin/deskrealm", "bootstrap-ticket")
    if not re.fullmatch(re.escape(origin) + r"/initialize#ticket=[A-Za-z0-9_-]{43}", ticket):
        raise RuntimeError("host-side command did not produce a valid initialization link")
    browser_bootstrap(scratch, origin, certificate, ticket)
    wait_for_center(origin, api_port, ca, initialized=True)
    print("Chromium first-administrator initialization and login passed.", flush=True)


def main():
    with tempfile.TemporaryDirectory(prefix="deskrealm-center-", dir="/tmp") as directory, ExitStack() as resources:
        scratch = Path(directory).resolve()
        exercise(scratch, publish_images(scratch, resources), resources)


if __name__ == "__main__":
    main()
