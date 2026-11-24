package m1

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"
)

func (p roundPeer) selectRelay(t *testing.T, h http.Handler, g sessionGrant, round relayRound, relay, action string) *httptest.ResponseRecorder {
	t.Helper()
	input := relaySelectionRequest{Connection: round.Connection, Round: round.ID, Relay: relay, Action: action}
	frame, err := input.frame(g)
	if err != nil {
		t.Fatal(err)
	}
	input.Signature = signRelay(p.key, frame)
	return nativeRequest(h, "POST", sessionsURL+"/"+g.ID+"/relay-selection", p.bearer, input)
}

func TestSessionRelaySelectionPostgres(t *testing.T) {
	f := newSessionFixture(t)
	alpha, _, _ := registerLifecycleRelay(t, f.h, f.admin, "alpha", "north")
	beta, _, _ := registerLifecycleRelay(t, f.h, f.admin, "beta", "south")
	alpha.health(t, f.h, 1, 1, 1, 204)
	beta.health(t, f.h, 1, 1, 0, 204)
	relaySweep(t, f.ctx, f.store)
	g := relayRoundSession(t, f, 701)
	controller, target := f.roundPeers()
	path := sessionsURL + "/" + g.ID + "/relay-selection"
	open := func(serial int) relayRound {
		t.Helper()
		connection := testSessionUUID(serial)
		checkCode(t, controller.join(t, f.h, g, connection), 200, "waiting_for_target")
		response := target.join(t, f.h, g, connection)
		checkCode(t, response, 200, "collecting")
		return responseValue[relayRound](t, response.Body.Bytes())
	}
	closeProbes := func(round relayRound) {
		t.Helper()
		f.sql(t, `UPDATE session_relay_rounds SET opened_at=statement_timestamp()-interval '3 seconds',
			deadline=statement_timestamp()-interval '1 second' WHERE id=$1`, round.ID)
	}
	read := func(peer roundPeer, round relayRound) *httptest.ResponseRecorder {
		return nativeRequest(f.h, "GET", path+"?connection_id="+round.Connection, peer.bearer, nil)
	}
	write := func(peer roundPeer, round relayRound, relay, action string) *httptest.ResponseRecorder {
		t.Helper()
		return peer.selectRelay(t, f.h, g, round, relay, action)
	}
	report := func(round relayRound, node string, one, two uint32) {
		t.Helper()
		candidate := roundCandidate(t, round, node)
		for _, measurement := range []struct {
			peer roundPeer
			rtt  uint32
		}{{controller, one}, {target, two}} {
			samples := []relayProbeSample{measuredRTT(measurement.rtt - 1000), measuredRTT(measurement.rtt + 1000), {Result: "timeout"}}
			checkCode(t, measurement.peer.report(t, f.h, g, round, candidate, samples), 204, "")
		}
	}
	expectFirst := func(reply *httptest.ResponseRecorder, relay string, sum uint32) relaySelectionResult {
		t.Helper()
		checkCode(t, reply, 200, relay)
		out := responseValue[relaySelectionResult](t, reply.Body.Bytes())
		if out.State != "ready" || len(out.Options) == 0 || out.Options[0].Relay != relay ||
			out.Options[0].CombinedRTT == nil || *out.Options[0].CombinedRTT != sum || out.Options[0].Reason != "dual_rtt" {
			t.Fatalf("unexpected first relay option: %+v", out)
		}
		return out
	}
	first := open(702)
	checkCode(t, read(controller, first), 409, errSessionConflict.Error())
	report(first, alpha.NodeID, 20_000, 120_000)
	report(first, beta.NodeID, 60_000, 50_000)
	closeProbes(first)
	expectFirst(read(controller, first), beta.NodeID, 110_000)
	checkCode(t, read(target, first), 200, "waiting_for_selection")
	checkCode(t, write(target, first, beta.NodeID, "select"), 401, errManagedIdentity.Error())
	wrongKey := roundPeer{relayController, f.userToken, f.targetKey}
	checkCode(t, write(wrongKey, first, beta.NodeID, "select"), 401, errManagedIdentity.Error())
	checkCode(t, write(controller, first, alpha.NodeID, "select"), 409, errSessionConflict.Error())
	// The preview becomes stale when the preferred node fills up.
	beta.health(t, f.h, 1, 2, 3, 204)
	checkCode(t, write(controller, first, beta.NodeID, "select"), 409, errSessionConflict.Error())
	expectFirst(read(controller, first), alpha.NodeID, 140_000)
	beta.health(t, f.h, 1, 3, 0, 204)
	for range 2 {
		checkCode(t, write(controller, first, beta.NodeID, "select"), 200, "confirmed")
	}
	checkCode(t, read(target, first), 200, beta.NodeID)
	f.sql(t, "UPDATE relay_nodes SET mode='draining' WHERE id=$1", beta.NodeID)
	checkCode(t, read(target, first), 200, "unavailable")
	checkCode(t, write(controller, first, beta.NodeID, "select"), 409, errSessionConflict.Error())
	expectFirst(write(controller, first, beta.NodeID, "fail"), alpha.NodeID, 140_000)
	checkCode(t, write(controller, first, alpha.NodeID, "select"), 200, "confirmed")
	checkCode(t, write(controller, first, beta.NodeID, "fail"), 200, alpha.NodeID)
	checkCode(t, write(controller, first, alpha.NodeID, "fail"), 200, "exhausted")
	checkCode(t, write(controller, first, beta.NodeID, "select"), 409, errSessionConflict.Error())
	f.count(t, 2, "SELECT count(*) FROM session_relay_selections WHERE round_id=$1 AND failed_at IS NOT NULL", first.ID)
	f.count(t, 2, "SELECT count(*) FROM audit_events WHERE session_id=$1 AND action='session.relay_selected'", g.ID)
	f.count(t, 2, "SELECT count(*) FROM audit_events WHERE session_id=$1 AND action='session.relay_selection_failed'", g.ID)

	f.sql(t, "UPDATE relay_nodes SET mode='enabled' WHERE id=$1", beta.NodeID)
	second := open(703)
	checkCode(t, read(controller, first), 409, errSessionConflict.Error())
	report(second, alpha.NodeID, 40_000, 60_000)
	report(second, beta.NodeID, 60_000, 40_000)
	closeProbes(second)
	expectFirst(read(controller, second), beta.NodeID, 100_000)
	alpha.health(t, f.h, 1, 2, 0, 204)
	ordered := []string{alpha.NodeID, beta.NodeID}
	slices.Sort(ordered)
	expectFirst(read(controller, second), ordered[0], 100_000)
	f.sql(t, `UPDATE session_relay_rounds SET opened_at=statement_timestamp()-interval '13 seconds',
		deadline=statement_timestamp()-interval '12 seconds' WHERE id=$1`, second.ID)
	checkCode(t, write(controller, second, ordered[0], "select"), 409, errSessionConflict.Error())

	csrf := csrfToken(f.admin.Value)
	policy := relayNetworkPolicy{Mode: "organization", DefaultRegion: "north",
		GeoRegions: []relayRegionNetwork{{netip.MustParsePrefix("192.0.2.0/24"), "south"}}}
	checkCode(t, request(f.h, "POST", relayNetworkURL, testOrigin, policy, f.admin, csrf), 200, `"revision":1`)
	third := open(704)
	closeProbes(third)
	response := read(controller, third)
	checkCode(t, response, 200, "geoip")
	out := responseValue[relaySelectionResult](t, response.Body.Bytes())
	if len(out.Options) != 2 || out.Options[0].Relay != beta.NodeID || out.Options[0].Reason != "geoip" || out.Options[0].CombinedRTT != nil {
		t.Fatalf("expected GeoIP to take priority over the default region: %+v", out)
	}
	policy.Revision, policy.GeoRegions = 1, nil
	checkCode(t, request(f.h, "POST", relayNetworkURL, testOrigin, policy, f.admin, csrf), 200, `"revision":2`)
	checkCode(t, write(controller, third, beta.NodeID, "select"), 409, errSessionConflict.Error())
	fourth := open(705)
	closeProbes(fourth)
	response = read(controller, fourth)
	checkCode(t, response, 200, "default_region")
	out = responseValue[relaySelectionResult](t, response.Body.Bytes())
	if len(out.Options) != 2 || out.Options[0].Relay != alpha.NodeID || out.Options[0].Reason != "default_region" {
		t.Fatalf("expected default region without a matching GeoIP rule: %+v", out)
	}
	checkCode(t, write(controller, fourth, alpha.NodeID, "select"), 200, "default_region")
	f.count(t, 1, `SELECT count(*) FROM session_relay_selections WHERE round_id=$1 AND relay_id=$2
		AND reason='default_region' AND combined_rtt_us IS NULL`, fourth.ID, alpha.NodeID)
}

func TestRelaySelectionFallbackOrder(t *testing.T) {
	policy := relayNetworkPolicy{
		Mode:          "restricted",
		DefaultRegion: "default",
		Networks:      []relaySiteNetwork{{netip.MustParsePrefix("192.0.2.0/24"), "office"}},
		GeoRegions: []relayRegionNetwork{
			{netip.MustParsePrefix("198.51.100.0/24"), "geo"},
			{netip.MustParsePrefix("198.51.0.0/16"), "elsewhere"},
		},
	}
	node := func(id, site, region string) relayRoundCandidate {
		return relayRoundCandidate{ID: id, Site: site, Region: region, Eligible: true, capacity: 4}
	}
	dead := node("0", "office", "geo")
	dead.Target.Samples = []relayProbeSample{{Result: "unreachable"}, {Result: "protocol_mismatch"}, {Result: "unreachable"}}
	timeout := node("2", "remote", "geo")
	timeout.Target.Samples = []relayProbeSample{{Result: "timeout"}, {Result: "timeout"}, {Result: "timeout"}}
	round := relayRound{controllerAddress: "192.0.2.5", targetAddress: "198.51.100.12",
		Candidates: []relayRoundCandidate{node("z", "remote", "elsewhere"), node("3", "remote", "default"), timeout,
			node("1", "office", "other"), dead}}
	got := orderRelayOptions(round, policy, nil)
	if len(got) != 4 {
		t.Fatalf("unexpected fallback list: %+v", got)
	}
	for i, want := range []struct{ id, reason string }{{"1", "site"}, {"2", "geoip"}, {"3", "default_region"}, {"z", "available"}} {
		if got[i].Relay != want.id || got[i].Reason != want.reason || got[i].CombinedRTT != nil {
			t.Fatalf("unexpected fallback option %d: %+v", i, got[i])
		}
	}
}
