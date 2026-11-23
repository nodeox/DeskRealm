"""Exercise RustDesk's native managed base crate against running hbbs/hbbr."""

import base64
from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import uuid

from native_coord_fixture import Fixture, LISTEN, PAIR_ENDPOINT, RELAY_ID, Route, TARGET, b64, services


def signed_frame(kind, fields):
    return b"DR-M2\0\x01" + bytes([kind]) + b"".join(len(v).to_bytes(2, "big") + v for v in fields)


def bearer(prefix, byte):
    return prefix + b64(bytes([byte]) * 32)


def decoded(raw):
    return base64.urlsafe_b64decode(raw + "==")


def confirmed_mask(purpose):
    if purpose.endswith("-file"):
        return 16, ["file_transfer"]
    if purpose.endswith("-clipboard"):
        return 9, ["view", "clipboard"]
    if purpose.endswith("-audio"):
        return 5, ["view", "audio"]
    if purpose.endswith("-input"):
        return 3, ["view", "input"]
    return 1, ["view"]


class SessionFixture(Fixture):
    def __init__(self, path):
        super().__init__(path)
        self.redemptions = {}
        self.file_events = {}
        self.file_retries = {}
        self.renewals = {}
        self.supervised_renew_attempts = {}
        self.supervised_receipts = {}
        self.revoked_renewals = set()
        self.status_reads = {}
        self.results = []
        self.stopped = set()
        self.device_challenge = None
        self.device_authenticated = False
        self.heartbeats = 0
        self.policy_gets = 0
        self.policy_receipts = []
        self.delivery = None

    def policy_request(self, path, authorization):
        if path.startswith("/v1/sessions/"):
            route = self.routes.get(path.split("/")[3])
            if (route is None or route.session not in self.redemptions
                    or path != f"/v1/sessions/{route.session}/status?connection_id={route.connection}"
                    or authorization != "Bearer " + bearer("drc_", 13)):
                return 403, {}
            reads = self.status_reads.get(route.session, 0) + 1
            self.status_reads[route.session] = reads
            if route.session in self.stopped:
                state = "stopped"
            elif route.purpose == "relay-supervised" and route.session in self.renewals:
                return 503, {}
            elif route.purpose == "direct-supervised" and route.session in self.supervised_renew_attempts:
                state = "revocation_requested"
            elif route.purpose == "relay-admit" and reads >= 3:
                state = "revocation_requested"
            else:
                state = "active"
            return 200, {"connection_id": str(route.connection), "state": state,
                         "lease_sequence": 2 if route.session in self.renewals else 1}
        if (path != f"/v1/managed/devices/{self.target_id}/policy"
                or authorization != "Bearer " + bearer("drd_", 12)
                or not self.device_authenticated):
            return 401, {}
        self.policy_gets += 1
        shapes = (
            (1, ["view", "input"], ["support", "helpdesk"], 3),
            (2, ["view"], [], 1),
            (1, ["view"], [], 1),
            None,
            (1, [], [], 0),
        )
        shape = shapes[self.policy_gets - 1]
        if shape is None:
            self.delivery = None
            return 200, {"policy": None}
        schema, actions, purposes, mask = shape
        raw = b"DR-M2-policy-v1\0" + bytes([mask]) + b"".join(
            purpose.encode() + b"\0" for purpose in sorted(purposes)
        )
        org = uuid.uuid4() if self.policy_gets == 3 else self.org
        self.delivery = {
            "device_id": str(self.target_id), "organization_id": str(org), "delivery_id": str(uuid.uuid4()),
            "policy_version": self.policy_gets, "key_epoch": 1, "schema_version": schema,
            "allowed_actions": actions, "unattended_purposes": purposes,
            "payload_sha256": hashlib.sha256(raw).hexdigest(),
        }
        return 200, {"policy": self.delivery}

    def device_request(self, path, authorization, body):
        if body.get("device_id") != str(self.target_id):
            return 403, {}
        if path == "/v1/managed/devices/challenge":
            if authorization is not None or body["protocol_version"] != 1:
                return 403, {}
            challenge = uuid.uuid4()
            expires = time.time_ns() // 1_000_000 + 45_000
            nonce = os.urandom(32)
            self.device_challenge = (challenge, expires, nonce)
            return 200, {
                "device_id": str(self.target_id), "organization_id": str(self.org), "challenge_id": str(challenge),
                "nonce": b64(nonce), "expires_ms": expires, "key_epoch": 1, "protocol_version": 1,
            }
        if path == "/v1/managed/devices/complete":
            challenge, expires, nonce = self.device_challenge
            frame = signed_frame(1, [
                self.org.bytes, self.target_id.bytes, challenge.bytes, nonce, (1).to_bytes(8, "big"),
                expires.to_bytes(8, "big"), (1).to_bytes(2, "big"),
            ])
            if (authorization is not None or body["challenge_id"] != str(challenge)
                    or decoded(body["signature"]) != self.target.sign(frame)):
                return 403, {}
            self.device_authenticated = True
            self.device_challenge = None
            expiry = datetime.now(timezone.utc) + timedelta(minutes=15)
            return 201, {"credential": bearer("drd_", 12), "expires_at": expiry.isoformat()}
        if authorization != "Bearer " + bearer("drd_", 12) or not self.device_authenticated:
            return 401, {}
        if path == "/v1/managed/devices/heartbeat" and body.get("reported_build") == "native-runtime-fixture":
            self.heartbeats += 1
            return 204, {}
        if path == "/v1/managed/devices/policy-receipts" and self.delivery:
            report = self.delivery
            failed = report["schema_version"] != 1
            if (body["delivery_id"] != report["delivery_id"] or body["policy_version"] != report["policy_version"]
                    or body["payload_sha256"] != report["payload_sha256"]
                    or body["result"] != ("failed" if failed else "applied")
                    or body["failed_fields"] != (["schema_version"] if failed else [])):
                return 403, {}
            frame = signed_frame(5, [
                self.org.bytes, self.target_id.bytes, uuid.UUID(report["delivery_id"]).bytes,
                report["policy_version"].to_bytes(8, "big"), bytes.fromhex(report["payload_sha256"]),
                (1).to_bytes(8, "big"), bytes([2 if failed else 1]), bytes([4 if failed else 0]),
            ])
            if decoded(body["signature"]) != self.target.sign(frame):
                return 403, {}
            self.policy_receipts.append((report["policy_version"], body["result"]))
            return 204, {}
        return 404, {}

    def session_request(self, path, authorization, body):
        if path.startswith("/v1/managed/devices/"):
            return self.device_request(path, authorization, body)
        if path.startswith("/v1/sessions/") and path.endswith("/renew"):
            route = self.routes.get(path.split("/")[3])
            if (route is None or route.session not in self.redemptions
                    or authorization != "Bearer " + bearer("drd_", 12)
                    or body.get("connection_id") != str(route.connection)
                    or body.get("session_credential") != bearer("drc_", 13)):
                return 403, {}
            previous = body.get("lease_sequence")
            if route.purpose.endswith("-supervised"):
                self.supervised_renew_attempts[route.session] = (
                    self.supervised_renew_attempts.get(route.session, 0) + 1
                )
                if route.purpose == "direct-supervised":
                    # The status endpoint remains responsive while renewal is stalled.
                    time.sleep(5)
                    return 403, {}
                if route.session in self.renewals:
                    return 503, {}
                if previous != 1:
                    return 409, {}
                self.renewals[route.session] = time.time_ns() // 1_000_000 + 30_000
                return 200, {"session_id": str(route.session), "connection_id": str(route.connection),
                             "lease_sequence": 2, "lease_until_ms": self.renewals[route.session],
                             "allowed_actions": ["view"]}
            if route.path == "relay" and self.status_reads.get(route.session, 0) >= 3 and previous == 2:
                self.revoked_renewals.add(route.session)
                return 403, {}
            if previous != 1:
                return 409, {}
            if route.session not in self.renewals:
                # The retry must retain the first request's local time bound.
                self.renewals[route.session] = time.time_ns() // 1_000_000 + 30_000
                return 503, {}
            return 200, {"session_id": str(route.session), "connection_id": str(route.connection),
                         "lease_sequence": 2, "lease_until_ms": self.renewals[route.session],
                         "allowed_actions": ["view"]}
        if path.startswith("/v1/sessions/") and path.endswith("/file-events"):
            route = self.routes.get(path.split("/")[3])
            expected_keys = {"connection_id", "lease_sequence", "event_id", "direction", "result", "size_bytes"}
            if (route is None or not route.purpose.endswith("-file") or route.session not in self.stopped
                    or authorization != "Bearer " + bearer("drc_", 13)
                    or set(body) != expected_keys or body["connection_id"] != str(route.connection)
                    or body["lease_sequence"] != 1):
                return 403, {}
            event = uuid.UUID(body["event_id"])
            records = self.file_events.setdefault(route.session, {})
            if event in records:
                if records[event] != body:
                    return 409, {}
                self.file_retries[route.session] = self.file_retries.get(route.session, 0) + 1
                return 204, {}
            records[event] = body
            # Persist the first event but lose the response, then require an exact retry.
            return (503 if len(records) == 1 else 204), {}
        if path.startswith("/v1/sessions/") and path.endswith("/stopped"):
            route = self.routes.get(path.split("/")[3])
            if (route is None or route.session not in self.redemptions
                    or authorization != "Bearer " + bearer("drc_", 13)):
                return 403, {}
            reasons = {"direct-supervised": "revoked", "relay-supervised": "lease_expired",
                       "direct-forbidden": "connection_lost", "relay-admit": "revoked"}
            expected = {"connection_id": str(route.connection),
                        "lease_sequence": 2 if route.session in self.renewals else 1,
                        "reason": reasons.get(route.purpose, "local_end")}
            if body != expected:
                return 403, {}
            self.stopped.add(route.session)
            if route.purpose.endswith("-supervised"):
                submissions = self.supervised_receipts.setdefault(route.session, [])
                submissions.append(body)
                if len(submissions) == 1:
                    # Persist the receipt, then lose the first response.
                    return 503, {}
            return 204, {}
        if path == "/v1/sessions":
            if authorization != "Bearer " + bearer("dru_", 11):
                return 401, {}
            purpose = body["purpose"]
            keys = {"book_id", "entry_id", "target_rustdesk_id", "target_device_id"} & body.keys()
            if purpose.endswith("-admit"):
                if keys != {"book_id", "entry_id"} or (
                    body["book_id"], body["entry_id"]
                ) != ("30000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000002"):
                    return 403, {}
            elif keys != {"target_rustdesk_id"} or body["target_rustdesk_id"] != TARGET:
                return 403, {}
            if body["confirmation_mode"] != "attended":
                return 403, {}
            allowed, actions = confirmed_mask(purpose) if purpose.endswith(("-audio", "-clipboard", "-file")) else (3, ["view", "input"])
            if body["requested_actions"] != actions:
                return 403, {}
            route = Route(self, "relay" if purpose.startswith("relay-") or purpose == "deny-redeem" else "direct",
                          allowed=allowed, purpose=purpose, expired=purpose == "expired")
            # The controller chooses its connection UUID on grant receipt.
            route.connection = None
            return 201, {
                "kid": "fixture-center", "organization_id": str(self.org), "user_id": str(self.user),
                "session_id": str(route.session), "grant_id": str(route.grant),
                "controller_device_id": str(self.controller_id), "target_device_id": str(self.target_id),
                "controller_key_sha256": hashlib.sha256(self.controller.public).hexdigest(),
                "target_key_sha256": hashlib.sha256(self.target.public).hexdigest(),
                "controller_rustdesk_id": "10700001", "target_rustdesk_id": TARGET,
                "policy_version": 1, "allowed_actions": actions,
                "confirmation_mode": "attended", "purpose": purpose,
                "issued_ms": route.issued, "expires_ms": route.expires, "signature": b64(route.signature),
            }
        if not path.startswith("/v1/sessions/") or not path.endswith("/redeem"):
            return None
        route = self.routes.get(path.split("/")[3])
        if route is None or authorization != "Bearer " + bearer("drd_", 12):
            return 401, {}
        result = self.check_redemption(route, body)
        self.results.append((route.purpose, result))
        if not result or route.purpose == "deny-redeem":
            return 403, {}
        if route.session in self.redemptions:
            return 409, {}
        self.redemptions[route.session] = body
        now = time.time_ns() // 1_000_000
        _, confirmed = confirmed_mask(route.purpose)
        return 200, {
            "session_id": str(route.session), "connection_id": str(route.connection),
            "path": route.path, "relay_id": RELAY_ID if route.path == "relay" else "",
            "allowed_actions": confirmed, "lease_sequence": 1,
            "redeemed_ms": now, "lease_until_ms": now + 30_000, "session_credential": bearer("drc_", 13),
        }

    def check_redemption(self, route, body):
        if route.connection is None or body["connection_id"] != str(route.connection) or body["path"] != route.path:
            return False
        if body["grant_id"] != str(route.grant) or decoded(body["grant_signature"]) != route.signature:
            return False
        if body["relay_id"] != (RELAY_ID if route.path == "relay" else ""):
            return False
        if route.path == "relay" and (PAIR_ENDPOINT, route.session, 204) not in self.requests:
            return False
        channel = bytes.fromhex(body["channel_sha256"])
        left, right = (decoded(body[f"{side}_nonce"]) for side in ("controller", "target"))
        if len(channel) != 32 or len(left) != 32 or len(right) != 32 or left == right:
            return False
        common = [route.bound, route.session.bytes, route.connection.bytes, channel, left, right,
                  bytes([2 if route.path == "relay" else 1])]
        if decoded(body["controller_signature"]) != self.controller.sign(signed_frame(4, [b"\x01", *common])):
            return False
        if decoded(body["target_signature"]) != self.target.sign(signed_frame(4, [b"\x02", *common])):
            return False
        mask, confirmed = confirmed_mask(route.purpose)
        consent = signed_frame(6, [route.bound, route.session.bytes, route.connection.bytes, channel, bytes([mask])])
        return body["confirmed_actions"] == confirmed and decoded(body["consent_signature"]) == self.target.sign(consent)


def main():
    servers, endpoint = Path(sys.argv[1]), Path(sys.argv[2])
    with tempfile.TemporaryDirectory(prefix="deskrealm-endpoint-") as work:
        fixture = SessionFixture(Path(work))
        try:
            with fixture.center_server() as (port, ca), services(servers, fixture, port, ca) as (hbbs, hbbr):
                env = dict(os.environ, DESKREALM_FIXTURE_CENTER=f"https://{LISTEN}:{port}",
                           DESKREALM_FIXTURE_CA=str(ca), DESKREALM_FIXTURE_HBBS=f"{LISTEN}:{hbbs}",
                           DESKREALM_FIXTURE_HBBR=f"{LISTEN}:{hbbr}",
                           DESKREALM_FIXTURE_CONTROLLER=str(fixture.controller_id),
                           DESKREALM_FIXTURE_TARGET=str(fixture.target_id),
                           DESKREALM_FIXTURE_ORGANIZATION=str(fixture.org))
                for test in ("managed_device_policy_api_roundtrip", "managed_endpoint_admission_on_direct_and_relay"):
                    result = subprocess.run([endpoint, "--exact", test, "--nocapture"], check=False, timeout=180,
                                            env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
                    print(result.stdout, end="", flush=True)
                    result.check_returncode()
                    if not all(marker in result.stdout for marker in (
                            f"test {test} ... ok", "test result: ok. 1 passed; 0 failed; 0 ignored")):
                        raise RuntimeError(f"required native endpoint test did not pass: {test}")
                assert fixture.heartbeats == 1
                assert fixture.policy_receipts == [(1, "applied"), (2, "failed"), (5, "applied")]
                assert fixture.results == [("direct-admit", True), ("relay-admit", True),
                                           ("deny-redeem", True), ("direct-clean", True),
                                           ("direct-forbidden", True), ("direct-input", True),
                                           ("relay-input", True), ("direct-audio", True),
                                           ("relay-audio", True), ("direct-clipboard", True),
                                           ("relay-clipboard", True), ("direct-file", True),
                                           ("relay-file", True), ("direct-supervised", True),
                                           ("relay-supervised", True)]
                assert len(fixture.redemptions) == 14 and len(fixture.renewals) == 3
                assert fixture.stopped == fixture.redemptions.keys()
                file_sessions = {route.session for route in fixture.routes.values() if route.purpose.endswith("-file")}
                assert fixture.file_events.keys() == file_sessions
                assert fixture.file_retries == dict.fromkeys(file_sessions, 1)
                for records in fixture.file_events.values():
                    assert len(records) == 3
                    assert {(event["direction"], event["result"], event["size_bytes"])
                            for event in records.values()} == {
                                ("upload", "completed", 14), ("download", "failed", 0),
                                ("download", "cancelled", 4),
                            }
                assert fixture.revoked_renewals == {
                    route.session for route in fixture.routes.values() if route.purpose == "relay-admit"
                }
                supervised = {route.purpose: route.session for route in fixture.routes.values()
                              if route.purpose.endswith("-supervised")}
                direct, relay = supervised["direct-supervised"], supervised["relay-supervised"]
                assert fixture.supervised_renew_attempts[direct] == 1
                assert fixture.supervised_renew_attempts[relay] >= 2
                assert set(fixture.supervised_receipts) == {direct, relay}
                assert all(len(submissions) == 2 and submissions[0] == submissions[1]
                           for submissions in fixture.supervised_receipts.values())
                for route in fixture.routes.values():
                    if route.session in fixture.stopped:
                        print(json.dumps({"scenario": route.purpose, "path": route.path,
                                          "session_id": str(route.session), "connection_id": str(route.connection),
                                          "relay_id": RELAY_ID if route.path == "relay" else None,
                                          "stop_received": True}, sort_keys=True), flush=True)
        except Exception:
            for file in sorted(fixture.directory.glob("*/output.log")):
                print(f"{file.parent.name}:\n{file.read_text()}", file=sys.stderr)
            raise
    print("Native managed endpoints checked admission, supervised shutdown and receipts on direct and relayed RustDesk TCP.", flush=True)


if __name__ == "__main__":
    main()
