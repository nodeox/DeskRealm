package m1

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"filippo.io/edwards25519"
)

func TestDevicePublicKey(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.StdEncoding.EncodeToString
	if _, ok := devicePublicKey(encode(pub)); !ok {
		t.Fatal("rejected normal Ed25519 key")
	}
	torsion, err := new(edwards25519.Point).SetBytes(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	point, err := new(edwards25519.Point).SetBytes(pub)
	if err != nil {
		t.Fatal(err)
	}
	mixed := new(edwards25519.Point).Add(point, torsion)
	for _, key := range [][]byte{edwards25519.NewIdentityPoint().Bytes(), torsion.Bytes(), mixed.Bytes()} {
		if _, ok := devicePublicKey(encode(key)); ok {
			t.Fatal("accepted key outside nonzero prime-order subgroup")
		}
	}
}

func approvedDevice(t *testing.T, h http.Handler, admin *http.Cookie, csrf, groupID, rustdeskID string) (device, enrollmentRequest) {
	t.Helper()
	token := credentialFixture(t, h, admin, csrf, groupID)
	in, _ := deviceProofFixture(t, h, groupID, token, rustdeskID)
	result := request(h, "POST", "/v1/enrollment/complete", "", in, nil, "")
	checkCode(t, result, 200, "pending")
	id := responseValue[enrollmentResult](t, result.Body.Bytes()).DeviceID
	result = request(h, "POST", "/v1/admin/devices/"+id+"/approve", testOrigin, struct{}{}, admin, csrf)
	checkCode(t, result, 200, "approved")
	return responseValue[device](t, result.Body.Bytes()), in
}

func rotationFixture(t *testing.T, h http.Handler, who *http.Cookie, csrf string, d device, restore bool) rotationGrant {
	t.Helper()
	result := request(h, "POST", "/v1/admin/devices/"+d.ID+"/key-rotation", testOrigin,
		map[string]any{"version": d.Version, "restore": restore}, who, csrf)
	checkCode(t, result, 201, "credential")
	out := responseValue[rotationGrant](t, result.Body.Bytes())
	if out.Version != d.Version || out.DeviceID != d.ID || !validToken(out.Nonce) || !validToken(out.Credential) || out.ExpiresAt.IsZero() {
		t.Fatal("invalid rotation grant")
	}
	return out
}

func signRotation(t *testing.T, grant rotationGrant, priv ed25519.PrivateKey) rotationRequest {
	t.Helper()
	if priv == nil {
		_, generated, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		priv = generated
	}
	in := rotationRequest{DeviceID: grant.DeviceID, Version: grant.Version, Credential: grant.Credential,
		PublicKey: base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))}
	in.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, rotationProof(in, grant.Nonce)))
	return in
}

func TestDeviceLifecyclePostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, _ := adminSession(t, ctx, store)
	createMember(t, h, admin, csrf)
	a := deviceGroupFixture(t, h, admin, csrf, "来源")
	b := deviceGroupFixture(t, h, admin, csrf, "目标")
	c := deviceGroupFixture(t, h, admin, csrf, "限制")
	current, oldEnrollment := approvedDevice(t, h, admin, csrf, a.ID, "20001001")
	other, _ := approvedDevice(t, h, admin, csrf, b.ID, "20001002")
	set := func(groupID string, approve bool) {
		t.Helper()
		out := request(h, "POST", "/v1/admin/device-groups/"+groupID+"/grants", testOrigin,
			deviceGroupGrant{Username: "member", Approve: approve}, admin, csrf)
		checkCode(t, out, 204, "")
	}
	set(a.ID, true)
	set(b.ID, false)
	login := loginRequest(h, "member", testPassword)
	checkCode(t, login, 204, "")
	member := login.Result().Cookies()[0]
	memberCSRF := csrfToken(member.Value)
	const prefix = "/v1/admin/devices/"
	path := prefix + current.ID
	change := func(action string, in deviceChange) *httptest.ResponseRecorder {
		return request(h, "POST", path+"/lifecycle/"+action, testOrigin, in, member, memberCSRF)
	}
	checkCode(t, change("move", deviceChange{Version: current.Version, GroupID: b.ID}), 404, "device_group_not_found")
	set(b.ID, true)
	changed := change("move", deviceChange{Version: current.Version, GroupID: b.ID})
	checkCode(t, changed, 200, b.ID)
	moved := responseValue[device](t, changed.Body.Bytes())
	if moved.ID != current.ID || moved.Version != current.Version+1 {
		t.Fatal("move changed stable identity or lost revision")
	}
	retry := change("move", deviceChange{Version: current.Version, GroupID: b.ID})
	checkCode(t, retry, 200, b.ID)
	if retry.Body.String() != changed.Body.String() {
		t.Fatal("retry changed move receipt")
	}
	set(a.ID, false)
	checkCode(t, change("move", deviceChange{Version: moved.Version, GroupID: c.ID}), 404, "device_group_not_found")
	checkCode(t, change("rename-id", deviceChange{Version: moved.Version, RustDeskID: other.RustDeskID}), 409, "device_conflict")

	replies := make(chan *httptest.ResponseRecorder, 2)
	for _, id := range []string{"20001003", "20001004"} {
		go func(id string) { replies <- change("rename-id", deviceChange{Version: moved.Version, RustDeskID: id}) }(id)
	}
	first, second := <-replies, <-replies
	if first.Code != 200 {
		first, second = second, first
	}
	checkCode(t, first, 200, "")
	checkCode(t, second, 409, "device_version_conflict")
	renamed := responseValue[device](t, first.Body.Bytes())
	history := request(h, "GET", path+"/history", "", nil, member, "")
	checkCode(t, history, 200, renamed.RustDeskID)
	checkCode(t, history, 200, current.RustDeskID)

	obsolete := rotationFixture(t, h, member, memberCSRF, renamed, false)
	grant := rotationFixture(t, h, member, memberCSRF, renamed, false)
	checkCode(t, request(h, "POST", "/v1/enrollment/rotate", "", signRotation(t, obsolete, nil), nil, ""), 401, "invalid_device_rotation")
	rotate := signRotation(t, grant, nil)
	bad := rotate
	bad.Version++
	checkCode(t, request(h, "POST", "/v1/enrollment/rotate", "", bad, nil, ""), 401, "invalid_device_rotation")
	set(b.ID, false)
	checkCode(t, request(h, "POST", "/v1/enrollment/rotate", "", rotate, nil, ""), 401, "invalid_device_rotation")
	set(b.ID, true)
	checkCode(t, request(h, "POST", "/v1/enrollment/rotate", "", rotate, nil, ""), 401, "invalid_device_rotation")
	rotate = signRotation(t, rotationFixture(t, h, member, memberCSRF, renamed, false), nil)
	rotations := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			rotations <- request(h, "POST", "/v1/enrollment/rotate", "", rotate, nil, "")
		}()
	}
	for range 2 {
		response := <-rotations
		checkCode(t, response, 200, "approved")
		got := responseValue[rotationReceipt](t, response.Body.Bytes())
		if got.DeviceID != current.ID || got.Version != renamed.Version+1 {
			t.Fatal("rotation changed identity or repeated a revision")
		}
	}
	checkCode(t, request(h, "POST", "/v1/enrollment/complete", "", oldEnrollment, nil, ""), 401, "invalid_enrollment")

	var currentKeys, revokedKeys, events int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE revoked_at IS NULL),
		count(*) FILTER (WHERE revoked_at IS NOT NULL) FROM device_key_history WHERE device_id=$1`, current.ID).Scan(&currentKeys, &revokedKeys); err != nil ||
		currentKeys != 1 || revokedKeys != 1 {
		t.Fatalf("key history active=%d revoked=%d err=%v", currentKeys, revokedKeys, err)
	}
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events a JOIN outbox o ON o.event_id=a.id
		WHERE a.resource_id=$1 AND a.action IN ('device.moved','device.rustdesk_id_changed','device.key_rotated')`, current.ID).Scan(&events); err != nil ||
		events != 3 {
		t.Fatalf("duplicate or missing audited changes: %d %v", events, err)
	}
}

func TestDeviceStateIsolationPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, _ := adminSession(t, ctx, store)
	group := deviceGroupFixture(t, h, admin, csrf, "维护组")
	current, oldEnrollment := approvedDevice(t, h, admin, csrf, group.ID, "20001005")
	issued := rotationFixture(t, h, admin, csrf, current, false)
	const prefix = "/v1/admin/devices/"
	change := func(d device, action string) device {
		t.Helper()
		r := request(h, "POST", prefix+d.ID+"/lifecycle/"+action, testOrigin, deviceChange{Version: d.Version}, admin, csrf)
		checkCode(t, r, 200, "")
		return responseValue[device](t, r.Body.Bytes())
	}
	if _, err := store.DB.Exec(ctx, `UPDATE devices SET connection_state='offline',expected_policy_version=4,
		applied_policy_version=3 WHERE id=$1`, current.ID); err != nil {
		t.Fatal(err)
	}
	disabled := change(current, "disable")
	if disabled.RegistrationState != "disabled" || disabled.ConnectionState != "offline" ||
		disabled.ExpectedPolicy == nil || disabled.AppliedPolicy == nil || *disabled.ExpectedPolicy != 4 || *disabled.AppliedPolicy != 3 {
		t.Fatal("disable changed independent connection or policy observations")
	}
	retried := change(current, "disable")
	if retried.Version != disabled.Version {
		t.Fatal("retry of disable wrote again")
	}
	checkCode(t, request(h, "POST", "/v1/enrollment/rotate", "", signRotation(t, issued, nil), nil, ""), 401, "invalid_device_rotation")
	checkCode(t, request(h, "POST", "/v1/enrollment/complete", "", oldEnrollment, nil, ""), 401, "invalid_enrollment")
	checkCode(t, request(h, "POST", prefix+current.ID+"/key-rotation", testOrigin, map[string]any{"version": disabled.Version, "restore": false}, admin, csrf),
		409, "device_lifecycle_conflict")
	restore := rotationFixture(t, h, admin, csrf, disabled, true)
	stale := rotationFixture(t, h, admin, csrf, disabled, true)
	if _, err := store.DB.Exec(ctx, "UPDATE device_key_rotations SET expires_at=now()-interval '1 minute' WHERE digest=$1", digest(stale.Credential)); err != nil {
		t.Fatal(err)
	}
	checkCode(t, request(h, "POST", "/v1/enrollment/rotate", "", signRotation(t, restore, nil), nil, ""), 401, "invalid_device_rotation")
	checkCode(t, request(h, "POST", "/v1/enrollment/rotate", "", signRotation(t, stale, nil), nil, ""), 401, "invalid_device_rotation")
	restore = rotationFixture(t, h, admin, csrf, disabled, true)
	newProof := signRotation(t, restore, nil)
	response := request(h, "POST", "/v1/enrollment/rotate", "", newProof, nil, "")
	checkCode(t, response, 200, "approved")
	result := responseValue[rotationReceipt](t, response.Body.Bytes())
	disabled.Version = result.Version
	retired := change(disabled, "retire")
	if retired.RegistrationState != "retired" || retired.ConnectionState != "offline" || retired.ExpectedPolicy == nil ||
		*retired.ExpectedPolicy != 4 {
		t.Fatal("retirement changed observations")
	}
	checkCode(t, request(h, "POST", "/v1/enrollment/rotate", "", newProof, nil, ""), 401, "invalid_device_rotation")
	checkCode(t, request(h, "POST", prefix+current.ID+"/key-rotation", testOrigin, map[string]any{"version": retired.Version, "restore": true}, admin, csrf),
		409, "device_lifecycle_conflict")
	res := request(h, "GET", prefix+current.ID+"/history", "", nil, admin, "")
	checkCode(t, res, 200, current.RustDeskID)
	events := responseValue[deviceHistory](t, res.Body.Bytes())
	if len(events.Keys) != 2 {
		t.Fatal("unexpected key history size")
	}
	for _, key := range events.Keys {
		if key.RevokedAt == nil || len(key.Fingerprint) != 64 || strings.Contains(res.Body.String(), newProof.PublicKey) {
			t.Fatal("retirement key history incorrect")
		}
	}
	var decision string
	if err := store.DB.QueryRow(ctx, "SELECT decision FROM registration_approvals WHERE device_id=$1", current.ID).Scan(&decision); err != nil || decision != "approved" {
		t.Fatalf("retirement lost decision history: %s %v", decision, err)
	}
	fresh := credentialFixture(t, h, admin, csrf, group.ID)
	oldEnrollment.Credential, oldEnrollment.AttemptID, oldEnrollment.Signature = fresh, "", ""
	ch := request(h, "POST", "/v1/enrollment/challenge", "", oldEnrollment, nil, "")
	checkCode(t, ch, 409, "device_conflict")
}
