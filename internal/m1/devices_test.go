package m1

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func deviceGroupFixture(t *testing.T, h http.Handler, admin *http.Cookie, csrf, name string) deviceGroup {
	t.Helper()
	res := request(h, "POST", "/v1/admin/device-groups", testOrigin, map[string]string{"name": name}, admin, csrf)
	checkCode(t, res, 201, name)
	return responseValue[deviceGroup](t, res.Body.Bytes())
}

func credentialFixture(t *testing.T, h http.Handler, who *http.Cookie, csrf, groupID string) string {
	t.Helper()
	res := request(h, "POST", "/v1/admin/enrollment-credentials", testOrigin, map[string]any{
		"group_id": groupID, "platform": "windows-amd64", "purpose": "enroll test", "lifetime_minutes": 30,
	}, who, csrf)
	checkCode(t, res, 201, "credential")
	value := responseValue[struct{ Credential string }](t, res.Body.Bytes()).Credential
	if !validToken(value) {
		t.Fatal("invalid credential")
	}
	return value
}

func deviceProofFixture(t *testing.T, h http.Handler, groupID, credential, rustdeskID string) (enrollmentRequest, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	in := enrollmentRequest{Credential: credential, GroupID: groupID, Platform: "windows-amd64", RustDeskID: rustdeskID,
		PublicKey: base64.StdEncoding.EncodeToString(pub)}
	res := request(h, "POST", "/v1/enrollment/challenge", "", in, nil, "")
	checkCode(t, res, 200, "nonce")
	challenge := responseValue[enrollmentChallenge](t, res.Body.Bytes())
	in.AttemptID = challenge.ID
	in.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, enrollmentProof(challenge.ID, challenge.Nonce, in)))
	return in, priv
}

func TestDeviceEnrollmentPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, _ := adminSession(t, ctx, store)
	a := deviceGroupFixture(t, h, admin, csrf, "一组")
	b := deviceGroupFixture(t, h, admin, csrf, "二组")
	credential := credentialFixture(t, h, admin, csrf, a.ID)
	in, priv := deviceProofFixture(t, h, a.ID, credential, "10001001")
	again := in
	again.AttemptID, again.Signature = "", ""
	res := request(h, "POST", "/v1/enrollment/challenge", "", again, nil, "")
	checkCode(t, res, 200, in.AttemptID)
	challenge := responseValue[enrollmentChallenge](t, res.Body.Bytes())
	if challenge.ExpiresAt.IsZero() {
		t.Fatal("missing expiry")
	}
	for _, delta := range []func(*enrollmentRequest){
		func(v *enrollmentRequest) { v.GroupID = b.ID },
		func(v *enrollmentRequest) { v.Platform = "windows-arm64" },
	} {
		modified := again
		delta(&modified)
		checkCode(t, request(h, "POST", "/v1/enrollment/challenge", "", modified, nil, ""), 401, "invalid_enrollment")
	}
	otherKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rival := again
	rival.PublicKey = base64.StdEncoding.EncodeToString(otherKey)
	checkCode(t, request(h, "POST", "/v1/enrollment/challenge", "", rival, nil, ""), 409, "enrollment_conflict")
	bad := in
	bad.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte("different challenge")))
	checkCode(t, request(h, "POST", "/v1/enrollment/complete", "", bad, nil, ""), 401, "invalid_enrollment")
	bad = in
	bad.RustDeskID = "10001002"
	checkCode(t, request(h, "POST", "/v1/enrollment/complete", "", bad, nil, ""), 409, "enrollment_conflict")
	var consumed bool
	if err := store.DB.QueryRow(ctx, "SELECT consumed_at IS NOT NULL FROM enrollment_credentials WHERE digest=$1", digest(credential)).Scan(&consumed); err != nil || consumed {
		t.Fatalf("incorrectly consumed credential: %v %v", consumed, err)
	}
	replies := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { replies <- request(h, "POST", "/v1/enrollment/complete", "", in, nil, "") }()
	}
	first, second := <-replies, <-replies
	checkCode(t, first, 200, "pending")
	checkCode(t, second, 200, "pending")
	if first.Body.String() != second.Body.String() {
		t.Fatal("same request changed device identity")
	}
	got := responseValue[enrollmentResult](t, first.Body.Bytes())
	var count int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events a JOIN outbox o ON o.event_id=a.id
		WHERE a.resource_id=$1 AND a.action='device.enrollment_pending'`, got.DeviceID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("enrollment audit: %d %v", count, err)
	}
	expired := credentialFixture(t, h, admin, csrf, a.ID)
	late, _ := deviceProofFixture(t, h, a.ID, expired, "10001003")
	if _, err := store.DB.Exec(ctx, "UPDATE enrollment_credentials SET expires_at=now()-interval '1 minute' WHERE digest=$1", digest(expired)); err != nil {
		t.Fatal(err)
	}
	checkCode(t, request(h, "POST", "/v1/enrollment/complete", "", late, nil, ""), 401, "invalid_enrollment")
}

func TestDeviceApprovalPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, _ := adminSession(t, ctx, store)
	createMember(t, h, admin, csrf)
	a := deviceGroupFixture(t, h, admin, csrf, "允许组")
	b := deviceGroupFixture(t, h, admin, csrf, "其他组")
	set := func(groupID string, approve bool) {
		checkCode(t, request(h, "POST", "/v1/admin/device-groups/"+groupID+"/grants", testOrigin, deviceGroupGrant{Username: "member", Approve: approve}, admin, csrf), 204, "")
	}
	set(a.ID, false)
	login := loginRequest(h, "member", testPassword)
	checkCode(t, login, 204, "")
	member := login.Result().Cookies()[0]
	memberCSRF := csrfToken(member.Value)
	makeDevice := func(groupID, rustdeskID string) string {
		credential := credentialFixture(t, h, admin, csrf, groupID)
		in, _ := deviceProofFixture(t, h, groupID, credential, rustdeskID)
		r := request(h, "POST", "/v1/enrollment/complete", "", in, nil, "")
		checkCode(t, r, 200, "pending")
		return responseValue[enrollmentResult](t, r.Body.Bytes()).DeviceID
	}
	first, second := makeDevice(a.ID, "10001004"), makeDevice(b.ID, "10001005")
	const base = "/v1/admin/devices"
	res := request(h, "GET", base, "", nil, member, "")
	checkCode(t, res, 200, first)
	if strings.Contains(res.Body.String(), second) {
		t.Fatal("cross-group device listed")
	}
	checkCode(t, request(h, "GET", base+"/"+second, "", nil, member, ""), 404, "device_not_found")
	checkCode(t, request(h, "POST", base+"/"+first+"/approve", testOrigin, struct{}{}, member, memberCSRF), 404, "device_not_found")
	set(a.ID, true)
	issued := credentialFixture(t, h, member, memberCSRF, a.ID)
	waiting, _ := deviceProofFixture(t, h, a.ID, issued, "10001006")
	set(a.ID, false)
	checkCode(t, request(h, "POST", "/v1/enrollment/complete", "", waiting, nil, ""), 401, "invalid_enrollment")
	set(a.ID, true)
	if _, err := store.DB.Exec(ctx, `UPDATE devices SET expected_policy_version=3,applied_policy_version=2,
		connection_state='offline' WHERE id=$1`, first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec(ctx, "ALTER TABLE audit_events ADD CONSTRAINT refuse_approval CHECK (action<>'device.approved')"); err != nil {
		t.Fatal(err)
	}
	path := base + "/" + first + "/approve"
	checkCode(t, request(h, "POST", path, testOrigin, struct{}{}, member, memberCSRF), 500, "service_error")
	checkCode(t, request(h, "GET", base+"/"+first, "", nil, member, ""), 200, "pending")
	if _, err := store.DB.Exec(ctx, "ALTER TABLE audit_events DROP CONSTRAINT refuse_approval"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		approved := request(h, "POST", path, testOrigin, struct{}{}, member, memberCSRF)
		checkCode(t, approved, 200, "approved")
		got := responseValue[device](t, approved.Body.Bytes())
		if got.ConnectionState != "offline" || got.ExpectedPolicy == nil || got.AppliedPolicy == nil || *got.ExpectedPolicy != 3 || *got.AppliedPolicy != 2 {
			t.Fatal("approval modified independent connection/policy states")
		}
	}
	checkCode(t, request(h, "POST", base+"/"+first+"/reject", testOrigin, struct{}{}, member, memberCSRF), 409, "device_decision_conflict")
	checkCode(t, request(h, "POST", base+"/"+second+"/approve", testOrigin, struct{}{}, member, memberCSRF), 404, "device_not_found")
	var count int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events a JOIN outbox o ON o.event_id=a.id
		WHERE a.resource_id=$1 AND a.action='device.approved'`, first).Scan(&count); err != nil || count != 1 {
		t.Fatalf("approval audit: %d %v", count, err)
	}
}
