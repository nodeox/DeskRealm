package m1

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const adminSessionsURL = "/v1/admin/sessions"

type issuedLease struct {
	session, path, connection, credential string
}

func (f sessionFixture) establish(t *testing.T, in sessionIssueRequest, conn int, actions ...string) issuedLease {
	t.Helper()
	g := f.issue(t, in)
	route := testSessionRoute(g, conn, "direct")
	checkCode(t, nativeRequest(f.h, "POST", sessionAuthorizeURL, f.rendezvous, route), 204, "")
	path := sessionsURL + "/" + g.ID
	proof := f.redeemProof(t, g, route, actions...)
	redeemed := nativeRequest(f.h, "POST", path+"/redeem", f.targetToken, proof)
	checkCode(t, redeemed, 200, "")
	credential := responseValue[struct {
		Credential string `json:"session_credential"`
	}](t, redeemed.Body.Bytes()).Credential
	if managedDigest(credential, sessionReceiptPrefix) == nil {
		t.Fatal("missing receipt credential")
	}
	return issuedLease{g.ID, path, route.Connection, credential}
}

func (l issuedLease) renew(h http.Handler, device string, seq int64) *httptest.ResponseRecorder {
	return nativeRequest(h, "POST", l.path+"/renew", device, map[string]any{
		"connection_id": l.connection, "session_credential": l.credential, "lease_sequence": seq,
	})
}

func (l issuedLease) status(h http.Handler) *httptest.ResponseRecorder {
	return nativeRequest(h, "GET", l.path+"/status?connection_id="+l.connection, l.credential, nil)
}

func (l issuedLease) stop(h http.Handler, seq int64, reason string) *httptest.ResponseRecorder {
	return nativeRequest(h, "POST", l.path+"/stopped", l.credential, map[string]any{
		"connection_id": l.connection, "lease_sequence": seq, "reason": reason,
	})
}

func TestSessionLeasesPostgres(t *testing.T) {
	f := newSessionFixture(t)
	changeTestRule(t, f.h, f.admin, f.rule, "publish")
	f.applyPolicy(t, []string{"view", "file_transfer"}, nil)
	in := f.input(40)
	in.Actions = []string{"view", "file_transfer"}
	l := f.establish(t, in, 41, "view", "file_transfer")
	detail := adminSessionsURL + "/" + l.session
	checkCode(t, l.renew(f.h, f.controllerToken, 1), 401, errManagedIdentity.Error())
	checkCode(t, l.renew(f.h, l.credential, 1), 401, errManagedIdentity.Error())
	checkCode(t, l.renew(f.h, f.targetToken, 3), 409, errSessionConflict.Error())
	checkCode(t, l.stop(f.h, 1, "revoked"), 409, errSessionConflict.Error())

	// Write failures roll the lease back to the original sequence.
	f.sql(t, "ALTER TABLE audit_events ADD CONSTRAINT fixture_renew CHECK(action <> 'session.renewed') NOT VALID")
	checkCode(t, l.renew(f.h, f.targetToken, 1), 500, "")
	checkCode(t, l.status(f.h), 200, `"lease_sequence":1`)
	f.sql(t, "ALTER TABLE audit_events DROP CONSTRAINT fixture_renew")
	first := l.renew(f.h, f.targetToken, 1)
	checkCode(t, first, 200, `"lease_sequence":2`)
	// Rotation of short-lived controller credentials does not invalidate the original bound login.
	grant := f.readGrant(t, l.session)
	f.sql(t, "DELETE FROM managed_controller_credentials WHERE digest=$1", grant.ControllerDigest)

	// Recreate the handler with only its persisted store. Receipt authentication
	// and bound expiry are independent of the center's process-local state.
	restarted := managedHandler(t, f.store, f.signing)
	again := l.renew(restarted, f.targetToken, 1)
	checkCode(t, again, 200, `"lease_sequence":2`)
	if !bytes.Equal(first.Body.Bytes(), again.Body.Bytes()) {
		t.Fatal("renewal replay extended or changed the lease")
	}
	checkCode(t, l.renew(restarted, f.targetToken, 2), 200, `"lease_sequence":3`)
	checkCode(t, l.renew(restarted, f.targetToken, 1), 409, errSessionConflict.Error())
	f.auditCount(t, l.session, "session.renewed", 2)

	size := int64(2048)
	file := sessionFileInput{Connection: l.connection, Event: testSessionUUID(43), Sequence: 2,
		Direction: "download", Result: "completed", Size: &size}
	filePath := l.path + "/file-events"
	checkCode(t, nativeRequest(f.h, "POST", filePath, f.targetToken, file), 401, errManagedIdentity.Error())
	checkCode(t, nativeRequest(f.h, "POST", filePath, l.credential, file), 204, "")
	checkCode(t, nativeRequest(restarted, "POST", filePath, l.credential, file), 204, "")
	file.Direction = "upload"
	checkCode(t, nativeRequest(f.h, "POST", filePath, l.credential, file), 409, errSessionConflict.Error())
	f.auditCount(t, l.session, "session.file_reported", 1)

	owner := request(f.h, "GET", detail, "", nil, f.user, "")
	checkCode(t, owner, 200, `"can_revoke":false`)
	if strings.Contains(owner.Body.String(), l.credential) || strings.Contains(owner.Body.String(), "signature") {
		t.Fatal("session detail exposed session credentials")
	}
	reason := map[string]string{"reason": "管理员断开"}
	checkCode(t, connectionRulePost(f.h, f.user, detail+"/revoke", reason), 403, errForbidden.Error())
	checkCode(t, request(f.h, "GET", adminSessionsURL, "", nil, f.admin, ""), 200, `"active":1`)
	checkCode(t, request(f.h, "GET", "/v1/overview", "", nil, f.admin, ""), 200, `"active_sessions":1`)

	outsider := request(f.h, "POST", "/v1/admin/users", testOrigin,
		map[string]string{"username": "othermember", "password": testPassword}, f.admin, csrfToken(f.admin.Value))
	checkCode(t, outsider, 201, "")
	login := loginRequest(f.h, "othermember", testPassword)
	checkCode(t, login, 204, "")
	other := login.Result().Cookies()[0]
	checkCode(t, request(f.h, "GET", detail, "", nil, other, ""), 404, errSessionMissing.Error())
	checkCode(t, request(f.h, "GET", adminSessionsURL, "", nil, other, ""), 200, `"total":0`)
	checkCode(t, connectionRulePost(f.h, other, detail+"/revoke", reason), 404, errSessionMissing.Error())
	info := request(f.h, "GET", "/v1/admin/devices/"+f.target, "", nil, f.admin, "")
	checkCode(t, info, 200, "")
	group := responseValue[device](t, info.Body.Bytes()).GroupID
	checkCode(t, connectionRulePost(f.h, f.admin, "/v1/admin/device-groups/"+group+"/grants",
		deviceGroupGrant{Username: "othermember", Approve: true}), 204, "")
	checkCode(t, request(f.h, "GET", detail, "", nil, other, ""), 200, `"can_revoke":true`)

	start := make(chan struct{})
	renewed, revoked := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	go func() { <-start; renewed <- l.renew(f.h, f.targetToken, 3) }()
	go func() { <-start; revoked <- connectionRulePost(f.h, other, detail+"/revoke", reason) }()
	close(start)
	r := <-renewed
	seq := int64(3)
	if r.Code == 200 {
		checkCode(t, r, 200, `"lease_sequence":4`)
		seq++
	} else {
		checkCode(t, r, 403, errSessionDenied.Error())
	}
	checkCode(t, <-revoked, 200, "revocation_requested")
	checkCode(t, l.renew(restarted, f.targetToken, seq), 403, errSessionDenied.Error())
	checkCode(t, connectionRulePost(f.h, other, detail+"/revoke", reason), 200, "revocation_requested")
	status := l.status(restarted)
	checkCode(t, status, 200, "revocation_requested")
	if strings.Contains(status.Body.String(), reason["reason"]) {
		t.Fatal("runtime status included administrator-only notes")
	}
	f.count(t, 1, "SELECT count(*) FROM audit_events WHERE session_id=$1 AND action='session.revocation_requested' AND receipt_state='pending'", l.session)

	f.sql(t, `UPDATE managed_session_redemptions SET renewed_at=stamp.t-interval '33 seconds',renewed_until=stamp.t-interval '3 seconds'
		FROM (SELECT clock_timestamp() t) stamp WHERE session_id=$1`, l.session)
	checkCode(t, l.status(restarted), 200, `"state":"lease_expired"`)
	checkCode(t, l.stop(f.h, seq+1, "revoked"), 409, errSessionConflict.Error())
	f.sql(t, "ALTER TABLE audit_events ADD CONSTRAINT fixture_stop CHECK(action <> 'session.stopped') NOT VALID")
	checkCode(t, l.stop(f.h, seq, "revoked"), 500, "")
	checkCode(t, l.status(restarted), 200, `"state":"lease_expired"`)
	f.sql(t, "ALTER TABLE audit_events DROP CONSTRAINT fixture_stop")
	checkCode(t, l.stop(restarted, seq, "revoked"), 204, "")
	checkCode(t, l.stop(f.h, seq, "revoked"), 204, "")
	checkCode(t, l.stop(f.h, seq, "local_end"), 409, errSessionConflict.Error())
	stopped := l.status(restarted)
	checkCode(t, stopped, 200, `"state":"stopped"`)
	state := responseValue[sessionActivity](t, stopped.Body.Bytes())
	if state.LeaseExpiredAt == nil || state.StopReceivedAt == nil || state.RevocationAt == nil ||
		!state.StopReceivedAt.After(*state.LeaseExpiredAt) {
		t.Fatal("late stop did not preserve the distinct expiry and revocation facts")
	}
	f.auditCount(t, l.session, "session.stopped", 1)
	f.auditCount(t, l.session, "session.revocation_requested", 1)
	f.count(t, 1, "SELECT count(*) FROM audit_events WHERE session_id=$1 AND action='session.revocation_requested' AND receipt_state='confirmed'", l.session)
}

func TestSessionInvalidationPostgres(t *testing.T) {
	for _, mutation := range []string{"logout", "disable_account", "archive_rule", "shrink_policy", "disable_device"} {
		t.Run(mutation, func(t *testing.T) {
			f := newSessionFixture(t)
			changeTestRule(t, f.h, f.admin, f.rule, "publish")
			f.applyPolicy(t, []string{"view", "input"}, nil)
			l := f.establish(t, f.input(50), 51, "input")
			switch mutation {
			case "logout":
				checkCode(t, connectionRulePost(f.h, f.user, "/v1/auth/logout", nil), 204, "")
			case "disable_account":
				path := "/v1/admin/users/" + f.userID + "/disable"
				reason := map[string]string{"reason": "测试会话撤权"}
				f.sql(t, "ALTER TABLE audit_events ADD CONSTRAINT fixture_revoke CHECK(action <> 'session.revocation_requested') NOT VALID")
				checkCode(t, connectionRulePost(f.h, f.admin, path, reason), 500, "")
				checkCode(t, l.status(f.h), 200, `"state":"active"`)
				f.sql(t, "ALTER TABLE audit_events DROP CONSTRAINT fixture_revoke")
				checkCode(t, connectionRulePost(f.h, f.admin, path, reason), 200, "")
			case "archive_rule":
				changeTestRule(t, f.h, f.admin, f.rule, "archive")
			case "shrink_policy":
				slim := policyFixture(t, f.h, f.admin, "缩小范围", []string{"view"}, nil)
				var assignment string
				if err := f.store.DB.QueryRow(f.ctx, "SELECT id FROM managed_policy_assignments WHERE device_id=$1", f.target).Scan(&assignment); err != nil {
					t.Fatal(err)
				}
				checkCode(t, connectionRulePost(f.h, f.admin, assignmentsURL+"/"+assignment,
					map[string]int64{"revision": 1, "policy_version": slim.Version}), 200, `"revision":2`)
			case "disable_device":
				info := request(f.h, "GET", "/v1/admin/devices/"+f.target, "", nil, f.admin, "")
				checkCode(t, info, 200, "")
				version := responseValue[device](t, info.Body.Bytes()).Version
				checkCode(t, connectionRulePost(f.h, f.admin, "/v1/admin/devices/"+f.target+"/lifecycle/disable", deviceChange{Version: version}), 200, "")
			}
			checkCode(t, l.status(f.h), 200, `"state":"revocation_requested"`)
			want, message := 403, errSessionDenied.Error()
			if mutation == "disable_device" {
				want, message = 401, errManagedIdentity.Error()
			}
			checkCode(t, l.renew(f.h, f.targetToken, 1), want, message)
			checkCode(t, l.stop(f.h, 1, "revoked"), 204, "")
			checkCode(t, l.status(f.h), 200, `"state":"stopped"`)
			f.auditCount(t, l.session, "session.revocation_requested", 1)
			f.auditCount(t, l.session, "session.stopped", 1)
		})
	}
}
