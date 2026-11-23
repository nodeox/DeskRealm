"""Exercise RustDesk's native managed base crate against running hbbs/hbbr."""

import base64
import hashlib
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

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

    def session_request(self, path, authorization, body):
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
                           DESKREALM_FIXTURE_TARGET=str(fixture.target_id))
                subprocess.run(
                    [endpoint, "--exact", "managed_endpoint_admission_on_direct_and_relay", "--nocapture"],
                    check=True, timeout=180, env=env,
                )
                assert fixture.results == [("direct-admit", True), ("relay-admit", True), ("deny-redeem", True)]
                assert len(fixture.redemptions) == 2
        except Exception:
            for file in sorted(fixture.directory.glob("*/output.log")):
                print(f"{file.parent.name}:\n{file.read_text()}", file=sys.stderr)
            raise
    print("Native managed endpoints redeemed real direct and relayed RustDesk TCP sessions.", flush=True)


if __name__ == "__main__":
    main()
