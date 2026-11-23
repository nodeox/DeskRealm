package m1

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"deskrealm/internal/managedproto"
)

const (
	sessionsURL         = "/v1/sessions"
	sessionAuthorizeURL = "/v1/internal/managed/rendezvous/authorize"
	sessionPairedURL    = "/v1/internal/managed/relay/paired"
	sessionTestRelay    = "fixture-relay"
)

type sessionFixture struct {
	ctx                                     context.Context
	store                                   Store
	h                                       http.Handler
	admin, user                             *http.Cookie
	signing                                 *ManagedSigningKey
	rule                                    connectionRule
	controller, target, userID              string
	controllerToken, targetToken, userToken string
	controllerKey, targetKey                ed25519.PrivateKey
	rendezvous, relay                       string
}

func fixtureToken(t *testing.T) string {
	t.Helper()
	value, err := token()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func newSessionFixture(t *testing.T) sessionFixture {
	t.Helper()
	ctx, store, _ := postgres(t)
	_, admin, csrf, _ := adminSession(t, ctx, store)
	_, signing := managedTestKey(t)
	rendezvous, relay := fixtureToken(t), fixtureToken(t)
	transport, err := ConfigureManagedTransport(rendezvous, sessionTestRelay, relay)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := ParseOrigin(testOrigin, false)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(store, origin, nil, AuthConfig{ManagedKey: signing, Transport: transport})
	member := createMember(t, h, admin, csrf)
	login := loginRequest(h, member.Username, testPassword)
	checkCode(t, login, 204, "")
	user := login.Result().Cookies()[0]
	controller, controllerKey := runtimeFixture(t, h, admin, csrf, "10700001", true)
	target, targetKey := runtimeFixture(t, h, admin, csrf, "10700002", true)
	controllerToken := runtimeCredentialFixture(t, h, controller, controllerKey)
	targetToken := runtimeCredentialFixture(t, h, target, targetKey)
	for id, credential := range map[string]string{controller: controllerToken, target: targetToken} {
		checkCode(t, nativeRequest(h, "POST", deviceHeartbeatURL, credential,
			map[string]string{"device_id": id, "reported_build": "1.5.3-fixture"}), 204, "")
	}
	binding := request(h, "POST", userChallengeURL, testOrigin, map[string]string{
		"device_id": controller, "device_credential": controllerToken,
	}, user, csrfToken(user.Value))
	checkCode(t, binding, 200, member.ID)
	proof := signManagedFixture(t, responseValue[identityChallenge](t, binding.Body.Bytes()), controllerKey)
	login = request(h, "POST", userCompleteURL, testOrigin, proof, user, csrfToken(user.Value))
	checkCode(t, login, 201, managedUserPrefix)
	userToken := responseValue[managedCredential](t, login.Body.Bytes()).Value
	role := createTestControlRole(t, h, admin, "view", "input", "clipboard", "file_transfer")
	rule := makeRule(t, h, admin, connectionRuleSpec{Name: "会话夹具", SourceUser: member.ID, Controller: controller,
		Target: target, Purpose: "helpdesk", Effect: "allow", Role: role.ID, RoleVersion: role.Version, Actions: role.Actions})
	return sessionFixture{ctx: ctx, store: store, h: h, admin: admin, user: user, signing: signing, rule: rule,
		controller: controller, target: target, userID: member.ID, controllerToken: controllerToken, targetToken: targetToken,
		userToken: userToken, controllerKey: controllerKey, targetKey: targetKey,
		rendezvous: rendezvousPrefix + rendezvous, relay: relayPrefix + relay}
}

func testSessionUUID(n int) string {
	return fmt.Sprintf("10000000-7000-4000-8000-%012x", n)
}

func (f sessionFixture) applyPolicy(t *testing.T, actions, unattended []string) int64 {
	t.Helper()
	published := policyFixture(t, f.h, f.admin, "会话策略", actions, unattended)
	assignmentFixture(t, f.h, f.admin, "device", f.target, published.Version)
	p := runtimePolicyFixture(t, f.h, f.target, f.targetToken)
	if p == nil || p.Version != published.Version {
		t.Fatal("target did not receive its policy")
	}
	checkCode(t, nativeRequest(f.h, "POST", receiptsURL, f.targetToken, signedPolicyFixture(t, p, f.targetKey, "applied")), 204, "")
	return published.Version
}

func (f sessionFixture) input(n int) sessionIssueRequest {
	return sessionIssueRequest{RequestID: testSessionUUID(n), Target: f.target, Purpose: "helpdesk",
		Actions: []string{"input", "view", "clipboard"}}
}

func (f sessionFixture) issue(t *testing.T, in sessionIssueRequest) sessionGrant {
	t.Helper()
	r := nativeRequest(f.h, "POST", sessionsURL, f.userToken, in)
	checkCode(t, r, 201, "")
	value := responseValue[struct {
		ID            string `json:"session_id"`
		Grant         string `json:"grant_id"`
		Signature     string `json:"signature"`
		Issued        int64  `json:"issued_ms"`
		Expires       int64  `json:"expires_ms"`
		ControllerKey string `json:"controller_key_sha256"`
	}](t, r.Body.Bytes())
	g := f.readGrant(t, value.ID)
	pub := f.controllerKey.Public().(ed25519.PublicKey)
	hash := sha256.Sum256(pub)
	frame, err := g.signingFrame()
	if err != nil || !ed25519.Verify(f.signing.key.Public().(ed25519.PublicKey), frame, g.Signature) ||
		g.Grant != value.Grant || g.Issued.UnixMilli() != value.Issued || g.Expires.UnixMilli() != value.Expires ||
		value.Expires-value.Issued != 60_000 || value.ControllerKey != hex.EncodeToString(hash[:]) ||
		value.Signature != base64.RawURLEncoding.EncodeToString(g.Signature) {
		t.Fatal("response grant does not match the persisted, valid signature")
	}
	return g
}

func (f sessionFixture) readGrant(t *testing.T, id string) sessionGrant {
	t.Helper()
	g, err := readSessionGrant(f.store.DB.QueryRow(f.ctx, sessionGrantSelect+"WHERE id=$1", id))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func (f sessionFixture) sql(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.store.DB.Exec(f.ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f sessionFixture) count(t *testing.T, want int, query string, args ...any) {
	t.Helper()
	var count int
	if err := f.store.DB.QueryRow(f.ctx, query, args...).Scan(&count); err != nil || count != want {
		t.Fatalf("expected %d rows, got %d: %v", want, count, err)
	}
}

func (f sessionFixture) auditCount(t *testing.T, session, action string, want int) {
	t.Helper()
	f.count(t, want, "SELECT count(*) FROM audit_events e JOIN outbox o ON e.id=o.event_id WHERE e.session_id=$1 AND e.action=$2", session, action)
}

func testSessionRoute(g sessionGrant, conn int, path string) sessionRouteInput {
	in := sessionRouteInput{Session: g.ID, Grant: g.Grant, Signature: base64.RawURLEncoding.EncodeToString(g.Signature),
		Controller: g.Controller, Target: g.Target, Connection: testSessionUUID(conn), Path: path}
	if path == "relay" {
		in.Relay = sessionTestRelay
	}
	return in
}

func (f sessionFixture) redeemProof(t *testing.T, g sessionGrant, route sessionRouteInput, actions ...string) sessionRedeemInput {
	t.Helper()
	grant, err := g.signingFrame()
	if err != nil {
		t.Fatal(err)
	}
	bound := sha256.Sum256(append(grant, g.Signature...))
	encode := func(k managedproto.Kind, fields ...[]byte) []byte {
		t.Helper()
		frame, err := managedproto.Encode(k, fields...)
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	sign := func(key ed25519.PrivateKey, frame []byte) string {
		return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, frame))
	}
	controllerNonce, targetNonce := fixtureToken(t), fixtureToken(t)
	c, _ := base64.RawURLEncoding.DecodeString(controllerNonce)
	d, _ := base64.RawURLEncoding.DecodeString(targetNonce)
	channel := sha256.Sum256([]byte("fixture-channel:" + route.Connection))
	sid, cid := managedFrameUUID(route.Session), managedFrameUUID(route.Connection)
	path := byte(1)
	if route.Path == "relay" {
		path = 2
	}
	out := sessionRedeemInput{Grant: route.Grant, GrantSignature: route.Signature, Connection: route.Connection,
		Path: route.Path, Relay: route.Relay, Channel: hex.EncodeToString(channel[:]), ControllerNonce: controllerNonce, TargetNonce: targetNonce,
		ControllerSignature: sign(f.controllerKey, encode(managedproto.ConnectionProof, []byte{1}, bound[:], sid, cid, channel[:], c, d, []byte{path})),
		TargetSignature:     sign(f.targetKey, encode(managedproto.ConnectionProof, []byte{2}, bound[:], sid, cid, channel[:], c, d, []byte{path})),
		ConfirmedActions:    actions}
	if len(actions) != 0 {
		out.ConsentSignature = sign(f.targetKey, encode(managedproto.SessionConsent, bound[:], sid, cid, channel[:], []byte{controlActionMask(actions)}))
	}
	return out
}

func TestSessionGrantPostgres(t *testing.T) {
	f := newSessionFixture(t)
	in := f.input(1)
	post := func(credential string, data any) *httptest.ResponseRecorder {
		return nativeRequest(f.h, "POST", sessionsURL, credential, data)
	}
	checkCode(t, post(f.targetToken, in), 401, errManagedIdentity.Error())
	checkCode(t, request(f.h, "POST", sessionsURL, testOrigin, in, f.admin, csrfToken(f.admin.Value)), 403, "")
	checkCode(t, post(f.userToken, map[string]any{"user_id": f.userID}), 400, "")
	checkCode(t, post(f.userToken, in), 403, errSessionDenied.Error())
	changeTestRule(t, f.h, f.admin, f.rule, "publish")
	checkCode(t, post(f.userToken, in), 403, errSessionDenied.Error())
	policy := f.applyPolicy(t, []string{"view", "input"}, nil)
	unattended := in
	unattended.Mode = "unattended"
	checkCode(t, post(f.userToken, unattended), 403, errSessionDenied.Error())

	f.sql(t, "ALTER TABLE audit_events ADD CONSTRAINT fixture_issue CHECK(action <> 'session.grant_issued') NOT VALID")
	checkCode(t, post(f.userToken, in), 500, "")
	f.count(t, 0, "SELECT count(*) FROM managed_sessions")
	f.sql(t, "ALTER TABLE audit_events DROP CONSTRAINT fixture_issue")

	g := f.issue(t, in)
	if g.User != f.userID || g.Controller != f.controller || g.Target != f.target || g.PolicyVersion != policy ||
		g.Mode != "attended" || g.Rule != f.rule.ID || g.RuleVersion != f.rule.Version || g.Mask != 3 {
		t.Fatalf("wrong authorization bindings or rights for session %s", g.ID)
	}
	in.Mode, in.Actions = "attended", []string{"view", "clipboard", "input"}
	checkCode(t, post(f.userToken, in), 200, g.ID)
	conflict := in
	conflict.Actions = []string{"view"}
	checkCode(t, post(f.userToken, conflict), 409, errSessionConflict.Error())
	f.auditCount(t, g.ID, "session.grant_issued", 1)

	route := testSessionRoute(g, 11, "direct")
	forged := route
	forged.Target = f.controller
	checkCode(t, nativeRequest(f.h, "POST", sessionAuthorizeURL, f.rendezvous, forged), 403, errSessionDenied.Error())
	checkCode(t, nativeRequest(f.h, "POST", sessionAuthorizeURL, f.rendezvous, route), 204, "")
	// Hold the rule row so the real archive waits after obtaining the organization lock.
	tx, err := f.store.DB.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	backendPID := tx.Conn().PgConn().PID()
	if _, err := tx.Exec(f.ctx, "SELECT id FROM connection_rules WHERE id=$1 FOR NO KEY UPDATE", f.rule.ID); err != nil {
		t.Fatal(err)
	}
	archived := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		archived <- connectionRulePost(f.h, f.admin, accessRulesURL+"/"+f.rule.ID+"/archive",
			map[string]int64{"version": f.rule.Version})
	}()
	waitForBlockedBackend := func(blocker int32) int32 {
		t.Helper()
		lockCtx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
		defer cancel()
		for {
			var blocked int32
			err := f.store.DB.QueryRow(lockCtx, `SELECT COALESCE((
				SELECT pid FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)) LIMIT 1),0)`, blocker).Scan(&blocked)
			if err != nil {
				t.Fatalf("request did not reach the lock held by %d: %v", blocker, err)
			}
			if blocked != 0 {
				return blocked
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	archivePID := waitForBlockedBackend(int32(backendPID))
	authorized := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		authorized <- nativeRequest(f.h, "POST", sessionAuthorizeURL, f.rendezvous, route)
	}()
	waitForBlockedBackend(archivePID)
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	checkCode(t, <-archived, 204, "")
	checkCode(t, <-authorized, 403, errSessionDenied.Error())
	checkCode(t, post(f.userToken, in), 403, errSessionDenied.Error())
	redeem := f.redeemProof(t, g, route, "view")
	checkCode(t, nativeRequest(f.h, "POST", sessionsURL+"/"+g.ID+"/redeem", f.targetToken, redeem), 403, errSessionDenied.Error())
	f.auditCount(t, g.ID, "session.route_denied", 1)
	f.auditCount(t, g.ID, "session.redeemed", 0)
}

func TestSessionRedemptionPostgres(t *testing.T) {
	f := newSessionFixture(t)
	changeTestRule(t, f.h, f.admin, f.rule, "publish")
	f.applyPolicy(t, []string{"view", "input"}, []string{"helpdesk"})
	g := f.issue(t, f.input(2))
	route := testSessionRoute(g, 21, "direct")
	other := testSessionRoute(g, 22, "direct")
	redeem := func(id, credential string, proof sessionRedeemInput) *httptest.ResponseRecorder {
		return nativeRequest(f.h, "POST", sessionsURL+"/"+id+"/redeem", credential, proof)
	}
	p1, p2 := f.redeemProof(t, g, route, "view"), f.redeemProof(t, g, other, "view")
	checkCode(t, redeem(g.ID, f.targetToken, p1), 403, errSessionDenied.Error())
	for _, candidate := range []sessionRouteInput{route, other} {
		checkCode(t, nativeRequest(f.h, "POST", sessionAuthorizeURL, f.rendezvous, candidate), 204, "")
	}
	checkCode(t, redeem(g.ID, f.controllerToken, p1), 401, errManagedIdentity.Error())
	checkCode(t, redeem(g.ID, f.targetToken, f.redeemProof(t, g, route)), 403, errSessionDenied.Error())
	checkCode(t, redeem(g.ID, f.targetToken, f.redeemProof(t, g, route, "clipboard")), 403, errSessionDenied.Error())
	tampered := p1
	tampered.ConsentSignature = p1.TargetSignature
	checkCode(t, redeem(g.ID, f.targetToken, tampered), 403, errSessionDenied.Error())
	tampered = p1
	tampered.ControllerSignature, tampered.TargetSignature = p1.TargetSignature, p1.ControllerSignature
	checkCode(t, redeem(g.ID, f.targetToken, tampered), 403, errSessionDenied.Error())

	f.sql(t, "ALTER TABLE audit_events ADD CONSTRAINT fixture_redeem CHECK(action <> 'session.redeemed') NOT VALID")
	checkCode(t, redeem(g.ID, f.targetToken, p1), 500, "")
	f.count(t, 0, "SELECT count(*) FROM managed_session_redemptions WHERE session_id=$1", g.ID)
	f.sql(t, "ALTER TABLE audit_events DROP CONSTRAINT fixture_redeem")

	type result struct {
		proof sessionRedeemInput
		reply *httptest.ResponseRecorder
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, proof := range []sessionRedeemInput{p1, p2} {
		go func() {
			<-start
			results <- result{proof, redeem(g.ID, f.targetToken, proof)}
		}()
	}
	close(start)
	var winner result
	for range 2 {
		r := <-results
		if r.reply.Code == 200 {
			if winner.reply != nil {
				t.Fatal("both concurrent connections redeemed the grant")
			}
			winner = r
		} else {
			checkCode(t, r.reply, 409, errSessionConflict.Error())
		}
	}
	if winner.reply == nil {
		t.Fatal("grant had no winning connection")
	}
	exact := redeem(g.ID, f.targetToken, winner.proof)
	checkCode(t, exact, 200, `"allowed_actions":["view"]`)
	if !bytes.Equal(exact.Body.Bytes(), winner.reply.Body.Bytes()) {
		t.Fatal("redemption retry changed the lease")
	}
	lease := responseValue[struct {
		Sequence int   `json:"lease_sequence"`
		Started  int64 `json:"redeemed_ms"`
		Until    int64 `json:"lease_until_ms"`
	}](t, exact.Body.Bytes())
	if lease.Sequence != 1 || lease.Until-lease.Started != 30_000 {
		t.Fatalf("invalid initial lease: %+v", lease)
	}
	loser := route
	if loser.Connection == winner.proof.Connection {
		loser = other
	}
	checkCode(t, nativeRequest(f.h, "POST", sessionAuthorizeURL, f.rendezvous, loser), 409, errSessionConflict.Error())
	winnerRoute := route
	if winnerRoute.Connection != winner.proof.Connection {
		winnerRoute = other
	}
	checkCode(t, redeem(g.ID, f.targetToken, f.redeemProof(t, g, winnerRoute, "input")), 409, errSessionConflict.Error())
	f.auditCount(t, g.ID, "session.redeemed", 1)

	unattended := f.input(3)
	unattended.Mode = "unattended"
	g = f.issue(t, unattended)
	relayRoute := testSessionRoute(g, 23, "relay")
	pr := f.redeemProof(t, g, relayRoute)
	checkCode(t, nativeRequest(f.h, "POST", sessionPairedURL, f.rendezvous, relayRoute), 401, errManagedIdentity.Error())
	checkCode(t, nativeRequest(f.h, "POST", sessionPairedURL, f.relay, relayRoute), 409, errSessionConflict.Error())
	checkCode(t, nativeRequest(f.h, "POST", sessionAuthorizeURL, f.rendezvous, relayRoute), 204, "")
	checkCode(t, redeem(g.ID, f.targetToken, pr), 403, errSessionDenied.Error())
	checkCode(t, nativeRequest(f.h, "POST", sessionPairedURL, f.relay, relayRoute), 204, "")
	checkCode(t, nativeRequest(f.h, "POST", sessionPairedURL, f.relay, relayRoute), 204, "")
	checkCode(t, redeem(g.ID, f.targetToken, pr), 200, `"path":"relay"`)
	f.auditCount(t, g.ID, "session.redeemed", 1)
	f.auditCount(t, g.ID, "session.relay_paired", 1)
	f.count(t, 1, "SELECT count(*) FROM managed_session_redemptions WHERE session_id=$1 AND path='relay' AND relay_id=$2", g.ID, sessionTestRelay)

	g = f.issue(t, f.input(4))
	g.Issued, g.Expires = g.Issued.Add(-2*time.Minute), g.Expires.Add(-2*time.Minute)
	frame, err := g.signingFrame()
	if err != nil {
		t.Fatal(err)
	}
	g.Signature = ed25519.Sign(f.signing.key, frame)
	f.sql(t, "UPDATE managed_sessions SET issued_at=$2,expires_at=$3,signature=$4 WHERE id=$1", g.ID, g.Issued, g.Expires, g.Signature)
	expired := testSessionRoute(g, 24, "direct")
	checkCode(t, nativeRequest(f.h, "POST", sessionAuthorizeURL, f.rendezvous, expired), 403, errSessionDenied.Error())
	checkCode(t, redeem(g.ID, f.targetToken, f.redeemProof(t, g, expired, "view")), 403, errSessionDenied.Error())
	f.count(t, 2, "SELECT count(*) FROM audit_events WHERE session_id=$1 AND reason='grant_expired'", g.ID)
}
