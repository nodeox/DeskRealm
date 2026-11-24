package m1

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

const relayNetworkURL = "/v1/admin/relay-network-policy"

func TestRelayIngressUsesOnlyTrustedProxy(t *testing.T) {
	for _, value := range []string{"10.0.0.0/8", "::ffff:127.0.0.1%en0", "0.0.0.0", "ff02::1", "127.0.0.1,::ffff:127.0.0.1"} {
		if _, err := ConfigureRelayIngress(value); err == nil {
			t.Fatalf("accepted invalid proxy setting %q", value)
		}
	}
	ingress, err := ConfigureRelayIngress("192.0.2.7, 2001:db8::a")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		peer      string
		forwarded []string
		want      string
	}{
		{"198.51.100.2:32123", []string{"10.0.0.1"}, "198.51.100.2"},
		{"192.0.2.7:32123", []string{"::ffff:10.0.0.1"}, "10.0.0.1"},
		{"[2001:db8::a]:32123", []string{"2001:db8:4::12"}, "2001:db8:4::12"},
		{"192.0.2.7:32123", nil, ""},
		{"192.0.2.7:32123", []string{"10.0.0.1, 10.0.0.2"}, ""},
		{"192.0.2.7:32123", []string{"10.0.0.1", "10.0.0.2"}, ""},
		{"malformed", []string{"10.0.0.1"}, ""},
	} {
		r := httptest.NewRequest("GET", testOrigin, nil)
		r.RemoteAddr = item.peer
		for _, header := range item.forwarded {
			r.Header.Add("X-Forwarded-For", header)
		}
		got, accepted := ingress.clientIP(r)
		if accepted != (item.want != "") || accepted && got.String() != item.want {
			t.Fatalf("peer %q and header %v produced %s %t, want %q", item.peer, item.forwarded, got, accepted, item.want)
		}
	}
}

func TestSessionRelayNetworkPolicyPostgres(t *testing.T) {
	f := newSessionFixture(t)
	alpha, _, _ := registerLifecycleRelay(t, f.h, f.admin, "alpha", "north")
	beta, _, _ := registerLifecycleRelay(t, f.h, f.admin, "beta", "south")
	gamma, _, _ := registerLifecycleRelay(t, f.h, f.admin, "gamma", "north")
	for _, relay := range []lifecycleRelayFixture{alpha, beta, gamma} {
		relay.health(t, f.h, 1, 1, 0, 204)
	}
	relaySweep(t, f.ctx, f.store)
	grant := relayRoundSession(t, f, 190)
	controller, target := f.roundPeers()
	ingress, err := ConfigureRelayIngress("192.0.2.7")
	if err != nil {
		t.Fatal(err)
	}
	origin, err := ParseOrigin(testOrigin, false)
	if err != nil {
		t.Fatal(err)
	}
	center := NewHandler(f.store, origin, nil, AuthConfig{ManagedKey: f.signing, RelayIngress: ingress})
	from := func(remote, forwarded string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = remote
			if forwarded != "" {
				r.Header.Set("X-Forwarded-For", forwarded)
			}
			center.ServeHTTP(w, r)
		})
	}
	left := from("192.0.2.7:41001", "10.1.2.42")
	right := from("[2001:db8:8::5]:41002", "")
	moved := from("192.0.2.7:41001", "10.1.2.43")
	spoof := from("198.51.100.50:41002", "2001:db8:8::5")
	policy := relayNetworkPolicy{
		Mode: "restricted",
		Networks: []relaySiteNetwork{
			{netip.MustParsePrefix("10.0.0.0/8"), "other-office"},
			{netip.MustParsePrefix("10.1.2.0/24"), "office"},
			{netip.MustParsePrefix("2001:db8:8::/64"), "plant"},
		},
		Pairs: []relaySitePair{{"office", "plant", []string{"north"}}},
		Relays: []relayReachability{
			{alpha.NodeID, []netip.Prefix{netip.MustParsePrefix("10.1.2.0/24"), netip.MustParsePrefix("2001:db8:8::/64")}},
			{beta.NodeID, []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}},
			{gamma.NodeID, []netip.Prefix{netip.MustParsePrefix("10.1.2.0/24")}},
		},
	}
	save := func(who *http.Cookie, csrf string, value relayNetworkPolicy) *httptest.ResponseRecorder {
		return request(center, "POST", relayNetworkURL, testOrigin, value, who, csrf)
	}
	csrf := csrfToken(f.admin.Value)
	checkCode(t, request(center, "GET", relayNetworkURL, "", nil, f.admin, ""), 200, `"revision":0`)
	checkCode(t, request(center, "GET", relayNetworkURL, "", nil, f.user, ""), 403, "forbidden")
	checkCode(t, save(f.user, csrfToken(f.user.Value), policy), 403, "forbidden")
	checkCode(t, save(f.admin, "", policy), 403, "csrf_denied")
	invalid := policy
	invalid.Networks = append([]relaySiteNetwork{{netip.MustParsePrefix("10.1.2.5/24"), "office"}}, policy.Networks...)
	checkCode(t, save(f.admin, csrf, invalid), 400, errRelayInput.Error())
	invalid = policy
	invalid.Relays = []relayReachability{{testSessionUUID(4405), policy.Relays[0].Networks}}
	checkCode(t, save(f.admin, csrf, invalid), 400, errRelayInput.Error())

	open := func(serial int, l, r http.Handler) relayRound {
		t.Helper()
		connection := testSessionUUID(serial)
		checkCode(t, controller.join(t, l, grant, connection), 200, "waiting_for_target")
		reply := target.join(t, r, grant, connection)
		checkCode(t, reply, 200, "collecting")
		return responseValue[relayRound](t, reply.Body.Bytes())
	}
	original := open(192, left, right)
	if original.NetworkRevision != 0 || len(original.Candidates) != 3 {
		t.Fatalf("unexpected baseline candidates: %+v", original)
	}
	checkCode(t, save(f.admin, csrf, policy), 200, `"revision":1`)
	checkCode(t, save(f.admin, csrf, policy), 409, errRelayConflict.Error())
	checkCode(t, controller.get(left, grant, original.Connection), 409, errSessionConflict.Error())
	values := []relayProbeSample{measuredRTT(1200), measuredRTT(1600), {Result: "timeout"}}
	a := roundCandidate(t, original, alpha.NodeID)
	checkCode(t, controller.report(t, left, grant, original, a, values), 409, errSessionConflict.Error())
	restricted := open(193, left, right)
	if restricted.NetworkRevision != 1 || len(restricted.Candidates) != 1 || restricted.Candidates[0].ID != alpha.NodeID {
		t.Fatalf("site pair, region and both network scopes did not select just alpha: %+v", restricted)
	}
	checkCode(t, controller.get(moved, grant, restricted.Connection), 409, errSessionConflict.Error())
	checkCode(t, target.get(spoof, grant, restricted.Connection), 409, errSessionConflict.Error())
	a = restricted.Candidates[0]
	checkCode(t, controller.report(t, moved, grant, restricted, a, values), 409, errSessionConflict.Error())
	checkCode(t, controller.report(t, left, grant, restricted, a, values), 204, "")
	checkCode(t, target.report(t, right, grant, restricted, a, values), 204, "")
	if reversed := open(194, right, left); len(reversed.Candidates) != 0 {
		t.Fatalf("explicit site pair unexpectedly matched in reverse: %+v", reversed)
	}
	if untrusted := open(195, left, spoof); len(untrusted.Candidates) != 0 {
		t.Fatalf("spoofed forwarded address matched a CIDR: %+v", untrusted)
	}
	checkCode(t, save(f.admin, csrf, relayNetworkPolicy{Revision: 1, Mode: "organization"}), 200, `"revision":2`)
	if restored := open(196, left, right); restored.NetworkRevision != 2 || len(restored.Candidates) != 3 {
		t.Fatalf("organization mode did not restore compatibility: %+v", restored)
	}
	f.count(t, 2, "SELECT count(*) FROM audit_events WHERE action='relay.network_policy_updated'")
	f.count(t, 2, "SELECT count(*) FROM session_relay_reports WHERE round_id=$1", restricted.ID)
}
