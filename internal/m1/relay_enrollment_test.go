package m1

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"deskrealm/internal/relayprotocol"
)

func relayNodeRequest(h http.Handler, path, credential string, payload any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(payload)
	r := httptest.NewRequest("POST", testOrigin+path, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	reply := httptest.NewRecorder()
	h.ServeHTTP(reply, r)
	return reply
}

func newRelayKeys(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(public), private
}

func signRelay(key ed25519.PrivateKey, message []byte) string {
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, message))
}

func TestRelayEnrollmentAndHealthPostgres(t *testing.T) {
	const adminPath = "/v1/admin/relays"
	const challengePath = "/v1/relays/enrollment/challenge"
	const completePath = "/v1/relays/enrollment/complete"
	const healthPath = "/v1/relays/health"
	ctx, store, _ := postgres(t)
	h, admin, csrf, _ := adminSession(t, ctx, store)
	createMember(t, h, admin, csrf)
	login := loginRequest(h, "member", testPassword)
	checkCode(t, login, 204, "")
	member := login.Result().Cookies()[0]
	left := relayRequest{"relay-a", "site-a", "north", "RELAY-A.example.test.:021117", "1.1.16", 10}
	right := relayRequest{"relay-b", "site-b", "south", "127.0.0.1:21118", "1.1.16", 20}
	checkCode(t, request(h, "POST", adminPath, testOrigin, left, member, csrfToken(member.Value)), 403, "forbidden")
	checkCode(t, request(h, "GET", adminPath, "", nil, member, ""), 403, "forbidden")
	checkCode(t, request(h, "POST", adminPath, testOrigin, left, admin, ""), 403, "csrf_denied")
	for _, endpoint := range []string{"[::ffff:0.0.0.0]:21117", "[::ffff:224.0.0.1]:21117", "[::ffff:127.0.0.1%eth0]:21117"} {
		invalid := left
		invalid.Endpoint = endpoint
		checkCode(t, request(h, "POST", adminPath, testOrigin, invalid, admin, csrf), 400, "invalid_task_request")
	}

	issue := func(input relayRequest) relayCredential {
		t.Helper()
		reply := request(h, "POST", adminPath, testOrigin, input, admin, csrf)
		checkCode(t, reply, 201, relayEnrollmentPrefix)
		if reply.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("credential response can be cached")
		}
		return responseValue[relayCredential](t, reply.Body.Bytes())
	}
	a, b := issue(left), issue(right)
	checkCode(t, request(h, "POST", adminPath, testOrigin, left, admin, csrf), 409, "task_target_conflict")
	pubA, keyA := newRelayKeys(t)
	pubB, keyB := newRelayKeys(t)
	checkCode(t, request(h, "POST", challengePath, testOrigin, map[string]string{"node_id": a.NodeID, "public_key": pubA},
		admin, csrf), 403, "origin_denied")
	challenge := func(id, credential, key string) *httptest.ResponseRecorder {
		return relayNodeRequest(h, challengePath, credential, map[string]string{"node_id": id, "public_key": key})
	}
	complete := func(record relayCredential, key, signature string) *httptest.ResponseRecorder {
		return relayNodeRequest(h, completePath, record.Credential,
			map[string]string{"node_id": record.NodeID, "public_key": key, "signature": signature})
	}
	checkCode(t, challenge(a.NodeID, b.Credential, pubA), 401, "managed_identity_denied")
	checkCode(t, challenge(a.NodeID, a.Credential, base64.RawURLEncoding.EncodeToString(append([]byte{1}, make([]byte, 31)...))),
		401, "managed_identity_denied")
	reply := challenge(a.NodeID, a.Credential, pubA)
	checkCode(t, reply, 200, "relay-a.example.test:21117")
	proofA := responseValue[relayprotocol.Enrollment](t, reply.Body.Bytes())
	replay := challenge(a.NodeID, a.Credential, pubA)
	if replay.Code != 200 || replay.Body.String() != reply.Body.String() {
		t.Fatal("challenge retry changed pending signed configuration")
	}
	checkCode(t, challenge(a.NodeID, a.Credential, pubB), 401, "managed_identity_denied")
	checkCode(t, complete(a, pubA, signRelay(keyB, proofA.Frame())), 401, "managed_identity_denied")
	modified := proofA
	modified.Endpoint = "replaced.invalid:21117"
	checkCode(t, complete(a, pubA, signRelay(keyA, modified.Frame())), 401, "managed_identity_denied")

	signature := signRelay(keyA, proofA.Frame())
	answers := make(chan int, 8)
	for range cap(answers) {
		go func() { answers <- complete(a, pubA, signature).Code }()
	}
	success := 0
	for range cap(answers) {
		switch code := <-answers; code {
		case 200:
			success++
		case 401:
		default:
			t.Fatalf("unexpected concurrent completion status %d", code)
		}
	}
	if success != 1 {
		t.Fatalf("credential consumed %d times", success)
	}
	checkCode(t, complete(a, pubA, signature), 401, "managed_identity_denied")
	checkCode(t, request(h, "POST", adminPath+"/"+a.NodeID+"/credential", testOrigin, struct{}{}, admin, csrf), 409, "task_target_conflict")
	checkCode(t, request(h, "GET", tasksPath+"/"+a.TaskID, "", nil, admin, ""), 200, "relay_pending_probe")

	reply = challenge(b.NodeID, b.Credential, pubA)
	checkCode(t, reply, 200, "127.0.0.1:21118")
	sharedKey := responseValue[relayprotocol.Enrollment](t, reply.Body.Bytes())
	checkCode(t, complete(b, pubA, signRelay(keyA, sharedKey.Frame())), 409, "task_target_conflict")
	taskSQL(t, ctx, store, "UPDATE relay_enrollments SET expires_at=clock_timestamp()-interval '1 second' WHERE node_id=$1", b.NodeID)
	checkCode(t, challenge(b.NodeID, b.Credential, pubB), 401, "managed_identity_denied")
	reply = request(h, "POST", adminPath+"/"+b.NodeID+"/credential", testOrigin, struct{}{}, admin, csrf)
	checkCode(t, reply, 201, b.TaskID)
	renewed := responseValue[relayCredential](t, reply.Body.Bytes())
	checkCode(t, challenge(b.NodeID, b.Credential, pubB), 401, "managed_identity_denied")
	reply = challenge(b.NodeID, renewed.Credential, pubB)
	checkCode(t, reply, 200, "127.0.0.1:21118")
	proofB := responseValue[relayprotocol.Enrollment](t, reply.Body.Bytes())
	taskSQL(t, ctx, store, "UPDATE relay_enrollments SET challenge_expires_at=clock_timestamp()-interval '1 second' WHERE node_id=$1", b.NodeID)
	checkCode(t, complete(renewed, pubB, signRelay(keyB, proofB.Frame())), 401, "managed_identity_denied")
	reply = challenge(b.NodeID, renewed.Credential, pubB)
	checkCode(t, reply, 200, b.NodeID)
	proofB = responseValue[relayprotocol.Enrollment](t, reply.Body.Bytes())
	checkCode(t, complete(renewed, pubB, signRelay(keyB, proofB.Frame())), 200, b.NodeID)

	report := func(health relayprotocol.Health, signer ed25519.PrivateKey) *httptest.ResponseRecorder {
		return relayNodeRequest(h, healthPath, "", struct {
			relayprotocol.Health
			Signature string `json:"signature"`
		}{health, signRelay(signer, health.Frame())})
	}
	var now time.Time
	if err := store.DB.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	configuration := func(proof relayprotocol.ConfigRequest, signer ed25519.PrivateKey) *httptest.ResponseRecorder {
		return relayNodeRequest(h, "/v1/relays/configuration", "", struct {
			relayprotocol.ConfigRequest
			Signature string `json:"signature"`
		}{proof, signRelay(signer, proof.Frame())})
	}
	configProof := relayprotocol.ConfigRequest{NodeID: a.NodeID, Nonce: proofA.Challenge, ObservedMS: now.UnixMilli()}
	checkCode(t, configuration(configProof, keyB), 401, "managed_identity_denied")
	checkCode(t, configuration(configProof, keyA), 200, proofA.Endpoint)
	configProof.ObservedMS = now.Add(-time.Minute).UnixMilli()
	checkCode(t, configuration(configProof, keyA), 401, "managed_identity_denied")
	status := relayprotocol.Health{NodeID: a.NodeID, ConfigVersion: 1, Sequence: 1,
		ObservedMS: now.UnixMilli(), Build: left.Build, ProtocolReady: true, ActiveSessions: 5}
	checkCode(t, report(status, keyB), 401, "managed_identity_denied")
	checkCode(t, report(status, keyA), 204, "")
	checkCode(t, report(status, keyA), 409, "task_target_conflict")
	for _, invalid := range []func(*relayprotocol.Health){
		func(h *relayprotocol.Health) { h.ActiveSessions = 11 },
		func(h *relayprotocol.Health) { h.ConfigVersion++ },
		func(h *relayprotocol.Health) { h.ObservedMS = now.Add(-time.Minute).UnixMilli() },
		func(h *relayprotocol.Health) { h.Build = "unapproved" },
	} {
		probe := status
		probe.Sequence++
		invalid(&probe)
		checkCode(t, report(probe, keyA), 409, "task_target_conflict")
	}
	status.NodeID = b.NodeID
	status.ActiveSessions = 0
	checkCode(t, report(status, keyB), 204, "")
	reply = request(h, "GET", adminPath, "", nil, admin, "")
	checkCode(t, reply, 200, "awaiting_probe")
	for _, secret := range []string{a.Credential, renewed.Credential, pubA, pubB, proofA.Challenge} {
		if strings.Contains(reply.Body.String(), secret) {
			t.Fatal("relay list leaked private registration material")
		}
	}
	nodes := responseValue[struct{ Nodes []relayNodeSummary }](t, reply.Body.Bytes()).Nodes
	if len(nodes) != 2 || nodes[0].State != "awaiting_probe" || nodes[1].State != "awaiting_probe" {
		t.Fatalf("health incorrectly changed probe eligibility: %+v", nodes)
	}
	taskSQL(t, ctx, store, "UPDATE relay_nodes SET health_at=clock_timestamp()-interval '16 seconds' WHERE id=$1", a.NodeID)
	reply = request(h, "GET", adminPath, "", nil, admin, "")
	nodes = responseValue[struct{ Nodes []relayNodeSummary }](t, reply.Body.Bytes()).Nodes
	if reply.Code != 200 || len(nodes) != 2 || nodes[0].State != "awaiting_health" || nodes[1].State != "awaiting_probe" {
		t.Fatalf("expired health changed the wrong eligibility: %+v", nodes)
	}
	var distinctCount, audits int
	if err := store.DB.QueryRow(ctx, "SELECT count(DISTINCT identity_key) FROM relay_nodes WHERE registered_at IS NOT NULL").
		Scan(&distinctCount); err != nil || distinctCount != 2 {
		t.Fatalf("relay identity count: %d %v", distinctCount, err)
	}
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='relay.registered' AND task_id IS NOT NULL`).
		Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("unexpected registration audit records: %d %v", audits, err)
	}
}
