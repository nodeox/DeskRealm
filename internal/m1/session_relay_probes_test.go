package m1

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type roundPeer struct {
	role   byte
	bearer string
	key    ed25519.PrivateKey
}

func (f sessionFixture) roundPeers() (roundPeer, roundPeer) {
	return roundPeer{relayController, f.userToken, f.controllerKey}, roundPeer{relayTarget, f.targetToken, f.targetKey}
}

func (p roundPeer) join(t *testing.T, h http.Handler, g sessionGrant, connection string) *httptest.ResponseRecorder {
	t.Helper()
	frame, err := relayJoinFrame(g, p.role, connection)
	if err != nil {
		t.Fatal(err)
	}
	return nativeRequest(h, "POST", sessionsURL+"/"+g.ID+"/relay-candidates", p.bearer,
		map[string]string{"connection_id": connection, "signature": signRelay(p.key, frame)})
}

func (p roundPeer) report(t *testing.T, h http.Handler, g sessionGrant, round relayRound, candidate relayRoundCandidate,
	samples []relayProbeSample) *httptest.ResponseRecorder {
	t.Helper()
	wire, ok := relaySampleBytes(samples)
	if !ok {
		t.Fatal("invalid fixture samples")
	}
	frame, err := relayReportFrame(g, p.role, round, candidate.ID, candidate.Endpoint, candidate.Version, wire)
	if err != nil {
		t.Fatal(err)
	}
	return nativeRequest(h, "POST", sessionsURL+"/"+g.ID+"/relay-probes", p.bearer, map[string]any{
		"connection_id": round.Connection, "round_id": round.ID, "relay_id": candidate.ID,
		"samples": samples, "signature": signRelay(p.key, frame),
	})
}

func (p roundPeer) get(h http.Handler, g sessionGrant, connection string) *httptest.ResponseRecorder {
	return nativeRequest(h, "GET", sessionsURL+"/"+g.ID+"/relay-candidates?connection_id="+connection, p.bearer, nil)
}

func roundCandidate(t *testing.T, round relayRound, id string) relayRoundCandidate {
	t.Helper()
	for _, item := range round.Candidates {
		if item.ID == id {
			return item
		}
	}
	t.Fatal("missing relay candidate", id)
	return relayRoundCandidate{}
}

func measuredRTT(us uint32) relayProbeSample {
	return relayProbeSample{Result: "ok", RTT: &us}
}

func relayRoundSession(t *testing.T, f sessionFixture, serial int) sessionGrant {
	t.Helper()
	changeTestRule(t, f.h, f.admin, f.rule, "publish")
	f.applyPolicy(t, []string{"view"}, []string{"view"})
	input := f.input(serial)
	input.Actions = []string{"view"}
	return f.issue(t, input)
}

func TestSessionRelayRoundPostgres(t *testing.T) {
	f := newSessionFixture(t)
	alpha, _, _ := registerLifecycleRelay(t, f.h, f.admin, "alpha", "north")
	beta, _, _ := registerLifecycleRelay(t, f.h, f.admin, "beta", "south")
	drained, _, _ := registerLifecycleRelay(t, f.h, f.admin, "drained", "north")
	full, _, _ := registerLifecycleRelay(t, f.h, f.admin, "full", "south")
	expired, _, _ := registerLifecycleRelay(t, f.h, f.admin, "expired", "south")
	for _, relay := range []lifecycleRelayFixture{alpha, beta, drained, expired} {
		relay.health(t, f.h, 1, 1, 0, 204)
	}
	full.health(t, f.h, 1, 1, 3, 204)
	relaySweep(t, f.ctx, f.store)
	f.sql(t, "UPDATE relay_nodes SET mode='draining' WHERE id=$1", drained.NodeID)
	f.sql(t, "UPDATE relay_nodes SET health_at=statement_timestamp()-interval '16 seconds' WHERE id=$1", expired.NodeID)
	g := relayRoundSession(t, f, 109)
	controller, target := f.roundPeers()
	connection := testSessionUUID(1010)
	checkCode(t, target.join(t, f.h, g, connection), 409, errSessionConflict.Error())
	misbound := roundPeer{relayController, f.controllerToken, f.controllerKey}
	checkCode(t, misbound.join(t, f.h, g, connection), 401, errManagedIdentity.Error())
	misbound = roundPeer{relayController, f.userToken, f.targetKey}
	checkCode(t, misbound.join(t, f.h, g, connection), 401, errManagedIdentity.Error())
	first := controller.join(t, f.h, g, connection)
	checkCode(t, first, 200, "waiting_for_target")
	waiting := responseValue[relayRound](t, first.Body.Bytes())
	if waiting.Version != 1 || len(waiting.Candidates) != 0 || waiting.Deadline != nil {
		t.Fatalf("unexpected pending round: %+v", waiting)
	}
	reply := target.join(t, f.h, g, connection)
	checkCode(t, reply, 200, "collecting")
	round := responseValue[relayRound](t, reply.Body.Bytes())
	if round.ID != waiting.ID || round.Opened == nil || round.Deadline == nil || round.Deadline.Sub(*round.Opened) != 2*time.Second ||
		len(round.Candidates) != 2 {
		t.Fatalf("unexpected opened round: %+v", round)
	}
	repeated := target.join(t, f.h, g, connection)
	checkCode(t, repeated, 200, waiting.ID)
	if current := responseValue[relayRound](t, repeated.Body.Bytes()); !current.Deadline.Equal(*round.Deadline) {
		t.Fatal("replaying join extended the measurement window")
	}
	a, b := roundCandidate(t, round, alpha.NodeID), roundCandidate(t, round, beta.NodeID)
	if !a.Eligible || !b.Eligible || a.Endpoint != alpha.endpoint || b.Endpoint != beta.endpoint {
		t.Fatalf("unexpected candidates: %+v %+v", a, b)
	}
	left := []relayProbeSample{measuredRTT(20_000), {Result: "timeout"}, measuredRTT(28_000)}
	right := []relayProbeSample{{Result: "unreachable"}, measuredRTT(44_000), {Result: "missing"}}
	third := []relayProbeSample{measuredRTT(31_000), measuredRTT(12_000), measuredRTT(18_000)}
	checkCode(t, controller.report(t, f.h, g, round, a, left), 204, "")
	checkCode(t, controller.report(t, f.h, g, round, a, left), 409, errSessionConflict.Error())
	checkCode(t, target.report(t, f.h, g, round, a, right), 204, "")
	badRole := roundPeer{relayTarget, f.targetToken, f.controllerKey}
	checkCode(t, badRole.report(t, f.h, g, round, b, third), 401, errManagedIdentity.Error())
	wrongEndpoint := b
	wrongEndpoint.Endpoint = a.Endpoint
	checkCode(t, controller.report(t, f.h, g, round, wrongEndpoint, third), 401, errManagedIdentity.Error())
	wrongVersion := b
	wrongVersion.Version++
	checkCode(t, controller.report(t, f.h, g, round, wrongVersion, third), 401, errManagedIdentity.Error())
	wrongRound := round
	wrongRound.ID = testSessionUUID(1111)
	checkCode(t, controller.report(t, f.h, g, wrongRound, b, third), 409, errSessionConflict.Error())
	highLoad := b
	highLoad.ID, highLoad.Endpoint = full.NodeID, full.endpoint
	checkCode(t, controller.report(t, f.h, g, round, highLoad, third), 409, errSessionConflict.Error())
	checkCode(t, controller.report(t, f.h, g, round, b, third), 204, "")
	f.sql(t, "UPDATE relay_nodes SET mode='draining',revision=revision+1 WHERE id=$1", beta.NodeID)
	checkCode(t, target.report(t, f.h, g, round, b, third), 409, errSessionConflict.Error())
	f.sql(t, `UPDATE session_relay_rounds SET opened_at=opened_at-interval '3 seconds',deadline=deadline-interval '3 seconds'
		WHERE id=$1`, round.ID)
	checkCode(t, target.report(t, f.h, g, round, a, third), 409, errSessionConflict.Error())
	reply = controller.get(f.h, g, connection)
	checkCode(t, reply, 200, "complete")
	complete := responseValue[relayRound](t, reply.Body.Bytes())
	a, b = roundCandidate(t, complete, alpha.NodeID), roundCandidate(t, complete, beta.NodeID)
	if a.Controller.Median == nil || *a.Controller.Median != 24_000 || a.Target.State != "insufficient" ||
		a.Target.Median != nil || a.Target.Samples[0].Result != "unreachable" ||
		b.Controller.Median == nil || *b.Controller.Median != 18_000 || b.Target.State != "missing" || b.Eligible {
		t.Fatalf("unexpected measured samples: %+v %+v", a, b)
	}
	next := testSessionUUID(1011)
	checkCode(t, controller.join(t, f.h, g, next), 200, "waiting_for_target")
	checkCode(t, controller.join(t, f.h, g, connection), 409, errSessionConflict.Error())
	checkCode(t, target.get(f.h, g, connection), 409, errSessionConflict.Error())
	reply = target.join(t, f.h, g, next)
	checkCode(t, reply, 200, "collecting")
	current := responseValue[relayRound](t, reply.Body.Bytes())
	if current.Version != 2 || len(current.Candidates) != 1 || current.Candidates[0].ID != alpha.NodeID {
		t.Fatalf("superseding round retained stale candidates: %+v", current)
	}
	f.sql(t, "INSERT INTO managed_session_revocations(session_id,reason) VALUES($1,'fixture')", g.ID)
	checkCode(t, controller.get(f.h, g, next), 403, errSessionDenied.Error())
	f.count(t, 3, `SELECT count(*) FROM session_relay_reports rep JOIN session_relay_rounds r ON r.id=rep.round_id WHERE r.session_id=$1`, g.ID)
}
