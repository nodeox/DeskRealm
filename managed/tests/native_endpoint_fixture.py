"""Exercise RustDesk's native managed base crate against running hbbs/hbbr."""

import base64
from datetime import datetime, timedelta, timezone
import hashlib
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


class SessionFixture(Fixture):
    def __init__(self, path):
        super().__init__(path)
        self.redemptions = {}
        self.results = []
        self.stopped = set()
        self.device_challenge = None
        self.device_authenticated = False
        self.heartbeats = 0
        self.policy_gets = 0
        self.policy_receipts = []
        self.delivery = None

    def policy_request(self, path, authorization):
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
        if path.startswith("/v1/sessions/") and path.endswith("/stopped"):
            route = self.routes.get(path.split("/")[3])
            if (route is None or route.session not in self.redemptions
                    or authorization != "Bearer " + bearer("drc_", 13)
                    or body != {"connection_id": str(route.connection), "lease_sequence": 1, "reason": "local_end"}):
                return 403, {}
            self.stopped.add(route.session)
            return 204, {}
        if path == "/v1/sessions":
            if authorization != "Bearer " + bearer("dru_", 11):
                return 401, {}
            if body["target_rustdesk_id"] != TARGET or body["confirmation_mode"] != "attended":
                return 403, {}
            if body["requested_actions"] != ["view", "input"]:
                return 403, {}
            purpose = body["purpose"]
            route = Route(self, "relay" if purpose in ("relay-admit", "deny-redeem") else "direct",
                          allowed=3, purpose=purpose, expired=purpose == "expired")
            # The controller chooses its connection UUID on grant receipt.
            route.connection = None
            return 201, {
                "kid": "fixture-center", "organization_id": str(self.org), "user_id": str(self.user),
                "session_id": str(route.session), "grant_id": str(route.grant),
                "controller_device_id": str(self.controller_id), "target_device_id": str(self.target_id),
                "controller_key_sha256": hashlib.sha256(self.controller.public).hexdigest(),
                "target_key_sha256": hashlib.sha256(self.target.public).hexdigest(),
                "controller_rustdesk_id": "10700001", "target_rustdesk_id": TARGET,
                "policy_version": 1, "allowed_actions": ["view", "input"],
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
        return 200, {
            "session_id": str(route.session), "connection_id": str(route.connection),
            "path": route.path, "relay_id": RELAY_ID if route.path == "relay" else "",
            "allowed_actions": ["view"], "lease_sequence": 1,
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
        consent = signed_frame(6, [route.bound, route.session.bytes, route.connection.bytes, channel, b"\x01"])
        return body["confirmed_actions"] == ["view"] and decoded(body["consent_signature"]) == self.target.sign(consent)


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
                    subprocess.run([endpoint, "--exact", test, "--nocapture"], check=True, timeout=180, env=env)
                assert fixture.heartbeats == 1
                assert fixture.policy_receipts == [(1, "applied"), (2, "failed"), (5, "applied")]
                assert fixture.results == [("direct-admit", True), ("relay-admit", True), ("deny-redeem", True)]
                assert len(fixture.redemptions) == 2 and fixture.stopped == fixture.redemptions.keys()
        except Exception:
            for file in sorted(fixture.directory.glob("*/output.log")):
                print(f"{file.parent.name}:\n{file.read_text()}", file=sys.stderr)
            raise
    print("Native managed endpoints redeemed real direct and relayed RustDesk TCP sessions.", flush=True)


if __name__ == "__main__":
    main()
