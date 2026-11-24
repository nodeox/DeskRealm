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
	cert := strings.Repeat("b", 64)
	installerVerifier, attester := configureInstallerFixture(t, verifier, cert)
	store.WindowsInstallers = installerVerifier
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

	type deploymentAPIKey struct {
		apiTokenRecord
		Token string `json:"token"`
	}
	const tokenPath = "/v1/admin/api-tokens"
	issueAPIKey := func(group string, cookie *http.Cookie, code int) deploymentAPIKey {
		t.Helper()
		r := request(h, "POST", tokenPath, testOrigin, apiTokenInput{Purpose: "Windows 部署工具",
			Action: "windows.deploy", Scope: "device_group", DeviceGroupID: group, LifetimeMinutes: 30},
			cookie, csrfToken(cookie.Value))
		checkCode(t, r, code, "")
		return responseValue[deploymentAPIKey](t, r.Body.Bytes())
	}
	tool := issueAPIKey(groupID, memberCookie, 201)
	otherGroupTool := issueAPIKey(outside, admin, 201)
	otherSameGroupTool := issueAPIKey(groupID, memberCookie, 201)
	grant(groupID, false)
	issueAPIKey(groupID, memberCookie, 403)
	toolRelease := deploymentReleasePrefix + release
	checkCode(t, nativeRequest(h, "GET", toolRelease, tool.Token, nil), 401, errAPIToken.Error())
	grant(groupID, true)
	checkCode(t, request(h, "GET", toolRelease, "", nil, admin, ""), 401, errAPIToken.Error())
	checkCode(t, nativeRequest(h, "GET", toolRelease, otherGroupTool.Token, nil), 404, errWindowsReleaseMissing.Error())
	checkCode(t, nativeRequest(h, "GET", "/v1/admin/users", tool.Token, nil), 401, errAPIToken.Error())
	readToken := request(h, "POST", tokenPath, testOrigin, apiTokenInput{Purpose: "只读", Action: "users.read",
		Scope: "self", LifetimeMinutes: 30}, admin, csrf)
	checkCode(t, readToken, 201, "")
	checkCode(t, nativeRequest(h, "GET", toolRelease, responseValue[deploymentAPIKey](t, readToken.Body.Bytes()).Token, nil),
		401, errAPIToken.Error())
	automated, automatedKey := newInput("7", "arm64", false)
	automated.Minutes = 60
	checkCode(t, nativeRequest(h, "POST", toolRelease+"/deployments", tool.Token, automated), 404, errWindowsReleaseMissing.Error())
	proofSHA, _, _ := windowsInstallerFixture(t, spool, sha, cert, attester, "automation")
	checkCode(t, request(h, "POST", "/v1/admin/windows-releases/"+release+"/installers", testOrigin,
		map[string]string{"evidence_sha256": proofSHA}, admin, csrf), 201, "")
	checkCode(t, nativeRequest(h, "GET", toolRelease+"/installers/arm64", tool.Token, nil), 200, "arm64 automation")
	checkCode(t, nativeRequest(h, "GET", toolRelease+"/installers/arm64", otherGroupTool.Token, nil), 404, errWindowsReleaseMissing.Error())
	checkCode(t, nativeRequest(h, "POST", toolRelease+"/deployments", otherGroupTool.Token, automated), 404, errWindowsReleaseMissing.Error())
	automated.AutoApprove = true
	checkCode(t, nativeRequest(h, "POST", toolRelease+"/deployments", tool.Token, automated), 403, "forbidden")
	automated.AutoApprove = false
	credential := nativeRequest(h, "POST", toolRelease+"/deployments", tool.Token, automated)
	checkCode(t, credential, 201, "")
	automatedReceipt := responseValue[windowsEnrollmentReceipt](t, credential.Body.Bytes())
	if !automatedReceipt.Expires.Equal(tool.ExpiresAt) || !validToken(automatedReceipt.Credential) {
		t.Fatal("tool credential was not bounded by the API token")
	}
	replay := nativeRequest(h, "POST", toolRelease+"/deployments", tool.Token, automated)
	checkCode(t, replay, 200, "")
	if responseValue[windowsEnrollmentReceipt](t, replay.Body.Bytes()) != automatedReceipt {
		t.Fatal("tool credential replay changed the receipt")
	}
	checkCode(t, nativeRequest(h, "POST", toolRelease+"/deployments", otherSameGroupTool.Token, automated), 409, errEnrollmentConflict.Error())
	toolStatus := toolRelease + "/deployments/" + automated.Deployment
	checkCode(t, nativeRequest(h, "GET", toolStatus, otherSameGroupTool.Token, nil), 404, errWindowsReleaseMissing.Error())
	checkCode(t, nativeRequest(h, "POST", toolStatus+"/revoke", otherSameGroupTool.Token, struct{}{}), 404, errWindowsReleaseMissing.Error())
	deviceInput := enroll(automated, automatedReceipt)
	deviceReceipt := complete(prove(deviceInput, challenge(deviceInput, 200), automatedKey), 200)
	if deviceReceipt.State != "pending" {
		t.Fatal("automated enrollment must await approval")
	}
	status := nativeRequest(h, "GET", toolStatus, tool.Token, nil)
	checkCode(t, status, 200, deviceReceipt.DeviceID)
	if strings.Contains(status.Body.String(), "credential") {
		t.Fatal("automation status exposed the credential")
	}
	checkCode(t, request(h, "POST", "/v1/admin/devices/"+deviceReceipt.DeviceID+"/approve", testOrigin, struct{}{}, admin, csrf), 200, "approved")
	deviceToken := runtimeCredentialFixture(t, h, deviceReceipt.DeviceID, automatedKey)
	devicePolicy := runtimePolicyFixture(t, h, deviceReceipt.DeviceID, deviceToken)
	if devicePolicy == nil || devicePolicy.Version != policy.Version {
		t.Fatal("approved automation device did not receive its group policy")
	}
	checkCode(t, nativeRequest(h, "POST", receiptsURL, deviceToken, signedPolicyFixture(t, devicePolicy, automatedKey, "applied")), 204, "")
	status = nativeRequest(h, "GET", toolStatus, tool.Token, nil)
	checkCode(t, status, 200, "approved")
	applied := responseValue[windowsEnrollmentStatus](t, status.Body.Bytes())
	if applied.ExpectedVersion == nil || applied.AppliedVersion == nil ||
		*applied.ExpectedVersion != policy.Version || *applied.AppliedVersion != policy.Version {
		t.Fatalf("automation status omitted policy receipt: %+v", applied)
	}
	checkCode(t, nativeRequest(h, "POST", toolStatus+"/revoke", tool.Token, struct{}{}), 409, errEnrollmentConflict.Error())

	cancelInput, cancelKey := newInput("8", "x64", false)
	cancel := nativeRequest(h, "POST", toolRelease+"/deployments", tool.Token, cancelInput)
	checkCode(t, cancel, 201, "")
	cancelRequest := enroll(cancelInput, responseValue[windowsEnrollmentReceipt](t, cancel.Body.Bytes()))
	cancelProof := challenge(cancelRequest, 200)
	checkCode(t, nativeRequest(h, "POST", toolRelease+"/deployments/"+cancelInput.Deployment+"/revoke", tool.Token, struct{}{}), 204, "")
	complete(prove(cancelRequest, cancelProof, cancelKey), 401)

	invalidate, invalidateKey := newInput("9", "x64", false)
	invalidation := nativeRequest(h, "POST", toolRelease+"/deployments", tool.Token, invalidate)
	checkCode(t, invalidation, 201, "")
	revokeRequest := enroll(invalidate, responseValue[windowsEnrollmentReceipt](t, invalidation.Body.Bytes()))
	revokeProof := challenge(revokeRequest, 200)
	checkCode(t, request(h, "POST", tokenPath+"/"+tool.ID+"/revoke", testOrigin, struct{}{}, admin, csrf), 204, "")
	checkCode(t, nativeRequest(h, "GET", toolRelease+"/installers/x64", tool.Token, nil), 401, errAPIToken.Error())
	complete(prove(revokeRequest, revokeProof, invalidateKey), 401)
	if status := get(invalidate, admin, 200); status.State != "revoked" {
		t.Fatalf("token revocation was not reflected in deployment status: %+v", status)
	}
	expiringInput, expiringKey := newInput("a", "x64", false)
	expiringTool := nativeRequest(h, "POST", toolRelease+"/deployments", otherSameGroupTool.Token, expiringInput)
	checkCode(t, expiringTool, 201, "")
	expiryRequest := enroll(expiringInput, responseValue[windowsEnrollmentReceipt](t, expiringTool.Body.Bytes()))
	expiryProof := challenge(expiryRequest, 200)
	if _, err := store.DB.Exec(ctx, "UPDATE api_tokens SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1",
		otherSameGroupTool.ID); err != nil {
		t.Fatal(err)
	}
	complete(prove(expiryRequest, expiryProof, expiringKey), 401)

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
