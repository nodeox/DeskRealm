package m1

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"deskrealm/internal/relayprotocol"
)

const relayAdminURL = "/v1/admin/relays"

type lifecycleRelayFixture struct {
	relayCredential
	key      ed25519.PrivateKey
	endpoint string
}

func localRelayProbe(t *testing.T, key ed25519.PrivateKey) (string, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	listener := relayFixtureListener(t)
	var ready atomic.Bool
	var load atomic.Int32
	ready.Store(true)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				frame := make([]byte, len("DeskRealm/relay/v1/probe\x00")+32)
				if _, err := io.ReadFull(conn, frame); err != nil || !ready.Load() {
					return
				}
				var body [68]byte
				copy(body[:64], "1.1.16")
				binary.BigEndian.PutUint32(body[64:], uint32(load.Load()))
				_, _ = conn.Write(append(body[:], ed25519.Sign(key, append(frame, body[:]...))...))
			}()
		}
	}()
	return listener.Addr().String(), &ready, &load
}

func registerLifecycleRelay(t *testing.T, h http.Handler, admin *http.Cookie, name, region string) (lifecycleRelayFixture, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	pub, key := newRelayKeys(t)
	endpoint, ready, load := localRelayProbe(t, key)
	msg := request(h, "POST", relayAdminURL, testOrigin,
		relayRequest{name, "site-" + name, region, endpoint, "1.1.16", 3}, admin, csrfToken(admin.Value))
	checkCode(t, msg, 201, relayEnrollmentPrefix)
	record := responseValue[relayCredential](t, msg.Body.Bytes())
	input := map[string]string{"node_id": record.NodeID, "public_key": pub}
	msg = relayNodeRequest(h, "/v1/relays/enrollment/challenge", record.Credential, input)
	checkCode(t, msg, 200, record.NodeID)
	proof := responseValue[relayprotocol.Enrollment](t, msg.Body.Bytes())
	input["signature"] = signRelay(key, proof.Frame())
	checkCode(t, relayNodeRequest(h, "/v1/relays/enrollment/complete", record.Credential, input), 200, record.NodeID)
	return lifecycleRelayFixture{record, key, endpoint}, ready, load
}

func (r lifecycleRelayFixture) health(t *testing.T, h http.Handler, version, seq int64, count int, code int) {
	t.Helper()
	payload := relayprotocol.Health{NodeID: r.NodeID, ConfigVersion: version, Sequence: seq, ObservedMS: time.Now().UnixMilli(),
		Build: "1.1.16", ProtocolReady: true, ActiveSessions: count}
	msg := relayNodeRequest(h, "/v1/relays/health", "", struct {
		relayprotocol.Health
		Signature string `json:"signature"`
	}{payload, signRelay(r.key, payload.Frame())})
	checkCode(t, msg, code, "")
}

func relaySweep(t *testing.T, ctx context.Context, store Store) {
	t.Helper()
	taskSQL(t, ctx, store, "UPDATE relay_nodes SET probe_checked_at=NULL")
	if err := store.probeDueRelays(ctx); err != nil {
		t.Fatal(err)
	}
}

func regionSessionFixture(t *testing.T, f sessionFixture, relay lifecycleRelayFixture) issuedLease {
	t.Helper()
	changeTestRule(t, f.h, f.admin, f.rule, "publish")
	f.applyPolicy(t, []string{"view"}, []string{"view"})
	input := f.input(71)
	input.Actions = []string{"view"}
	g := f.issue(t, input)
	conn := testSessionUUID(710)
	receipt := sessionReceiptPrefix + fixtureToken(t)
	f.sql(t, `INSERT INTO managed_session_routes(session_id,connection_id,path,relay_id,paired_at)
		VALUES($1,$2,'relay',$3,statement_timestamp())`, g.ID, conn, relay.NodeID)
	f.sql(t, `INSERT INTO managed_session_redemptions(session_id,connection_id,path,relay_id,proof_hash,allowed_mask,redeemed_at,lease_until,receipt_digest)
		VALUES($1,$2,'relay',$3,decode(repeat('00',32),'hex'),1,statement_timestamp(),statement_timestamp()+interval '30 seconds',$4)`,
		g.ID, conn, relay.NodeID, managedDigest(receipt, sessionReceiptPrefix))
	return issuedLease{g.ID, sessionsURL + "/" + g.ID, conn, receipt}
}

func TestRegionalRelayLifecyclePostgres(t *testing.T) {
	f := newSessionFixture(t)
	a, ready, _ := registerLifecycleRelay(t, f.h, f.admin, "alpha", "north")
	b, _, _ := registerLifecycleRelay(t, f.h, f.admin, "beta", "south")
	a.health(t, f.h, 1, 1, 1, 204)
	b.health(t, f.h, 1, 1, 0, 204)
	relaySweep(t, f.ctx, f.store)
	session := regionSessionFixture(t, f, a)
	type nodeView struct {
		relayNodeSummary
		Sessions []relaySession
		Probes   []relayProbeHistory
	}
	detail := func(state string) nodeView {
		t.Helper()
		msg := request(f.h, "GET", relayAdminURL+"/"+a.NodeID, "", nil, f.admin, "")
		checkCode(t, msg, 200, a.NodeID)
		view := responseValue[nodeView](t, msg.Body.Bytes())
		if view.State != state {
			t.Fatalf("expected %s, got %+v", state, view.relayNodeSummary)
		}
		return view
	}
	change := func(cookie *http.Cookie, rev int64, action, endpoint string) *httptest.ResponseRecorder {
		return request(f.h, "POST", relayAdminURL+"/"+a.NodeID+"/lifecycle", testOrigin,
			relayChange{action, rev, endpoint}, cookie, csrfToken(cookie.Value))
	}
	operation := func(rev int64, action, endpoint, want string) string {
		t.Helper()
		msg := change(f.admin, rev, action, endpoint)
		checkCode(t, msg, 200, want)
		id := responseValue[struct {
			TaskID string `json:"task_id"`
		}](t, msg.Body.Bytes()).TaskID
		if id == "" {
			t.Fatal("no task for relay operation")
		}
		checkCode(t, request(f.h, "GET", tasksPath+"/"+id, "", nil, f.admin, ""), 200, want)
		return id
	}
	overview := func(want string) {
		t.Helper()
		checkCode(t, request(f.h, "GET", "/v1/overview", "", nil, f.admin, ""), 200, `"available_relays":`+want)
		msg := request(f.h, "GET", "/v1/overview", "", nil, f.user, "")
		checkCode(t, msg, 200, "")
		if bytes.Contains(msg.Body.Bytes(), []byte("available_relays")) {
			t.Fatal("member received relay metric")
		}
	}
	detail("available")
	overview("2")
	checkCode(t, change(f.user, 1, "drain", ""), 403, "forbidden")
	checkCode(t, request(f.h, "GET", relayAdminURL+"/"+a.NodeID, "", nil, f.user, ""), 403, "forbidden")
	checkCode(t, request(f.h, "POST", relayAdminURL+"/"+a.NodeID+"/lifecycle", testOrigin, relayChange{"drain", 1, ""}, f.admin, ""), 403, "csrf_denied")
	drained := operation(1, "drain", "", "relay_draining")
	view := detail("draining")
	if len(view.Sessions) != 1 || view.Sessions[0].ID != session.session || view.Sessions[0].State != "active" || view.OperationTask != drained {
		t.Fatalf("drain did not preserve the active session and operation: %+v", view)
	}
	checkCode(t, session.renew(f.h, f.targetToken, 1), 200, `"lease_sequence":2`)
	checkCode(t, change(f.admin, 1, "disable", ""), 409, errRelayConflict.Error())
	overview("1")
	operation(2, "disable", "", "relay_disabled")
	detail("disabled")
	ready.Store(false)
	operation(3, "resume", "", "relay_probe_failed")
	detail("disabled")
	ready.Store(true)
	f.sql(t, "UPDATE relay_nodes SET health_at=statement_timestamp()-interval '16 seconds' WHERE id=$1", a.NodeID)
	operation(4, "resume", "", "relay_health_required")
	a.health(t, f.h, 1, 2, 1, 204)
	operation(5, "resume", "", "relay_resumed")
	detail("available")
	checkCode(t, change(f.admin, 6, "change_endpoint", b.endpoint), 409, errRelayEndpointConflict.Error())
	wrong, _, _ := localRelayProbe(t, b.key)
	operation(6, "change_endpoint", wrong, "relay_probe_failed")
	if old := detail("available"); old.Endpoint != a.endpoint || old.Version != 1 || old.Probes[0].Result != "protocol_mismatch" {
		t.Fatalf("failed probe changed the active endpoint or lacked evidence: %+v", old)
	}
	valid, validReady, validLoad := localRelayProbe(t, a.key)
	operation(7, "change_endpoint", valid, "relay_endpoint_changed")
	if changed := detail("awaiting_health"); changed.Endpoint != valid || changed.Version != 2 {
		t.Fatalf("did not switch to verified endpoint: %+v", changed)
	}
	a.health(t, f.h, 1, 3, 1, 409)
	a.health(t, f.h, 2, 4, 1, 204)
	detail("available")
	overview("2")
	validLoad.Store(3)
	relaySweep(t, f.ctx, f.store)
	detail("at_capacity")
	overview("1")
	validLoad.Store(0)
	relaySweep(t, f.ctx, f.store)
	detail("available")
	validReady.Store(false)
	for _, state := range []string{"available", "available", "probe_unreachable"} {
		relaySweep(t, f.ctx, f.store)
		detail(state)
	}
	a.health(t, f.h, 2, 5, 1, 204)
	validReady.Store(true)
	relaySweep(t, f.ctx, f.store)
	detail("available")
	f.count(t, 1, "SELECT count(*) FROM audit_events WHERE resource_id=$1 AND action='relay.probe_lost'", a.NodeID)
	f.count(t, 1, "SELECT count(*) FROM audit_events WHERE resource_id=$1 AND action='relay.probe_recovered'", a.NodeID)
	results := make(chan int, 6)
	for range cap(results) {
		go func() { results <- change(f.admin, 8, "drain", "").Code }()
	}
	applied := 0
	for range cap(results) {
		switch code := <-results; code {
		case 200:
			applied++
		case 409:
		default:
			t.Fatalf("unexpected concurrent drain status %d", code)
		}
	}
	if applied != 1 {
		t.Fatalf("expected one drain for the lifecycle revision, got %d", applied)
	}
	f.count(t, 8, "SELECT count(*) FROM audit_events WHERE resource_id=$1 AND source='relay_lifecycle' AND task_id IS NOT NULL", a.NodeID)
	f.count(t, 1, "SELECT count(*) FROM managed_session_redemptions WHERE session_id=$1 AND relay_id=$2 AND lease_sequence=2", session.session, a.NodeID)
}

func TestRegionalRelayBrowserPostgres(t *testing.T) {
	if os.Getenv("DESKREALM_RELAY_BROWSER") != "1" {
		t.Skip("run scripts/deskrealm-m3-test relay-lifecycle")
	}
	f := newSessionFixture(t)
	a, _, _ := registerLifecycleRelay(t, f.h, f.admin, "alpha", "north")
	b, _, _ := registerLifecycleRelay(t, f.h, f.admin, "beta", "south")
	full, _, _ := registerLifecycleRelay(t, f.h, f.admin, "full", "south")
	faulty, faultReady, _ := registerLifecycleRelay(t, f.h, f.admin, "faulty", "south")
	expired, _, _ := registerLifecycleRelay(t, f.h, f.admin, "expired", "south")
	a.health(t, f.h, 1, 1, 1, 204)
	b.health(t, f.h, 1, 1, 0, 204)
	full.health(t, f.h, 1, 1, 3, 204)
	faulty.health(t, f.h, 1, 1, 0, 204)
	expired.health(t, f.h, 1, 1, 0, 204)
	f.sql(t, "UPDATE relay_nodes SET health_at=statement_timestamp()-interval '16 seconds' WHERE id=$1", expired.NodeID)
	faultReady.Store(false)
	for range 3 {
		relaySweep(t, f.ctx, f.store)
	}
	session := regionSessionFixture(t, f, a)
	wrong, _, _ := localRelayProbe(t, b.key)
	good, _, _ := localRelayProbe(t, a.key)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	go f.store.RunRelayProbes(ctx)
	// Keep the fixture's signed reports current while the browser navigates.
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for sequence := int64(2); ; sequence++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				for _, relay := range []lifecycleRelayFixture{a, b, full, faulty} {
					var version int64
					err := f.store.DB.QueryRow(ctx, "SELECT config_version FROM relay_nodes WHERE id=$1", relay.NodeID).Scan(&version)
					if err != nil {
						return
					}
					payload := relayprotocol.Health{NodeID: relay.NodeID, ConfigVersion: version, Sequence: sequence,
						ObservedMS: time.Now().UnixMilli(), Build: "1.1.16", ProtocolReady: true}
					if relay.NodeID == full.NodeID {
						payload.ActiveSessions = 3
					}
					if relay.NodeID == a.NodeID {
						payload.ActiveSessions = 1
					}
					relayNodeRequest(f.h, "/v1/relays/health", "", struct {
						relayprotocol.Health
						Signature string `json:"signature"`
					}{payload, signRelay(relay.key, payload.Frame())})
				}
			}
		}
	}()
	root := filepath.Join("..", "..", "web")
	server := httptest.NewUnstartedServer(nil)
	origin, err := ParseOrigin("http://"+server.Listener.Addr().String(), true)
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = NewHandler(f.store, origin, os.DirFS(filepath.Join(root, "dist")))
	server.Start()
	defer server.Close()
	scratch := t.TempDir()
	report := filepath.Join(scratch, "browser.json")
	cmd := exec.CommandContext(f.ctx, "npx", "--no-install", "playwright", "test", "tests/relay-lifecycle.spec.ts")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "DESKREALM_BROWSER_BASE="+server.URL, "DESKREALM_BROWSER_OUTPUT="+scratch,
		"DESKREALM_BROWSER_REPORT="+report, "DESKREALM_BROWSER_RELAY_NODE="+a.NodeID,
		"DESKREALM_BROWSER_RELAY_BAD="+wrong, "DESKREALM_BROWSER_RELAY_GOOD="+good,
		"DESKREALM_BROWSER_RELAY_OCCUPIED="+b.endpoint, "DESKREALM_BROWSER_RELAY_SESSION="+session.session)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Chromium relay lifecycle: %v\n%s", err, output)
	}
	reportData, err := os.ReadFile(report)
	if err != nil || !bytes.Contains(reportData, []byte(`"expected": 1`)) || !bytes.Contains(reportData, []byte(`"skipped": 0`)) {
		t.Fatalf("missing passing Chromium relay test: %v", err)
	}
	f.count(t, 1, "SELECT count(*) FROM relay_nodes WHERE id=$1 AND endpoint=$2 AND config_version=2 AND mode='enabled'", a.NodeID, good)
	f.count(t, 5, "SELECT count(*) FROM audit_events WHERE resource_id=$1 AND source='relay_lifecycle'", a.NodeID)
	t.Logf("%s", output)
}
