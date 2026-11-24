package m1

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

func TestRegionalRelayPairingPostgres(t *testing.T) {
	f := newSessionFixture(t)
	a, _, _ := registerLifecycleRelay(t, f.h, f.admin, "alpha", "north")
	b, _, _ := registerLifecycleRelay(t, f.h, f.admin, "beta", "south")
	a.health(t, f.h, 1, 1, 0, 204)
	b.health(t, f.h, 1, 1, 1, 204)
	relaySweep(t, f.ctx, f.store)
	g := relayRoundSession(t, f, 901)
	controller, target := f.roundPeers()
	connection := testSessionUUID(902)
	checkCode(t, controller.join(t, f.h, g, connection), 200, `"regional":true`)
	response := target.join(t, f.h, g, connection)
	checkCode(t, response, 200, "collecting")
	round := responseValue[relayRound](t, response.Body.Bytes())
	f.sql(t, `UPDATE session_relay_rounds SET opened_at=statement_timestamp()-interval '3 seconds',
		deadline=statement_timestamp()-interval '1 second' WHERE id=$1`, round.ID)
	selectionURL := sessionsURL + "/" + g.ID + "/relay-selection?connection_id=" + connection
	pairingURL := sessionsURL + "/" + g.ID + "/relay-pairing"
	pair := relayPairingInput{Connection: connection, Round: round.ID, Relay: a.NodeID}
	frame, err := pair.frame(g, 1)
	if err != nil {
		t.Fatal(err)
	}
	pair.Signature = signRelay(a.key, frame)
	checkCode(t, nativeRequest(f.h, "POST", pairingURL, target.bearer, pair), 409, errSessionConflict.Error())
	response = nativeRequest(f.h, "GET", selectionURL, target.bearer, nil)
	checkCode(t, response, 200, "waiting_for_selection")
	if got := responseValue[relaySelectionResult](t, response.Body.Bytes()); got.Ticket != nil {
		t.Fatal("issued a ticket before confirmation")
	}
	response = controller.selectRelay(t, f.h, g, round, a.NodeID, "select")
	checkCode(t, response, 200, "confirmed")
	left := responseValue[relaySelectionResult](t, response.Body.Bytes()).Ticket
	response = nativeRequest(f.h, "GET", selectionURL, target.bearer, nil)
	checkCode(t, response, 200, a.NodeID)
	right := responseValue[relaySelectionResult](t, response.Body.Bytes()).Ticket
	for _, issue := range []struct {
		peer   roundPeer
		ticket *relaySessionTicket
	}{{controller, left}, {target, right}} {
		if issue.ticket == nil {
			t.Fatal("confirmed endpoint did not receive a ticket")
		}
		wire, err := base64.RawURLEncoding.DecodeString(issue.ticket.Frame)
		sig, valid := canonicalSignature(issue.ticket.Signature)
		if err != nil || !valid || len(wire) != 199 || wire[10] != issue.peer.role ||
			!bytes.Contains(wire, issue.peer.key.Public().(ed25519.PublicKey)) ||
			!ed25519.Verify(f.signing.key.Public().(ed25519.PublicKey), wire, sig) ||
			issue.ticket.PublicKey != base64.RawURLEncoding.EncodeToString(a.key.Public().(ed25519.PublicKey)) {
			t.Fatal("ticket did not bind the issuing center, role key and selected relay")
		}
	}
	checkCode(t, nativeRequest(f.h, "POST", pairingURL, controller.bearer, pair), 401, errManagedIdentity.Error())
	wrong := pair
	wrong.Signature = signRelay(b.key, frame)
	checkCode(t, nativeRequest(f.h, "POST", pairingURL, target.bearer, wrong), 401, errManagedIdentity.Error())
	wrong = pair
	wrong.Relay = b.NodeID
	checkCode(t, nativeRequest(f.h, "POST", pairingURL, target.bearer, wrong), 409, errSessionConflict.Error())
	for range 2 {
		checkCode(t, nativeRequest(f.h, "POST", pairingURL, target.bearer, pair), 204, "")
	}
	f.count(t, 1, "SELECT count(*) FROM managed_session_routes WHERE session_id=$1 AND connection_id=$2 AND relay_id=$3 AND paired_at IS NOT NULL", g.ID, connection, a.NodeID)
	f.count(t, 1, "SELECT count(*) FROM audit_events WHERE session_id=$1 AND action='session.relay_paired'", g.ID)
	checkCode(t, controller.selectRelay(t, f.h, g, round, a.NodeID, "fail"), 200, b.NodeID)
	f.count(t, 0, "SELECT count(*) FROM managed_session_routes WHERE session_id=$1 AND connection_id=$2", g.ID, connection)
	checkCode(t, nativeRequest(f.h, "POST", pairingURL, target.bearer, pair), 409, errSessionConflict.Error())
	checkCode(t, controller.selectRelay(t, f.h, g, round, b.NodeID, "select"), 200, "confirmed")
	pair.Relay = b.NodeID
	frame, err = pair.frame(g, 1)
	if err != nil {
		t.Fatal(err)
	}
	pair.Signature = signRelay(b.key, frame)
	checkCode(t, nativeRequest(f.h, "POST", pairingURL, target.bearer, pair), 204, "")
	checkCode(t, controller.join(t, f.h, g, testSessionUUID(903)), 200, "waiting_for_target")
	route := testSessionRoute(g, 902, "relay")
	route.Relay = b.NodeID
	redeem := f.redeemProof(t, g, route, "view")
	checkCode(t, nativeRequest(f.h, "POST", sessionsURL+"/"+g.ID+"/redeem", target.bearer, redeem), 403, errSessionDenied.Error())
}
