"""Run patched hbbs and hbbr against local signed peers and a TLS center fixture."""

import base64
import contextlib
import hashlib
import http.server
import json
import os
from pathlib import Path
import socket
import ssl
import struct
import subprocess
import sys
import tempfile
import threading
import time
import uuid

TARGET = "10700002"
RELAY_ID = "fixture-relay"
ROUTE_ENDPOINT = "/v1/internal/managed/rendezvous/authorize"
PAIR_ENDPOINT = "/v1/internal/managed/relay/paired"
LISTEN = "127.0.0.1"
RENDEZVOUS_TOKEN = bytes(range(32))
RELAY_TOKEN = bytes(range(32, 64))


def b64(raw):
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def varint(n):
    result = bytearray()
    while n >= 128:
        result.append((n & 127) | 128)
        n >>= 7
    result.append(n)
    return bytes(result)


def field(n, value):
    if isinstance(value, int):
        return varint(n << 3) + varint(value)
    if isinstance(value, str):
        value = value.encode()
    return varint(n << 3 | 2) + varint(len(value)) + value


def fields(data):
    result = {}
    pos = 0

    def integer():
        nonlocal pos
        out = shift = 0
        while pos < len(data) and shift < 64:
            byte = data[pos]
            pos += 1
            out |= (byte & 127) << shift
            if byte < 128:
                return out
            shift += 7
        raise ValueError("malformed fixture varint")

    while pos < len(data):
        tag = integer()
        number, wire = tag >> 3, tag & 7
        if wire == 0:
            result[number] = integer()
        elif wire == 2:
            size = integer()
            if size > len(data) - pos:
                raise ValueError("truncated fixture record")
            result[number] = data[pos:pos + size]
            pos += size
        else:
            raise ValueError(f"unexpected wire kind {wire}")
    return result


def send(sock, message):
    width = max(1, ((len(message) << 2).bit_length() + 7) // 8)
    sock.sendall(((len(message) << 2) | (width - 1)).to_bytes(width, "little") + message)


def take(sock, count):
    data = bytearray()
    while len(data) < count:
        part = sock.recv(count - len(data))
        if not part:
            raise EOFError
        data.extend(part)
    return bytes(data)


def receive(sock):
    first = take(sock, 1)
    header = first + take(sock, first[0] & 3)
    return fields(take(sock, int.from_bytes(header, "little") >> 2))


def envelope(operation, payload, route=None, offer=b""):
    data = field(operation, payload)
    if route is not None:
        data += field(1, route.wire)
    if offer:
        data += field(10, offer)
    return field(40, data)


class SigningKey:
    def __init__(self, path, seed):
        self.path = path
        self.path.write_bytes(bytes.fromhex("302e020100300506032b657004220420") + seed)
        public = subprocess.check_output(
            ["openssl", "pkey", "-inform", "DER", "-in", path, "-pubout", "-outform", "DER"],
            stderr=subprocess.PIPE,
        )
        self.public = public[-32:]

    def sign(self, message):
        with tempfile.NamedTemporaryFile() as data:
            data.write(message)
            data.flush()
            return subprocess.check_output([
                "openssl", "pkeyutl", "-sign", "-rawin", "-keyform", "DER",
                "-inkey", self.path, "-in", data.name,
            ])


class Route:
    def __init__(self, fixture, path, *, expired=False, target=TARGET, allowed=1, purpose="helpdesk", attended=True):
        self.center = fixture
        self.path = path
        self.target = target
        self.allowed, self.purpose = allowed, purpose
        self.attended = attended
        self.session, self.grant, self.connection = uuid.uuid4(), uuid.uuid4(), uuid.uuid4()
        now = time.time_ns() // 1_000_000
        self.issued = now - (120_000 if expired else 5_000)
        self.expires = self.issued + 60_000
        fields = [
            b"fixture-center", fixture.org.bytes, self.session.bytes, self.grant.bytes, fixture.user.bytes,
            fixture.controller_id.bytes, fixture.target_id.bytes, hashlib.sha256(fixture.controller.public).digest(),
            hashlib.sha256(fixture.target.public).digest(), struct.pack(">Q", 1), bytes([allowed]),
            bytes([1 if attended else 2]), purpose.encode(),
            struct.pack(">Q", self.issued), struct.pack(">Q", self.expires),
        ]
        self.frame = b"DR-M2\0\x01\x03" + b"".join(struct.pack(">H", len(v)) + v for v in fields)
        self.signature = fixture.center.sign(self.frame)
        self.bound = hashlib.sha256(self.frame + self.signature).digest()
        self.wire = field(1, self.frame) + field(2, self.signature) + field(3, self.connection.bytes) + field(4, target)
        fixture.routes[str(self.session)] = self

    def proof(self, key, purpose, role, challenge, offer):
        frame = (
            b"DeskRealm/M2/transport-proof/v1\0" + bytes([purpose, role]) + self.bound +
            self.connection.bytes + challenge + (offer or bytes(32)) + self.target.encode()
        )
        return envelope(12, field(1, key.public) + field(2, key.sign(frame)))

    def request(self):
        return {
            "session_id": str(self.session), "grant_id": str(self.grant), "grant_signature": b64(self.signature),
            "controller_device_id": str(self.center.controller_id), "target_device_id": str(self.center.target_id),
            "target_rustdesk_id": self.target, "connection_id": str(self.connection),
            "path": self.path, "relay_id": RELAY_ID if self.path == "relay" else "",
        }


def tls_context(directory, subject_alt_name="IP:127.0.0.1"):
    ca, ca_key = directory / "ca.pem", directory / "ca.key"
    tls_cert, tls_key, csr = [directory / name for name in ("server.pem", "server.key", "server.csr")]
    ext = directory / "server.ext"
    ext.write_text(f"subjectAltName={subject_alt_name}\nextendedKeyUsage=serverAuth\n")
    commands = [
        ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=DeskRealm fixture CA", "-days", "2",
         "-keyout", ca_key, "-out", ca, "-addext", "basicConstraints=critical,CA:TRUE"],
        ["req", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=DeskRealm fixture", "-keyout", tls_key, "-out", csr],
        ["x509", "-req", "-in", csr, "-CA", ca, "-CAkey", ca_key, "-CAcreateserial", "-days", "2",
         "-extfile", ext, "-out", tls_cert],
    ]
    for args in commands:
        subprocess.run(["openssl", *args], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(tls_cert, tls_key)
    return ca, context


class Fixture:
    def __init__(self, directory):
        self.center, self.controller, self.target = (
            SigningKey(directory / f"{role}.der", bytes([seed]) * 32)
            for role, seed in (("center", 1), ("controller", 2), ("target", 3))
        )
        self.org, self.user, self.controller_id, self.target_id = [uuid.uuid4() for _ in range(4)]
        self.routes = {}
        self.denied = set()
        self.deny_pair = set()
        self.requests = []
        self.allow_pair_response = threading.Event()
        self.allow_pair_response.set()
        self.directory = directory

    def session_request(self, path, authorization, body):
        return None

    def policy_request(self, path, authorization):
        return None

    @contextlib.contextmanager
    def center_server(self):
        fixture = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def reply_json(self, result):
                status, body = result
                payload = b"" if status == 204 else json.dumps(body).encode()
                try:
                    self.send_response(status)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(payload)))
                    self.end_headers()
                    if payload:
                        self.wfile.write(payload)
                except (BrokenPipeError, ConnectionResetError, ssl.SSLEOFError):
                    # A supervisor can cancel an in-flight fixture request.
                    pass

            def do_GET(self):
                result = fixture.policy_request(self.path, self.headers.get("Authorization"))
                self.reply_json(result or (404, {}))

            def do_POST(self):
                request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                session_result = fixture.session_request(self.path, self.headers.get("Authorization"), request)
                if session_result is not None:
                    self.reply_json(session_result)
                    return
                route = fixture.routes.get(request.get("session_id"))
                token = b64(RENDEZVOUS_TOKEN) if self.path == ROUTE_ENDPOINT else b64(RELAY_TOKEN)
                prefix = "drr_" if self.path == ROUTE_ENDPOINT else "drl_"
                status = 403
                if (self.path == ROUTE_ENDPOINT and route and route.connection is None and
                        self.headers.get("Authorization") == "Bearer drr_" + b64(RENDEZVOUS_TOKEN)):
                    route.connection = uuid.UUID(request["connection_id"])
                if self.path in (ROUTE_ENDPOINT, PAIR_ENDPOINT) and route and (
                    self.headers.get("Authorization") == "Bearer " + prefix + token
                    and request == route.request() and route.target == TARGET and route.session not in fixture.denied
                ):
                    if self.path == ROUTE_ENDPOINT or (
                        route.path == "relay" and route.session not in fixture.deny_pair and
                        any(p == ROUTE_ENDPOINT and sid == route.session and code == 204 for p, sid, code in fixture.requests)
                    ):
                        status = 204
                fixture.requests.append((self.path, route.session if route else None, status))
                if self.path == PAIR_ENDPOINT and not fixture.allow_pair_response.wait(3):
                    status = 503
                self.send_response(status)
                self.end_headers()

        ca, context = tls_context(self.directory)
        with http.server.ThreadingHTTPServer((LISTEN, 0), Handler) as service:
            service.socket = context.wrap_socket(service.socket, server_side=True)
            thread = threading.Thread(target=service.serve_forever, daemon=True)
            thread.start()
            try:
                yield service.server_port, ca
            finally:
                service.shutdown()
                thread.join()


def ports():
    for _ in range(30):
        with contextlib.ExitStack() as stack:
            reservation = stack.enter_context(socket.socket())
            reservation.bind((LISTEN, 0))
            port = reservation.getsockname()[1]
            if not 5000 < port < 65530:
                continue
            try:
                for offset in (-1, 1, 2, 3, 4):
                    stack.enter_context(socket.socket()).bind((LISTEN, port + offset))
                stack.enter_context(socket.socket(type=socket.SOCK_DGRAM)).bind((LISTEN, port))
            except OSError:
                continue
            return port, port + 1
    raise RuntimeError("could not reserve fixture ports")


@contextlib.contextmanager
def services(binary, fixture, center_port, ca):
    hbbs, hbbr = ports()
    fixed = {key: value for key, value in os.environ.items() if not key.startswith("DESKREALM_MANAGED_")}
    fixed.update(
        DESKREALM_MANAGED_MODE="1", DESKREALM_MANAGED_CENTER=f"https://{LISTEN}:{center_port}",
        DESKREALM_MANAGED_CENTER_KID="fixture-center", DESKREALM_MANAGED_CENTER_PUBLIC_KEY=b64(fixture.center.public),
        DESKREALM_MANAGED_CA_FILE=str(ca), DESKREALM_MANAGED_RELAY_ID=RELAY_ID, TEST_HBBS="no",
    )
    with contextlib.ExitStack() as stack:
        for name, port, configuration in (
            ("hbbs", hbbs, dict(DESKREALM_MANAGED_RENDEZVOUS_TOKEN=b64(RENDEZVOUS_TOKEN),
                                 DESKREALM_MANAGED_RELAY_ADDRESS=f"{LISTEN}:{hbbr}")),
            ("hbbr", hbbr, dict(DESKREALM_MANAGED_RELAY_TOKEN=b64(RELAY_TOKEN))),
        ):
            folder = fixture.directory / name
            folder.mkdir()
            log = stack.enter_context((folder / "output.log").open("wb"))
            process = subprocess.Popen([binary / name, "-p", str(port), "-k", "-"], cwd=folder,
                                       env={**fixed, **configuration}, stdout=log, stderr=log)

            def stop(process):
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=3)

            stack.callback(stop, process)
            for _ in range(80):
                if process.poll() is not None:
                    raise RuntimeError((folder / "output.log").read_text())
                try:
                    with socket.create_connection((LISTEN, port), timeout=0.2):
                        break
                except OSError:
                    time.sleep(0.05)
            else:
                raise RuntimeError(f"{name} startup timed out")
        yield hbbs, hbbr


def closed(sock):
    try:
        assert sock.recv(1) == b"", "denied socket emitted data"
    except ConnectionResetError:
        pass


def silent(sock, *, datagram=False):
    original = sock.gettimeout()
    try:
        sock.settimeout(0.25)
        incoming = sock.recv(4096)
        raise AssertionError(f"expected no {'UDP offer' if datagram else 'TCP data'}, got {len(incoming)} bytes")
    except TimeoutError:
        pass
    finally:
        sock.settimeout(original)


def begin(sock, route, operation, payload, key, purpose, role, offer=b""):
    send(sock, envelope(operation, payload, route, offer))
    challenge = fields(receive(sock)[40])[11]
    assert len(challenge) == 32
    proof = route.proof(key, purpose, role, challenge, offer)
    send(sock, proof)
    return proof


def coordinate(port, target_socket, route, controller_key, target_key):
    direct = route.path == "direct"
    with socket.create_connection((LISTEN, port), timeout=5) as controller:
        begin(controller, route, 2 if direct else 8, field(1, TARGET), controller_key,
              1 if direct else 2, 1)
        forwarded = fields(fields(target_socket.recv(4096))[40])
        assert fields(forwarded[1]) == fields(route.wire)
        offer = forwarded[10]
        assert len(offer) == 32
        if direct:
            assert fields(forwarded[3])[1]
        else:
            relay = fields(forwarded[8])
            assert relay[2].decode() == str(route.connection) and relay[5] == 1
        with socket.create_connection((LISTEN, port), timeout=5) as target:
            begin(target, route, 4 if direct else 9, b"", target_key, 3 if direct else 4, 2, offer)
            response = fields(receive(controller)[40])
        assert fields(response[1]) == fields(route.wire)
        assert response[10] == offer
        payload = fields(response[7 if direct else 9])
        if direct:
            assert payload[1] and payload[2]
        else:
            assert payload[2].decode() == str(route.connection) and payload[5]
    return offer


def run(fixture, hbbs, hbbr):
    upgrade = (b"GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
               b"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
    for port in (hbbs + 2, hbbr + 2):
        with socket.create_connection((LISTEN, port), timeout=5) as websocket:
            try:
                websocket.sendall(upgrade)
            except ConnectionResetError:
                continue
            closed(websocket)

    with socket.socket(type=socket.SOCK_DGRAM) as target_udp:
        target_udp.settimeout(5)
        target_udp.bind((LISTEN, 0))
        registration = lambda public: field(15, field(1, TARGET) + field(2, fixture.target_id.bytes) + field(3, public))
        with socket.create_connection((LISTEN, hbbs), timeout=5) as legacy:
            send(legacy, field(8, field(1, TARGET)))
            closed(legacy)
        target_udp.sendto(registration(fixture.target.public), (LISTEN, hbbs))
        assert 16 in fields(target_udp.recv(4096))
        target_udp.sendto(field(10, field(2, TARGET)), (LISTEN, hbbs))
        silent(target_udp, datagram=True)
        with socket.create_connection((LISTEN, hbbr), timeout=5) as legacy:
            send(legacy, field(18, field(2, str(uuid.uuid4()))))
            closed(legacy)
        direct = Route(fixture, "direct")
        expired = Route(fixture, "direct", expired=True)
        for route in (expired, Route(fixture, "direct", target="other-id")):
            with socket.create_connection((LISTEN, hbbs), timeout=5) as peer:
                send(peer, envelope(2, field(1, TARGET), route))
                closed(peer)
        assert not fixture.requests
        with socket.create_connection((LISTEN, hbbs), timeout=5) as wrong:
            begin(wrong, direct, 2, field(1, TARGET), fixture.target, 1, 1)
            closed(wrong)
        silent(target_udp, datagram=True)
        assert not fixture.requests

        refused = Route(fixture, "direct")
        fixture.denied.add(refused.session)
        with socket.create_connection((LISTEN, hbbs), timeout=5) as controller:
            begin(controller, refused, 2, field(1, TARGET), fixture.controller, 1, 1)
            closed(controller)
        silent(target_udp, datagram=True)
        assert (ROUTE_ENDPOINT, refused.session, 403) in fixture.requests

        target_udp.sendto(registration(fixture.controller.public), (LISTEN, hbbs))
        assert 16 in fields(target_udp.recv(4096))
        with socket.create_connection((LISTEN, hbbs), timeout=5) as controller:
            begin(controller, direct, 2, field(1, TARGET), fixture.controller, 1, 1)
            closed(controller)
        silent(target_udp, datagram=True)
        target_udp.sendto(registration(fixture.target.public), (LISTEN, hbbs))
        assert 16 in fields(target_udp.recv(4096))
        coordinate(hbbs, target_udp, direct, fixture.controller, fixture.target)

        with socket.create_connection((LISTEN, hbbs), timeout=5) as first:
            proof = begin(first, direct, 2, field(1, TARGET), fixture.controller, 1, 1)
            assert 3 in fields(fields(target_udp.recv(4096))[40])
            first.shutdown(socket.SHUT_RDWR)
        with socket.create_connection((LISTEN, hbbs), timeout=5) as replay:
            send(replay, envelope(2, field(1, TARGET), direct))
            assert fields(receive(replay)[40])[11]
            send(replay, proof)
            closed(replay)

        for deny in (True, False):
            route = Route(fixture, "relay")
            if deny:
                fixture.deny_pair.add(route.session)
            offer = coordinate(hbbs, target_udp, route, fixture.controller, fixture.target)
            fixture.allow_pair_response.clear()
            try:
                with socket.create_connection((LISTEN, hbbr), timeout=5) as source, \
                     socket.create_connection((LISTEN, hbbr), timeout=5) as duplicate, \
                     socket.create_connection((LISTEN, hbbr), timeout=5) as target:
                    begin(source, route, 13, 1, fixture.controller, 5, 1, offer)
                    silent(source)
                    begin(duplicate, route, 13, 1, fixture.controller, 5, 1, offer)
                    closed(duplicate)
                    with socket.create_connection((LISTEN, hbbr), timeout=5) as wrong_key:
                        begin(wrong_key, route, 13, 2, fixture.controller, 5, 2, offer)
                        closed(wrong_key)
                    assert not any(p == PAIR_ENDPOINT and sid == route.session for p, sid, _ in fixture.requests)
                    begin(target, route, 13, 2, fixture.target, 5, 2, offer)
                    silent(source)
                    silent(target)
                    fixture.allow_pair_response.set()
                    if deny:
                        closed(source)
                        closed(target)
                    else:
                        assert fields(receive(source)[40])[14] == 1
                        assert fields(receive(target)[40])[14] == 1
                        source.sendall(b"fixture-controller-payload")
                        assert take(target, len(b"fixture-controller-payload")) == b"fixture-controller-payload"
                        target.sendall(b"fixture-target-payload")
                        assert take(source, len(b"fixture-target-payload")) == b"fixture-target-payload"
            finally:
                fixture.allow_pair_response.set()
            assert (PAIR_ENDPOINT, route.session, 403 if deny else 204) in fixture.requests

    print("Managed hbbs routes and hbbr pairing passed with signed peers and a TLS center fixture.", flush=True)


def main():
    with tempfile.TemporaryDirectory(prefix="deskrealm-coordination-") as work:
        fixture = Fixture(Path(work))
        try:
            with fixture.center_server() as (port, ca), services(Path(sys.argv[1]), fixture, port, ca) as (hbbs, hbbr):
                run(fixture, hbbs, hbbr)
        except Exception:
            for log in sorted(fixture.directory.glob("*/output.log")):
                print(f"{log.parent.name}:\n{log.read_text()}", file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
