package m1

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"deskrealm/internal/windowsdelivery"
)

func TestWindowsEnrollmentPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	spool, center, verifier, private := windowsFixtureVerifier(t)
	store.WindowsDelivery = verifier
	var err error
	store.WindowsReleases, err = verifier.ConfigureReleases(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, admin, csrf, _ := adminSession(t, ctx, store)
	keyPath, signer := managedTestKey(t)
	h := managedHandler(t, store, signer)
	member := createMember(t, h, admin, csrf)
	login := loginRequest(h, member.Username, testPassword)
	checkCode(t, login, 204, "")
	memberCookie := login.Result().Cookies()[0]
	group := func(name string) string {
		t.Helper()
		r := request(h, "POST", "/v1/admin/device-groups", testOrigin, map[string]string{"name": name}, admin, csrf)
		checkCode(t, r, 201, "")
		return responseValue[deviceGroup](t, r.Body.Bytes()).ID
	}
	groupID, outside := group("部署目标组"), group("发布范围之外")
	grant := func(id string, approve bool) {
		t.Helper()
		checkCode(t, request(h, "POST", "/v1/admin/device-groups/"+id+"/grants", testOrigin,
			deviceGroupGrant{Username: member.Username, Approve: approve}, admin, csrf), 204, "")
	}
	grant(groupID, false)
	grant(outside, true)
	sha := windowsFixtureStage(t, spool, windowsdelivery.Approval{
		Schema: 1, Version: "0.2.0-dev.1", Center: center, Groups: []string{groupID},
		Brand:  windowsdelivery.Brand{ID: "deskrealm", Revision: 1, Name: "端域"},
		Source: "1234567890abcdef1234567890abcdef12345678",
	}, private)
	response := request(h, "POST", tasksPath+"/windows-delivery", testOrigin,
		map[string]string{"request_key": "scoped-enrollment", "manifest_sha256": sha}, admin, csrf)
	checkCode(t, response, 202, "")
	task := responseValue[struct{ ID string }](t, response.Body.Bytes()).ID
	claim, worked, err := store.claimTaskTarget(ctx, time.Minute)
	if err != nil || !worked || claim.Task != task {
		t.Fatalf("cannot claim candidate: %+v %v", claim, err)
	}
	if err := store.runWindowsDelivery(ctx, claim); err != nil {
		t.Fatal(err)
	}
	response = request(h, "POST", tasksPath+"/"+task+"/windows-release", testOrigin, struct{}{}, admin, csrf)
	checkCode(t, response, 201, "")
	release := responseValue[windowsRelease](t, response.Body.Bytes()).ID
	deployments := "/v1/windows-releases/" + release + "/deployments"
	policy := policyFixture(t, h, admin, "Windows 部署基线", []string{"view"}, nil)
	assignmentFixture(t, h, admin, "group", groupID, policy.Version)

	newInput := func(suffix, arch string, auto bool) (windowsEnrollmentInput, ed25519.PrivateKey) {
		t.Helper()
		public, private, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		return windowsEnrollmentInput{Deployment: "28210a40-8a61-4cf0-ab1c-7d50cbc0c00" + suffix, Group: groupID,
			Arch: arch, RustDeskID: "1072100" + suffix, PublicKey: base64.StdEncoding.EncodeToString(public),
			Purpose: "批量部署", Minutes: 10, AutoApprove: auto}, private
	}
	issue := func(input windowsEnrollmentInput, cookie *http.Cookie, code int) windowsEnrollmentReceipt {
		t.Helper()
		r := request(h, "POST", deployments, testOrigin, input, cookie, csrfToken(cookie.Value))
		checkCode(t, r, code, "")
		if r.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("deployment response may be cached")
		}
		return responseValue[windowsEnrollmentReceipt](t, r.Body.Bytes())
	}
	challenge := func(in enrollmentRequest, code int) enrollmentChallenge {
		t.Helper()
		r := nativeRequest(h, "POST", "/v1/enrollment/challenge", "", in)
		checkCode(t, r, code, "")
		return responseValue[enrollmentChallenge](t, r.Body.Bytes())
	}
	enroll := func(in windowsEnrollmentInput, receipt windowsEnrollmentReceipt) enrollmentRequest {
		return enrollmentRequest{Credential: receipt.Credential, GroupID: in.Group, Platform: receipt.Platform,
			RustDeskID: in.RustDeskID, PublicKey: in.PublicKey}
	}
	prove := func(in enrollmentRequest, proof enrollmentChallenge, key ed25519.PrivateKey) enrollmentRequest {
		in.AttemptID = proof.ID
		in.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, enrollmentProof(proof.ID, proof.Nonce, in)))
		return in
	}
	complete := func(in enrollmentRequest, code int) enrollmentResult {
		t.Helper()
		r := nativeRequest(h, "POST", "/v1/enrollment/complete", "", in)
		checkCode(t, r, code, "")
		return responseValue[enrollmentResult](t, r.Body.Bytes())
	}
	get := func(in windowsEnrollmentInput, cookie *http.Cookie, code int) windowsEnrollmentStatus {
		t.Helper()
		r := request(h, "GET", deployments+"/"+in.Deployment, "", nil, cookie, "")
		checkCode(t, r, code, "")
		if strings.Contains(r.Body.String(), `"credential"`) {
			t.Fatal("status included a credential")
		}
		return responseValue[windowsEnrollmentStatus](t, r.Body.Bytes())
	}

	pending, pendingKey := newInput("1", "x64", false)
	checkCode(t, request(h, "POST", deployments, testOrigin, pending, admin, ""), 403, "")
	issue(pending, memberCookie, 404)
	unpublished := pending
	unpublished.Group = outside
	issue(unpublished, admin, 404)
	grant(groupID, true)
	auto, autoKey := newInput("2", "arm64", true)
	issue(auto, memberCookie, 403)
	first := issue(pending, memberCookie, 201)
	if first.State != "issued" || !validToken(first.Credential) {
		t.Fatal("deployment credential not issued")
	}
	reloaded, err := LoadManagedSigningKey(keyPath, "test-2026")
	if err != nil {
		t.Fatal(err)
	}
	h = managedHandler(t, store, reloaded)
	if replay := issue(pending, memberCookie, 200); replay != first {
		t.Fatal("restart changed deployment receipt")
	}
	substituted := pending
	substituted.PublicKey = auto.PublicKey
	issue(substituted, memberCookie, 409)
	requestPending := enroll(pending, first)
	for _, mutate := range []func(*enrollmentRequest){
		func(in *enrollmentRequest) { in.PublicKey = auto.PublicKey },
		func(in *enrollmentRequest) { in.RustDeskID = auto.RustDeskID },
		func(in *enrollmentRequest) { in.Platform = "windows-arm64" },
		func(in *enrollmentRequest) { in.GroupID = outside },
	} {
		incorrect := requestPending
		mutate(&incorrect)
		challenge(incorrect, 401)
	}
	proof := challenge(requestPending, 200)
	complete(prove(requestPending, proof, autoKey), 401)
	submitted := prove(requestPending, proof, pendingKey)
	registered := complete(submitted, 200)
	if registered.State != "pending" || registered.DeviceID == "" || !userIDPattern.MatchString(registered.Organization) {
		t.Fatalf("unexpected manual enrollment: %+v", registered)
	}
	if status := get(pending, memberCookie, 200); status.Registration != "pending" ||
		status.Device != registered.DeviceID || status.ExpectedVersion == nil || *status.ExpectedVersion != policy.Version ||
		status.AppliedVersion != nil {
		t.Fatalf("unexpected pending status: %+v", status)
	}
	checkCode(t, request(h, "POST", "/v1/admin/devices/"+registered.DeviceID+"/approve", testOrigin, struct{}{},
		memberCookie, csrfToken(memberCookie.Value)), 200, "approved")
	if replay := complete(submitted, 200); replay != registered {
		t.Fatalf("manual approval changed initial receipt: %+v", replay)
	}
	if replay := issue(pending, memberCookie, 200); replay.State != "consumed" || replay.Credential != "" {
		t.Fatal("consumed credential was disclosed")
	}

	autoReceipt := issue(auto, admin, 201)
	requestAuto := enroll(auto, autoReceipt)
	requestAuto = prove(requestAuto, challenge(requestAuto, 200), autoKey)
	approved := complete(requestAuto, 200)
	if approved.State != "approved" || approved.DeviceID == "" || complete(requestAuto, 200) != approved {
		t.Fatalf("automatic enrollment did not produce an approved receipt: %+v", approved)
	}
	runtime := runtimeCredentialFixture(t, h, approved.DeviceID, autoKey)
	delivery := runtimePolicyFixture(t, h, approved.DeviceID, runtime)
	if delivery == nil || delivery.Version != policy.Version {
		t.Fatal("registered device did not receive expected policy")
	}
	checkCode(t, nativeRequest(h, "POST", receiptsURL, runtime,
		signedPolicyFixture(t, delivery, autoKey, "applied")), 204, "")
	if status := get(auto, admin, 200); status.Registration != "approved" ||
		status.AppliedVersion == nil || *status.AppliedVersion != policy.Version {
		t.Fatalf("signed policy application missing: %+v", status)
	}

	revoked, revokedKey := newInput("3", "x64", false)
	requestRevoked := enroll(revoked, issue(revoked, memberCookie, 201))
	revokedProof := challenge(requestRevoked, 200)
	revokePath := deployments + "/" + revoked.Deployment + "/revoke"
	for range 2 {
		checkCode(t, request(h, "POST", revokePath, testOrigin, struct{}{}, admin, csrf), 204, "")
	}
	complete(prove(requestRevoked, revokedProof, revokedKey), 401)
	if status := get(revoked, admin, 200); status.State != "revoked" {
		t.Fatalf("revocation missing: %+v", status)
	}
	lostGrant, lostGrantKey := newInput("6", "x64", false)
	requestLostGrant := enroll(lostGrant, issue(lostGrant, memberCookie, 201))
	lostGrantProof := challenge(requestLostGrant, 200)
	grant(groupID, false)
	complete(prove(requestLostGrant, lostGrantProof, lostGrantKey), 401)
	get(lostGrant, memberCookie, 404)
	grant(groupID, true)
	expired, _ := newInput("4", "arm64", false)
	expiring := issue(expired, admin, 201)
	if _, err := store.DB.Exec(ctx, `UPDATE enrollment_credentials SET expires_at=clock_timestamp()-interval '1 second'
		WHERE id=$1`, expiring.ID); err != nil {
		t.Fatal(err)
	}
	challenge(enroll(expired, expiring), 401)
	if replay := issue(expired, admin, 200); replay.State != "expired" || replay.Credential != "" {
		t.Fatal("expired credential was disclosed")
	}
	withdrawn, withdrawnKey := newInput("5", "x64", false)
	requestWithdrawn := enroll(withdrawn, issue(withdrawn, admin, 201))
	withdrawnProof := challenge(requestWithdrawn, 200)
	checkCode(t, request(h, "POST", "/v1/admin/windows-releases/"+release+"/revoke", testOrigin, struct{}{}, admin, csrf), 204, "")
	complete(prove(requestWithdrawn, withdrawnProof, withdrawnKey), 401)
	issue(withdrawn, admin, 404)
	if status := get(withdrawn, admin, 200); status.State != "revoked" {
		t.Fatalf("release withdrawal missing: %+v", status)
	}
	grant(groupID, false)
	get(auto, memberCookie, 404)
}
