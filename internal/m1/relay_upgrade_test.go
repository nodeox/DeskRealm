package m1

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"deskrealm/internal/relayprotocol"
)

func TestRegionalRelayUpgradePostgres(t *testing.T) {
	f := newSessionFixture(t)
	first, ready, load := registerLifecycleRelay(t, f.h, f.admin, "upgrade", "north")
	second, _, _ := registerLifecycleRelay(t, f.h, f.admin, "other", "south")
	first.health(t, f.h, 1, 1, 1, 204)
	second.health(t, f.h, 1, 1, 0, 204)
	relaySweep(t, f.ctx, f.store)
	session := regionSessionFixture(t, f, first)
	target, original := strings.Repeat("a", 64), strings.Repeat("b", 64)
	url := relayAdminURL + "/" + first.NodeID
	admin := func(cookie *http.Cookie, action string, rev int64, digest string) *httptest.ResponseRecorder {
		return request(f.h, "POST", url+"/upgrade", testOrigin,
			map[string]any{"action": action, "revision": rev, "sha256": digest}, cookie, csrfToken(cookie.Value))
	}
	node := func(key ed25519.PrivateKey, ticket string, action, digest string) *httptest.ResponseRecorder {
		t.Helper()
		nonce, err := token()
		if err != nil {
			t.Fatal(err)
		}
		r := relayprotocol.UpgradeRequest{NodeID: first.NodeID, TaskID: ticket, Nonce: nonce,
			ObservedMS: time.Now().UnixMilli(), Action: action, ConfigVersion: 1, SHA256: digest}
		return relayNodeRequest(f.h, "/v1/relays/upgrade", "", struct {
			relayprotocol.UpgradeRequest
			Signature string `json:"signature"`
		}{r, signRelay(key, r.Frame())})
	}
	checkCode(t, admin(f.user, "start", 1, target), 403, "forbidden")
	created := admin(f.admin, "start", 1, target)
	checkCode(t, created, 200, "pending")
	ticket := responseValue[relayprotocol.Upgrade](t, created.Body.Bytes()).TaskID
	checkCode(t, admin(f.admin, "start", 2, target), 409, errRelayConflict.Error())
	transition := func(action string, rev int64) *httptest.ResponseRecorder {
		return request(f.h, "POST", url+"/lifecycle", testOrigin, relayChange{action, rev, ""}, f.admin, csrfToken(f.admin.Value))
	}
	checkCode(t, transition("resume", 2), 409, errRelayConflict.Error())
	checkCode(t, transition("disable", 2), 409, errRelayConflict.Error())
	checkCode(t, session.renew(f.h, f.targetToken, 1), 200, `"lease_sequence":2`)
	checkCode(t, node(second.key, ticket, "status", target), 401, "")
	checkCode(t, node(first.key, ticket, "status", original), 409, "")
	checkCode(t, node(first.key, ticket, "claim", original), 409, "")
	f.sql(t, "UPDATE relay_upgrades SET ready_at=clock_timestamp() WHERE task_id=$1", ticket)
	checkCode(t, node(first.key, ticket, "claim", original), 409, "") // Zero forwarded, but the lease remains live.
	checkCode(t, session.stop(f.h, 2, "local_end"), 204, "")
	load.Store(1)
	checkCode(t, node(first.key, ticket, "claim", original), 409, "")
	load.Store(0)
	checkCode(t, node(first.key, ticket, "claim", original), 200, "claimed")
	checkCode(t, node(first.key, ticket, "claim", original), 200, "claimed")
	checkCode(t, admin(f.admin, "cancel", 2, ""), 409, "")
	checkCode(t, node(first.key, ticket, "succeeded", target), 409, "") // No health since claim.
	first.health(t, f.h, 1, 2, 0, 204)
	ready.Store(false)
	checkCode(t, node(first.key, ticket, "rolled_back", original), 409, "")
	ready.Store(true)
	checkCode(t, node(first.key, ticket, "rolled_back", original), 200, "rolled_back")
	checkCode(t, node(first.key, ticket, "rolled_back", original), 200, "rolled_back")
	checkCode(t, request(f.h, "GET", tasksPath+"/"+ticket, "", nil, f.admin, ""), 200, "relay_upgrade_rolled_back")
	f.count(t, 1, "SELECT count(*) FROM audit_events WHERE task_id=$1 AND action='relay.upgrade_rolled_back'", ticket)
	checkCode(t, transition("resume", 3), 200, "relay_resumed")
	created = admin(f.admin, "start", 4, target)
	checkCode(t, created, 200, "pending")
	ticket = responseValue[relayprotocol.Upgrade](t, created.Body.Bytes()).TaskID
	checkCode(t, admin(f.admin, "cancel", 5, ""), 200, "canceled")
	checkCode(t, node(first.key, ticket, "status", target), 200, "canceled")
	checkCode(t, node(first.key, ticket, "claim", original), 409, "")
}
