"""Build and exercise the center installation in a disposable Docker project."""

import argparse
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


def available_ports(default_relay):
    with ExitStack() as stack:
        ports = []
        for _ in range(4 if default_relay else 3):
            tcp = stack.enter_context(socket.socket())
            tcp.bind((LOOPBACK, 0))
            ports.append(tcp.getsockname()[1])
        udp = stack.enter_context(socket.socket(type=socket.SOCK_DGRAM))
        udp.bind((LOOPBACK, ports[2]))
        return ports


def publish_images(scratch, resources, offline, default_relay):
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
    binaries = ("--bin", "hbbs", "--bin", "hbbr") if default_relay else ("--bin", "hbbs")
    cargo(server, scratch / "cargo", SERVER_IMAGE, "build", "--locked", *binaries)
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
        labels = ("--label", f"org.deskrealm.test.offline={scratch.name}") if offline else ()
        run("docker", "build", "--quiet", "--platform", architecture,
            "--target", name, "--tag", tag, *labels,
            "--file", ROOT / "internal/m3/install/runtime.Dockerfile", context)
        push = output("docker", "push", tag)
        digest = re.search(r"^dev: digest: (sha256:[a-f0-9]{64}) size:", push, re.MULTILINE)
        if not digest:
            raise RuntimeError(f"local registry did not report a digest for {name}")
        references[name] = f"{repository}@{digest[1]}"
        resources.callback(cleanup, "docker", "image", "rm", references[name])
        print(f"Built {name} for {architecture} at {digest[1]}", flush=True)
    return references, registry


def prepare_offline_bundle(scratch, images, registry, resources):
    media = scratch / "bundle"
    media.mkdir()
    manifest = media / "manifest.json"
    archive = media / "images.tar"
    key, public = scratch / "signing-key.pem", scratch / "trusted-key.pem"
    signature = media / "manifest.sig"
    platform = output("docker", "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
    bindings = {}
    for role, reference in images.items():
        if role != "postgresql":
            run("docker", "pull", "--platform", platform, reference)
        image_id = output("docker", "image", "inspect", "--format", "{{.Id}}", reference)
        bindings[role] = {"reference": reference, "image_id": image_id}
        if role != "postgresql":
            resources.callback(cleanup, "docker", "image", "rm", image_id)
    run("docker", "image", "save", "--output", archive, *(entry["image_id"] for entry in bindings.values()))
    digest = hashlib.sha256()
    with archive.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    manifest.write_text(json.dumps({
        "version": 1, "platform": platform, "images": bindings,
        "archive": {"file": archive.name, "sha256": digest.hexdigest(), "bytes": archive.stat().st_size},
    }))
    run("openssl", "genpkey", "-algorithm", "ED25519", "-out", key)
    run("openssl", "pkey", "-in", key, "-pubout", "-out", public)
    run("openssl", "pkeyutl", "-sign", "-rawin", "-inkey", key, "-in", manifest, "-out", signature)
    run("docker", "rm", "--force", "--volumes", registry)
    for role in ("center", "rendezvous"):
        entry = bindings[role]
        reference = entry["reference"]
        cleanup("docker", "image", "rm", reference)
        cleanup("docker", "image", "rm", reference.split("@")[0] + ":dev")
        cleanup("docker", "image", "rm", entry["image_id"])
        absent = subprocess.run(["docker", "image", "inspect", entry["image_id"]], capture_output=True)
        if absent.returncode == 0:
            raise RuntimeError(f"{role} image unexpectedly remains cached before offline installation")
    print("Signed three pinned images and stopped their development registry before installer import.", flush=True)
    return {"manifest": str(manifest), "signature": str(signature), "trusted_public_key": str(public)}


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


def browser_case(scratch, origin, certificate, script, title, settings):
    key = run("openssl", "x509", "-in", certificate, "-pubkey", "-noout", stdout=subprocess.PIPE)
    der = run("openssl", "pkey", "-pubin", "-outform", "DER", input=key.stdout, stdout=subprocess.PIPE)
    spki = base64.b64encode(hashlib.sha256(der.stdout).digest()).decode()
    report = scratch / "chromium.json"
    run("npm", "--prefix", "web", "run", script, env={
        **os.environ,
        **settings,
        "DESKREALM_BROWSER_BASE": origin,
        "DESKREALM_BROWSER_TLS_SPKI": spki,
        "DESKREALM_BROWSER_LOCAL_HOST": HOST,
        "DESKREALM_BROWSER_OUTPUT": str(scratch / "playwright"),
        "DESKREALM_BROWSER_REPORT": str(report),
    })
    suites = json.loads(report.read_text())["suites"]
    specs = [spec for suite in suites for spec in suite.get("specs", [])]
    if len(specs) != 1 or specs[0]["title"] != title:
        raise RuntimeError(f"required Chromium regression is missing: {title}")
    tests = specs[0]["tests"]
    if not tests or any(t["expectedStatus"] != "passed" or not t["results"] or
                        any(r["status"] != "passed" for r in t["results"]) for t in tests):
        raise RuntimeError(f"required Chromium regression did not pass: {title}")


def start_default_relay(scratch, resources, origin, ca, prepared, token_file, port):
    work = scratch / "existing-relay"
    work.mkdir()
    binary = target_directory(scratch / "source" / "server") / "debug" / "hbbr"
    args = ("docker", "run", "--detach", "--publish", f"{LOOPBACK}:{port}:21117",
            "--mount", f"type=bind,src={binary},dst=/hbbr,readonly",
            "--mount", f"type=bind,src={work},dst=/relay",
            "--mount", f"type=bind,src={ca},dst=/relay-ca.pem,readonly",
            "--workdir", "/relay", "--entrypoint", "/hbbr")
    settings = {
        "DESKREALM_MANAGED_MODE": "1",
        "DESKREALM_MANAGED_CENTER": origin,
        "DESKREALM_MANAGED_CENTER_KID": prepared["signing_key_id"],
        "DESKREALM_MANAGED_CENTER_PUBLIC_KEY": prepared["signing_public_key"],
        "DESKREALM_MANAGED_RELAY_ID": prepared["default_relay"]["id"],
        "DESKREALM_MANAGED_RELAY_TOKEN": token_file.read_text().strip(),
        "DESKREALM_MANAGED_CA_FILE": "/relay-ca.pem",
    }
    env_file = work / "relay.env"
    env_file.touch(mode=0o600)
    env_file.write_text("".join(f"{name}={value}\n" for name, value in settings.items()))
    container = output(*args, "--env-file", env_file, SERVER_IMAGE, "-k", "_")
    resources.callback(cleanup, "docker", "rm", "--force", container)
    return container


def exercise(scratch, images, resources, offline=None, default_relay=False, install_task=False, center_upgrade=False):
    api_port, check_port, route_port, *extra_ports = available_ports(default_relay)
    origin = f"https://{HOST}:{api_port}"
    ca, _ = tls_context(scratch, f"DNS:{HOST}")
    certificate, private_key = scratch / "server.pem", scratch / "server.key"
    installation = scratch / "deployment"
    config = scratch / "center.json"
    configuration = {
        "version": 1, "directory": str(installation), "public_origin": origin,
        "id_server": f"{LOOPBACK}:{route_port}",
        "https": {"mode": "provided", "certificate": str(certificate),
                  "private_key": str(private_key), "ca_certificate": str(ca)},
        "images": images,
        "ports": {"rendezvous": route_port, "rendezvous_check": check_port, "bind_host": LOOPBACK},
    }
    if offline:
        configuration["offline"] = offline
    if default_relay:
        relay_port = extra_ports[0]
        relay_token = scratch / "default-relay.token"
        relay_token.touch(mode=0o600)
        relay_token.write_text(base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip("=") + "\n")
        configuration["default_relay"] = {
            "id": "existing-relay", "address": f"{LOOPBACK}:{relay_port}", "token_file": str(relay_token),
        }
    config.write_text(json.dumps(configuration))
    installer = scratch / "deskrealm-install"
    run("go", "build", "-o", installer, "./cmd/deskrealm-install")
    flags = ("--config", config)
    run(installer, "preflight", *flags)
    if default_relay:
        assert_image_failure(scratch, installer, configuration)
    prepared = json.loads(output(installer, "prepare", *flags))
    compose = ("docker", "compose", "--env-file", "/dev/null", "-f", prepared["compose_file"])
    upgrade_candidates = publish_upgrade_candidates(scratch, images["center"], resources) if center_upgrade else None
    resources.callback(cleanup, *compose, "down", "--volumes", "--remove-orphans")
    if json.loads(output(installer, "status", *flags))["phase"] != "prepared":
        raise RuntimeError("status did not identify a prepared deployment")
    pending = json.loads(output(installer, "install-record", *flags))
    if pending["version"] != 1 or any(step["state"] != "pending" for step in pending["steps"]):
        raise RuntimeError("a prepared installation did not have a pending task record")
    if default_relay:
        relay = start_default_relay(scratch, resources, origin, ca, prepared, relay_token, relay_port)
    if offline:
        archive = Path(offline["manifest"]).parent / "images.tar"
        with archive.open("r+b") as file:
            first = file.read(1)
            file.seek(0)
            file.write(bytes([first[0] ^ 1]))
        try:
            assert_install_refusal(installer, flags, compose, "archive SHA-256")
        finally:
            with archive.open("r+b") as file:
                file.write(first)
    installed = json.loads(output(installer, "install", *flags))
    if installed["phase"] != "ready" or installed["initialized"] is not False:
        raise RuntimeError("installation did not reach ready before issuing a ticket")
    if any(installed.get(key) != value for key, value in prepared.items()):
        raise RuntimeError("starting services changed the persisted deployment identity")
    if default_relay:
        if installed["default_relay"] != {"id": "existing-relay", "address": f"{LOOPBACK}:{relay_port}"}:
            raise RuntimeError("installer did not report the configured default relay")
        run("docker", "stop", relay, stdout=subprocess.DEVNULL)
        unreachable = json.loads(output(installer, "status", *flags))
        if unreachable["phase"] != "default-relay" or "unreachable" not in unreachable["detail"]:
            raise RuntimeError("installer advertised a ready center after the default relay became unavailable")
        run("docker", "start", relay, stdout=subprocess.DEVNULL)
        if json.loads(output(installer, "install", *flags))["phase"] != "ready":
            raise RuntimeError("installer did not recover after restarting the default relay")
        print("Existing managed hbbr configuration and TCP reachability verified independently from center startup.", flush=True)
    completed = json.loads(output(installer, "install-record", *flags))
    if (completed["task_id"] != pending["task_id"] or completed["revision"] <= pending["revision"] or
            not all(step["state"] == "completed" for step in completed["steps"])):
        raise RuntimeError("the installation task did not record all completed steps")
    if offline:
        media = Path(offline["manifest"]).parent
        media.rename(media.with_name("detached-bundle"))
        print("Signed archive imported by image ID; subsequent restarts use the cache with the media detached.", flush=True)
    wait_for_center(origin, api_port, ca, initialized=False)
    run(*compose, "exec", "-T", "rendezvous", "getent", "ahostsv4", HOST, stdout=subprocess.DEVNULL)
    print("Compose migration, CA-verified HTTPS and native hbbs readiness passed.", flush=True)

    ticket = output(installer, "bootstrap-ticket", *flags)
    if not re.fullmatch(re.escape(origin) + r"/initialize#ticket=[A-Za-z0-9_-]{43}", ticket):
        raise RuntimeError("host-side command did not produce a valid initialization link")
    browser_case(scratch, origin, certificate, "test:install", "HTTPS 中心首次管理员初始化和登录",
                 {"DESKREALM_BROWSER_TICKET": ticket})
    wait_for_center(origin, api_port, ca, initialized=True)
    print("Chromium first-administrator initialization and login passed.", flush=True)

    if center_upgrade:
        exercise_center_upgrade(scratch, installer, flags, compose, prepared, images["center"], upgrade_candidates)
        return

    container_ids = output(*compose, "ps", "--all", "--quiet")
    assert_initialized(installer, flags, prepared)
    if output(*compose, "ps", "--all", "--quiet") != container_ids:
        raise RuntimeError("re-running install replaced a healthy service")
    if json.loads(output(installer, "install-record", *flags)) != completed:
        raise RuntimeError("a healthy recheck changed the completed installation record")
    denied = subprocess.run([installer, "bootstrap-ticket", *flags], capture_output=True, text=True)
    if not denied.returncode or denied.stdout or "already initialized" not in denied.stderr:
        raise RuntimeError("installer allowed another initialization link after bootstrap")

    run(*compose, "down")
    if json.loads(output(installer, "status", *flags))["phase"] != "stopped":
        raise RuntimeError("status did not identify a stopped deployment")
    interrupted_rebuild(scratch, installer, flags, ca)
    interrupted = json.loads(output(installer, "install-record", *flags))
    interrupted_steps = {step["name"]: step["state"] for step in interrupted["steps"]}
    if interrupted["task_id"] != completed["task_id"] or interrupted_steps != {
            "images": "completed", "database": "completed", "services": "completed", "health": "interrupted"}:
        raise RuntimeError("rebuild did not record the actual interrupted step")
    if install_task:
        assert_import(installer, flags, interrupted, imported=True)
        assert_import(installer, flags, interrupted, imported=False)
    migration = output(*compose, "ps", "--all", "--quiet", "migrate")
    if not migration:
        raise RuntimeError("cannot identify the completed migration container")
    migration_started = output("docker", "inspect", "--format", "{{.State.StartedAt}}", migration)
    run(*compose, "stop", "rendezvous")
    assert_initialized(installer, flags, prepared)
    if (output(*compose, "ps", "--all", "--quiet", "migrate") != migration or
            output("docker", "inspect", "--format", "{{.State.StartedAt}}", migration) != migration_started):
        raise RuntimeError("resuming the rendezvous service re-ran an already completed migration")
    resumed = json.loads(output(installer, "install-record", *flags))
    if (resumed["task_id"] != interrupted["task_id"] or resumed["revision"] <= interrupted["revision"] or
            not all(step["state"] == "completed" for step in resumed["steps"])):
        raise RuntimeError("installation did not resume to completion on the same task")
    if output(*compose, "ps", "--all", "--quiet") == container_ids:
        raise RuntimeError("the test did not rebuild the Compose services")
    print("Interrupted rebuild retained the database, deployment ID and both public keys.", flush=True)
    print("Installation journal preserved the task across interruption; migration was not re-run to recover rendezvous.", flush=True)
    if install_task:
        assert_import(installer, flags, resumed, imported=True)
        assert_import(installer, flags, resumed, imported=False)
        browser_case(scratch, origin, certificate, "test:install-task", "任务中心显示中心安装结果和已导入的中断记录", {
            "DESKREALM_BROWSER_INSTALL_TASK": resumed["task_id"],
            "DESKREALM_BROWSER_INSTALL_REVISION": str(interrupted["revision"]),
        })
        print("Host import and Chromium verified the same task for interrupted and completed snapshots.", flush=True)

    run(*compose, "down")
    journal = installation / "install-record.json"
    original_journal = journal.with_suffix(".original")
    journal.rename(original_journal)
    try:
        assert_install_refusal(installer, flags, compose, "install-record.json")
    finally:
        original_journal.rename(journal)

    record = installation / "database-ready"
    original_marker = record.with_suffix(".original")
    record.rename(original_marker)
    try:
        assert_install_refusal(installer, flags, compose, "initialization record is missing")
    finally:
        original_marker.rename(record)

    original_record = record.read_bytes()
    try:
        record.write_text(f"1 {prepared['deployment_id']} 1\n")
        assert_install_refusal(installer, flags, compose, "different cluster")
    finally:
        record.write_bytes(original_record)

    database = installation / "postgres"
    original = installation / "postgres-original"
    database.rename(original)
    try:
        database.mkdir(mode=0o700)
        assert_install_refusal(installer, flags, compose, "missing or incomplete")
    finally:
        database.rmdir()
        original.rename(database)
    print("Missing journal, missing database record and mismatched PostgreSQL state were rejected before startup.", flush=True)


def publish_upgrade_candidates(scratch, original, resources):
    workspace = scratch / "upgrade"
    workspace.mkdir()
    arch = output("docker", "version", "--format", "{{.Server.Arch}}")
    (workspace / "broken.go").write_text("""package main
import ("fmt"; "os"; "syscall")
func main() {
    if len(os.Args) == 2 && os.Args[1] == "schema-version" {
        if incompatible := os.Getenv("DESKREALM_TEST_SCHEMA_VERSION"); incompatible != "" {
            fmt.Println(incompatible)
            return
        }
        if err := syscall.Exec("/original-deskrealm", os.Args, os.Environ()); err != nil {
            panic(err)
        }
    }
    os.Exit(1)
}
""")
    run("go", "build", "-trimpath", "-o", workspace / "broken", workspace / "broken.go",
        env={**os.environ, "GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0"})

    def publish(label, recipe):
        repository = original.split("@", 1)[0]
        tag = f"{repository}:{label}"
        resources.callback(cleanup, "docker", "image", "rm", tag)
        recipe_file = workspace / "Dockerfile"
        recipe_file.write_text(recipe)
        run("docker", "build", "--quiet", "--platform", f"linux/{arch}", "-f", recipe_file, "-t", tag, workspace)
        push = output("docker", "push", tag)
        digest = re.search(rf"^{label}: digest: (sha256:[a-f0-9]{{64}}) size:", push, re.MULTILINE)
        if not digest:
            raise RuntimeError(f"local registry did not report a digest for {label}")
        reference = f"{repository}@{digest[1]}"
        resources.callback(cleanup, "docker", "image", "rm", reference)
        return reference

    good = publish("upgrade-good", f'FROM {original}\nLABEL org.deskrealm.test.variant="candidate"\n')
    bad = publish("upgrade-bad", f"""FROM {original} AS original
FROM original
COPY --from=original /usr/local/bin/deskrealm /original-deskrealm
COPY broken /usr/local/bin/deskrealm
""")
    incompatible = publish("upgrade-incompatible", f"FROM {bad}\nENV DESKREALM_TEST_SCHEMA_VERSION=0\n")
    return good, bad, incompatible, arch


def exercise_center_upgrade(scratch, installer, flags, compose, prepared, original, candidates):
    good, bad, incompatible, arch = candidates
    workspace = scratch / "upgrade"

    def restore(label):
        backup = workspace / f"backup-{label}"
        destination = workspace / f"recover-{label}"
        result = json.loads(output(installer, "backup", *flags, "--output", backup))
        verified = json.loads(output(installer, "restore-check", "--backup", backup,
                                     "--sha256", result["backup_sha256"], "--output", destination))
        if verified["database"]["users"] != 1 or not verified["database"]["initialized"]:
            raise RuntimeError(f"restored copy lost administrator state: {label}")
        return destination, verified

    def service_id(service):
        container = output(*compose, "ps", "--all", "--quiet", service)
        if not container:
            raise RuntimeError(f"cannot find {service} container")
        return container

    def current_image():
        return output("docker", "inspect", "--format", "{{.Config.Image}}", service_id("center"))

    def assert_identity():
        status = json.loads(output(installer, "status", *flags))
        if status["phase"] != "ready" or not status["initialized"]:
            raise RuntimeError("center did not come back initialized and ready")
        if any(status.get(key) != value for key, value in prepared.items()):
            raise RuntimeError("center upgrade changed the base identity")

    stable_services = ("database", "rendezvous", "migrate")
    stable = {service: service_id(service) for service in stable_services}
    destination, recovered = restore("original")
    missing = subprocess.run([installer, "upgrade", *flags, "--image", good, "--recovery", workspace / "missing"],
                             capture_output=True, text=True)
    if not missing.returncode or current_image() != original:
        raise RuntimeError("center upgrade accepted a missing restore verification")
    incompatible_result = subprocess.run(
        [installer, "upgrade", *flags, "--image", incompatible, "--recovery", destination],
        capture_output=True, text=True,
    )
    if (not incompatible_result.returncode or "running schema version" not in incompatible_result.stderr
            or incompatible_result.stdout or current_image() != original):
        raise RuntimeError(f"center upgrade accepted a mismatched candidate schema: {incompatible_result.stderr}")
    failure = subprocess.run([installer, "upgrade", *flags, "--image", bad, "--recovery", destination],
                             capture_output=True, text=True)
    rolled_back = json.loads(failure.stdout) if failure.stdout else {}
    if not failure.returncode or rolled_back.get("state") != "rolled_back" or current_image() != original:
        raise RuntimeError(f"failed center did not restore the original image: {failure.stdout} {failure.stderr}")
    assert_identity()
    repeated = subprocess.run([installer, "upgrade", *flags, "--image", bad], capture_output=True, text=True)
    if not repeated.returncode or json.loads(repeated.stdout) != rolled_back:
        raise RuntimeError("repeating a failed operation replaced its rollback record")
    print("Failed center candidate rolled back to the original digest and both public keys.", flush=True)

    upgraded = json.loads(output(installer, "upgrade", *flags, "--image", good, "--recovery", destination))
    if (upgraded["state"] != "succeeded" or current_image() != good or upgraded["database"] != rolled_back["database"]
            or upgraded["backup_sha256"] != recovered["backup_sha256"]):
        raise RuntimeError(f"center upgrade did not retain the verified database: {upgraded}")
    assert_identity()
    instance = service_id("center")
    if json.loads(output(installer, "upgrade", *flags, "--image", good)) != upgraded:
        raise RuntimeError("repeating a successful operation changed its verification record")
    if json.loads(output(installer, "install", *flags))["phase"] != "ready" or service_id("center") != instance:
        raise RuntimeError("normal install changed the center override")
    if {service: service_id(service) for service in stable_services} != stable:
        raise RuntimeError("upgrade changed a database, migration, or rendezvous container")
    _, updated = restore("upgraded")
    if json.loads(output(installer, "upgrade-status", "--config", updated["config"])) != upgraded:
        raise RuntimeError("restoring the upgraded center lost its image record")
    print(f"Healthy center upgrade and isolated recovery passed without replacing untouched services: {arch}.", flush=True)


def assert_install_refusal(installer, flags, compose, reason):
    failed = subprocess.run([installer, "install", *flags], capture_output=True, text=True)
    if (not failed.returncode or failed.stdout or reason not in failed.stderr or
            output(*compose, "ps", "--all", "--quiet")):
        raise RuntimeError(f"installer did not stop at expected failed check: {reason}")


def assert_import(installer, flags, record, imported):
    result = json.loads(output(installer, "import-record", *flags, "--actor", "install-admin"))
    if result != {"task_id": record["task_id"], "revision": record["revision"], "imported": imported}:
        raise RuntimeError("installation task import changed the task identity or replay outcome")


def assert_image_failure(scratch, installer, configuration):
    config = scratch / "unavailable-image.json"
    wrong = {**configuration, "directory": str(scratch / "unavailable-image")}
    reference = configuration["images"]["center"].split("@")[0]
    wrong["images"] = {**configuration["images"], "center": f"{reference}@sha256:{'0' * 64}"}
    config.write_text(json.dumps(wrong))
    flags = ("--config", config)
    prepared = json.loads(output(installer, "prepare", *flags))
    compose = ("docker", "compose", "--env-file", "/dev/null", "-f", prepared["compose_file"])
    assert_install_refusal(installer, flags, compose, "center image: cannot pull")
    record = json.loads(output(installer, "install-record", *flags))
    steps = {step["name"]: step["state"] for step in record["steps"]}
    if steps != {"images": "failed", "database": "pending", "services": "pending", "health": "pending"}:
        raise RuntimeError("unavailable image was not recorded at the image step")
    print("Unavailable pinned image stopped installation and recorded the failed image step.", flush=True)


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
                    journal = json.loads(output(installer, "install-record", *flags))
                    if any(step["name"] == "health" and step["state"] == "running" for step in journal["steps"]):
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
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument("--offline", action="store_true")
    modes.add_argument("--default-relay", action="store_true")
    modes.add_argument("--install-task", action="store_true")
    modes.add_argument("--center-upgrade", action="store_true")
    options = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="deskrealm-center-", dir="/tmp") as directory, ExitStack() as resources:
        scratch = Path(directory).resolve()
        images, registry = publish_images(scratch, resources, options.offline, options.default_relay)
        offline = prepare_offline_bundle(scratch, images, registry, resources) if options.offline else None
        exercise(scratch, images, resources, offline, options.default_relay, options.install_task, options.center_upgrade)


if __name__ == "__main__":
    main()
