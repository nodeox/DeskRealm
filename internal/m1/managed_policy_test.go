package m1

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"deskrealm/internal/managedproto"

	"github.com/jackc/pgx/v5"
)

const (
	policiesURL    = "/v1/admin/strategies"
	assignmentsURL = policiesURL + "/assignments"
	receiptsURL    = "/v1/managed/devices/policy-receipts"
)

func policyFixture(t *testing.T, h http.Handler, admin *http.Cookie, name string, actions, unattended []string) publishedPolicy {
	t.Helper()
	r := connectionRulePost(h, admin, policiesURL, policyContent{Name: name, Actions: actions, Unattended: unattended})
	checkCode(t, r, 201, `"schema_version":1`)
	return responseValue[publishedPolicy](t, r.Body.Bytes())
}

func assignmentFixture(t *testing.T, h http.Handler, admin *http.Cookie, scope, id string, policy int64) policyAssignment {
	t.Helper()
	r := connectionRulePost(h, admin, assignmentsURL, map[string]any{"scope": scope, "object_id": id, "policy_version": policy})
	checkCode(t, r, 201, `"revision":1`)
	return responseValue[policyAssignment](t, r.Body.Bytes())
}

func policyStatusFixture(t *testing.T, h http.Handler, admin *http.Cookie, device string, expected, applied int64, state string) devicePolicyStatus {
	t.Helper()
	r := request(h, "GET", "/v1/admin/devices/"+device+"/policy", "", nil, admin, "")
	checkCode(t, r, 200, state)
	p := responseValue[devicePolicyStatus](t, r.Body.Bytes())
	version := func(value *int64) int64 {
		if value == nil {
			return 0
		}
		return *value
	}
	if version(p.ExpectedVersion) != expected || version(p.AppliedVersion) != applied || p.State != state {
		t.Fatalf("wrong device policy status: %+v", p)
	}
	return p
}

func runtimePolicyFixture(t *testing.T, h http.Handler, device, token string) *policyDelivery {
	t.Helper()
	r := nativeRequest(h, "GET", "/v1/managed/devices/"+device+"/policy", token, nil)
	checkCode(t, r, 200, "")
	if r.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("runtime policy may be cached")
	}
	return responseValue[struct {
		Policy *policyDelivery `json:"policy"`
	}](t, r.Body.Bytes()).Policy
}

func TestManagedPolicyAssignmentPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	_, admin, csrf, _ := adminSession(t, ctx, store)
	_, signing := managedTestKey(t)
	h := managedHandler(t, store, signing)
	member := createMember(t, h, admin, csrf)
	login := loginRequest(h, member.Username, testPassword)
	checkCode(t, login, 204, "")
	memberCookie := login.Result().Cookies()[0]
	host, _ := runtimeFixture(t, h, admin, csrf, "10600001", true)
	info := request(h, "GET", "/v1/admin/devices/"+host, "", nil, admin, "")
	group := responseValue[device](t, info.Body.Bytes()).GroupID
	policyStatusFixture(t, h, admin, host, 0, 0, "unassigned")
	checkCode(t, request(h, "GET", policiesURL, "", nil, memberCookie, ""), 403, "forbidden")
	checkCode(t, connectionRulePost(h, memberCookie, policiesURL, policyContent{Name: "拒绝非法发布", Actions: []string{"view"}}), 403, "forbidden")
	checkCode(t, connectionRulePost(h, admin, policiesURL, policyContent{Name: "无操作字段"}), 400, errPolicyInput.Error())
	checkCode(t, connectionRulePost(h, admin, policiesURL, policyContent{Name: "重复用途", Actions: []string{"view"}, Unattended: []string{"helpdesk", "helpdesk"}}), 400, errPolicyInput.Error())

	defaultPolicy := policyFixture(t, h, admin, "组织默认", []string{"view"}, nil)
	groupPolicy := policyFixture(t, h, admin, "组查看", []string{"view", "audio"}, nil)
	userPolicy := policyFixture(t, h, admin, "归属用户", []string{"view", "clipboard"}, nil)
	hostPolicy := policyFixture(t, h, admin, "获准无人值守设备", []string{"input", "view"}, []string{"helpdesk"})
	expectedHash := sha256.Sum256([]byte("DR-M2-policy-v1\x00\x03helpdesk\x00"))
	if hostPolicy.SHA256 != hex.EncodeToString(expectedHash[:]) {
		t.Fatal("policy digest differs from canonical bytes")
	}
	checkCode(t, request(h, "POST", policiesURL, testOrigin, policyContent{Name: "缺少来源", Actions: []string{"view"}}, admin, ""), 403, "csrf_denied")
	checkCode(t, connectionRulePost(h, admin, assignmentsURL,
		map[string]any{"scope": "organization", "policy_version": hostPolicy.Version}), 400, errPolicyInput.Error())
	checkCode(t, connectionRulePost(h, admin, assignmentsURL,
		map[string]any{"scope": "organization", "policy_version": hostPolicy.Version + 999}), 400, errPolicyInput.Error())
	assignmentFixture(t, h, admin, "organization", "", defaultPolicy.Version)
	policyStatusFixture(t, h, admin, host, defaultPolicy.Version, 0, "pending")
	other, _ := runtimeFixture(t, h, admin, csrf, "10600002", true)
	policyStatusFixture(t, h, admin, other, defaultPolicy.Version, 0, "pending")

	north := assignmentFixture(t, h, admin, "group", group, groupPolicy.Version)
	policyStatusFixture(t, h, admin, host, groupPolicy.Version, 0, "pending")
	assignmentFixture(t, h, admin, "user", member.ID, userPolicy.Version)
	before := policyStatusFixture(t, h, admin, host, groupPolicy.Version, 0, "pending")
	ownerURL := "/v1/admin/devices/" + host + "/policy-owner"
	requestOwner := map[string]any{"owner_user_id": member.ID, "device_version": before.DeviceVersion}
	checkCode(t, connectionRulePost(h, memberCookie, ownerURL, requestOwner), 403, "forbidden")
	checkCode(t, connectionRulePost(h, admin, ownerURL, requestOwner), 200, member.ID)
	owned := policyStatusFixture(t, h, admin, host, userPolicy.Version, 0, "pending")
	if owned.Owner != member.ID {
		t.Fatalf("owner change was not persisted: %+v", owned)
	}
	checkCode(t, connectionRulePost(h, admin, ownerURL, requestOwner), 409, errDeviceVersion.Error())
	specific := assignmentFixture(t, h, admin, "device", host, hostPolicy.Version)
	policyStatusFixture(t, h, admin, host, hostPolicy.Version, 0, "pending")
	clear := connectionRulePost(h, admin, assignmentsURL+"/"+specific.ID, map[string]any{"revision": 1, "policy_version": nil})
	checkCode(t, clear, 200, `"revision":2`)
	owned = policyStatusFixture(t, h, admin, host, userPolicy.Version, 0, "pending")
	checkCode(t, connectionRulePost(h, admin, ownerURL, map[string]any{"owner_user_id": "", "device_version": owned.DeviceVersion}), 200, "pending")
	policyStatusFixture(t, h, admin, host, groupPolicy.Version, 0, "pending")

	otherInfo := request(h, "GET", "/v1/admin/devices/"+other, "", nil, admin, "")
	destination := responseValue[device](t, otherInfo.Body.Bytes()).GroupID
	active := policyStatusFixture(t, h, admin, host, groupPolicy.Version, 0, "pending")
	moveURL := "/v1/admin/devices/" + host + "/lifecycle/move"
	checkCode(t, connectionRulePost(h, admin, moveURL, deviceChange{GroupID: destination, Version: active.DeviceVersion}), 200, destination)
	policyStatusFixture(t, h, admin, host, defaultPolicy.Version, 0, "pending")

	// Bind fixed bucket representatives to this actual assignment ID. These
	// independent fixture devices make both sides of the 50% split deterministic.
	cohort := map[string]bool{}
	seenBuckets := map[bool]bool{}
	for n := byte(1); len(cohort) < 2 && n < 100; n++ {
		raw := [16]byte{0x81, 0x27, 0, 1, 0, 0, 0x40, 0, 0x80, 0, 0, 0, 0, 0, 0, n}
		sum := sha256.Sum256(append(managedFrameUUID(north.ID), raw[:]...))
		picked := binary.BigEndian.Uint32(sum[:4])%100 < 50
		if seenBuckets[picked] {
			continue
		}
		seenBuckets[picked] = true
		id := hex.EncodeToString(raw[:])
		id = id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
		public, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB.Exec(ctx, `INSERT INTO devices(id,organization_id,group_id,platform,rustdesk_id,public_key,registration_state)
			SELECT $1,organization_id,id,'windows-amd64',$3,$4,'approved' FROM device_groups WHERE id=$2`,
			id, group, "cohort-"+id[len(id)-4:], public); err != nil {
			t.Fatal(err)
		}
		cohort[id] = picked
	}
	if len(cohort) != 2 {
		t.Fatal("could not construct two deterministic cohort buckets")
	}
	constraints := map[string]any{"revision": 1, "policy_version": groupPolicy.Version, "canary_version": userPolicy.Version, "canary_percent": 50}
	assignmentPath := assignmentsURL + "/" + north.ID
	replies := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { replies <- connectionRulePost(h, admin, assignmentPath, constraints) }()
	}
	success := 0
	for range 2 {
		r := <-replies
		if r.Code == 200 {
			success++
		} else {
			checkCode(t, r, 409, errPolicyStale.Error())
		}
	}
	if success != 1 {
		t.Fatal("assignment must have a single optimistic winner")
	}
	current := map[string]devicePolicyStatus{}
	for id, picked := range cohort {
		want := groupPolicy.Version
		if picked {
			want = userPolicy.Version
		}
		current[id] = policyStatusFixture(t, h, admin, id, want, 0, "pending")
	}
	constraints["revision"] = 2
	checkCode(t, connectionRulePost(h, admin, assignmentPath, constraints), 200, `"revision":3`)
	for id, prior := range current {
		stable := policyStatusFixture(t, h, admin, id, *prior.ExpectedVersion, 0, "pending")
		if stable.DeliveryID != prior.DeliveryID {
			t.Fatal("same canary inputs changed the delivery")
		}
	}
	rollback := connectionRulePost(h, admin, assignmentPath+"/rollback",
		map[string]int64{"revision": 3, "rollback_to_revision": 1})
	checkCode(t, rollback, 200, `"rollback_from_revision":3`)
	for id, picked := range cohort {
		now := policyStatusFixture(t, h, admin, id, groupPolicy.Version, 0, "pending")
		if (now.DeliveryID != current[id].DeliveryID) != picked {
			t.Fatal("rollback changed incorrect cohort targets")
		}
	}
	checkCode(t, request(h, "GET", assignmentPath+"?revision=2", "", nil, admin, ""), 200, `"canary_percent":50`)
	checkCode(t, request(h, "GET", assignmentPath, "", nil, admin, ""), 200, `"rollback_to_revision":1`)
	checkCode(t, request(h, "GET", policiesURL, "", nil, admin, ""), 200, hostPolicy.SHA256)
}

func signedPolicyFixture(t *testing.T, p *policyDelivery, key ed25519.PrivateKey, result string, fields ...string) policyReceipt {
	t.Helper()
	var state, mask byte
	switch result {
	case "applied":
		state = 1
	case "failed":
		state = 2
	}
	for _, field := range fields {
		switch field {
		case "allowed_actions":
			mask |= 1
		case "unattended_purposes":
			mask |= 2
		case "schema_version":
			mask |= 4
		}
	}
	sum, err := hex.DecodeString(p.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	message, err := managedproto.Encode(managedproto.PolicyReceipt, managedFrameUUID(p.Organization),
		managedFrameUUID(p.Device), managedFrameUUID(p.ID), binary.BigEndian.AppendUint64(nil, uint64(p.Version)),
		sum, binary.BigEndian.AppendUint64(nil, uint64(p.KeyEpoch)), []byte{state}, []byte{mask})
	if err != nil {
		t.Fatal(err)
	}
	return policyReceipt{Device: p.Device, Delivery: p.ID, Version: p.Version, SHA256: p.SHA256, Result: result,
		Failed: fields, Signed: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, message))}
}

func TestManagedPolicyReceiptsPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	_, admin, csrf, _ := adminSession(t, ctx, store)
	_, signing := managedTestKey(t)
	h := managedHandler(t, store, signing)
	first, firstKey := runtimeFixture(t, h, admin, csrf, "10601001", true)
	second, secondKey := runtimeFixture(t, h, admin, csrf, "10601002", true)
	firstToken := runtimeCredentialFixture(t, h, first, firstKey)
	secondToken := runtimeCredentialFixture(t, h, second, secondKey)
	if p := runtimePolicyFixture(t, h, first, firstToken); p != nil {
		t.Fatal("unassigned device received policy")
	}
	primary := policyFixture(t, h, admin, "基线", []string{"view"}, nil)
	upgrade := policyFixture(t, h, admin, "升级", []string{"view", "input"}, nil)
	specific := policyFixture(t, h, admin, "无人值守", []string{"view", "input"}, []string{"ops"})
	org := assignmentFixture(t, h, admin, "organization", "", primary.Version)
	firstPolicy := runtimePolicyFixture(t, h, first, firstToken)
	secondPolicy := runtimePolicyFixture(t, h, second, secondToken)
	if firstPolicy == nil || secondPolicy == nil || firstPolicy.Version != primary.Version || firstPolicy.ID == secondPolicy.ID {
		t.Fatal("delivery did not bind each registered device")
	}
	post := func(token string, receipt policyReceipt, status int, code string) {
		t.Helper()
		checkCode(t, nativeRequest(h, "POST", receiptsURL, token, receipt), status, code)
	}
	effective := func(id, purpose string, applicable, unattended bool, version int64, actions ...string) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, store.DB, func(tx pgx.Tx) error {
			result, ok, err := effectiveManagedPolicy(ctx, tx, firstPolicy.Organization, id, purpose)
			if err == nil && (applicable != ok || result.Unattended != unattended || result.Version != version ||
				!slices.Equal(actions, result.Actions)) {
				t.Errorf("unexpected capabilities for %s: %+v ok=%v", id, result, ok)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	effective(first, "ops", false, false, 0)
	firstApplied := signedPolicyFixture(t, firstPolicy, firstKey, "applied")
	post(secondToken, firstApplied, 401, errManagedIdentity.Error())
	post(firstToken, signedPolicyFixture(t, firstPolicy, secondKey, "applied"), 401, errManagedIdentity.Error())
	checkCode(t, nativeRequest(h, "GET", "/v1/managed/devices/"+first+"/policy", secondToken, nil), 401, errManagedIdentity.Error())
	checkCode(t, request(h, "POST", receiptsURL, testOrigin, firstApplied, admin, csrf), 403, "origin_denied")
	wrongVersion := firstApplied
	wrongVersion.Version += 999
	post(firstToken, wrongVersion, 409, errPolicyStale.Error())
	wrongHash := firstApplied
	wrongHash.SHA256 = upgrade.SHA256
	post(firstToken, wrongHash, 409, errPolicyStale.Error())

	if _, err := store.DB.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT policy_test_audit_failure
		CHECK (action <> 'device.policy_reported') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	post(firstToken, firstApplied, 500, "service_error")
	policyStatusFixture(t, h, admin, first, primary.Version, 0, "pending")
	effective(first, "ops", false, false, 0)
	if _, err := store.DB.Exec(ctx, "ALTER TABLE audit_events DROP CONSTRAINT policy_test_audit_failure"); err != nil {
		t.Fatal(err)
	}
	post(firstToken, firstApplied, 204, "")
	post(firstToken, firstApplied, 204, "")
	post(firstToken, signedPolicyFixture(t, firstPolicy, firstKey, "failed", "allowed_actions"), 409, errPolicyStale.Error())
	effective(first, "ops", true, false, primary.Version, "view")
	post(secondToken, signedPolicyFixture(t, secondPolicy, secondKey, "applied"), 204, "")

	path := assignmentsURL + "/" + org.ID
	checkCode(t, connectionRulePost(h, admin, path, map[string]int64{"revision": 1, "policy_version": upgrade.Version}), 200, `"revision":2`)
	upFirst := runtimePolicyFixture(t, h, first, firstToken)
	upSecond := runtimePolicyFixture(t, h, second, secondToken)
	policyStatusFixture(t, h, admin, first, upgrade.Version, primary.Version, "pending")
	effective(first, "ops", false, false, 0)
	post(firstToken, firstApplied, 409, errPolicyStale.Error())
	failed := signedPolicyFixture(t, upFirst, firstKey, "failed", "allowed_actions", "schema_version")
	post(firstToken, failed, 204, "")
	post(firstToken, failed, 204, "")
	post(secondToken, signedPolicyFixture(t, upSecond, secondKey, "applied"), 204, "")
	status := policyStatusFixture(t, h, admin, first, upgrade.Version, primary.Version, "failed")
	if !slices.Equal(status.FailedFields, []string{"allowed_actions", "schema_version"}) || status.ReceivedAt == nil {
		t.Fatalf("missing structured failure details: %+v", status)
	}
	policyStatusFixture(t, h, admin, second, upgrade.Version, upgrade.Version, "applied")
	effective(first, "ops", false, false, 0)

	retryURL := "/v1/admin/devices/" + first + "/policy-retry"
	retry := map[string]string{"delivery_id": upFirst.ID}
	checkCode(t, connectionRulePost(h, admin, retryURL, retry), 200, "pending")
	checkCode(t, connectionRulePost(h, admin, retryURL, retry), 409, errPolicyStale.Error())
	renewed := runtimePolicyFixture(t, h, first, firstToken)
	if renewed.ID == upFirst.ID || renewed.Version != upgrade.Version || runtimePolicyFixture(t, h, second, secondToken).ID != upSecond.ID {
		t.Fatal("retry changed the wrong delivery")
	}
	post(firstToken, failed, 409, errPolicyStale.Error())
	post(firstToken, signedPolicyFixture(t, renewed, firstKey, "applied"), 204, "")
	effective(first, "ops", true, false, upgrade.Version, "input", "view")

	checkCode(t, connectionRulePost(h, admin, path+"/rollback", map[string]int64{"revision": 2, "rollback_to_revision": 1}),
		200, `"rollback_to_revision":1`)
	rolledBack := runtimePolicyFixture(t, h, first, firstToken)
	policyStatusFixture(t, h, admin, first, primary.Version, upgrade.Version, "pending")
	if rolledBack.ID == firstPolicy.ID {
		t.Fatal("rollback reused an earlier generation")
	}
	post(firstToken, firstApplied, 409, errPolicyStale.Error())
	post(firstToken, signedPolicyFixture(t, rolledBack, firstKey, "applied"), 204, "")
	policyStatusFixture(t, h, admin, first, primary.Version, primary.Version, "applied")

	assignmentFixture(t, h, admin, "device", first, specific.Version)
	unattended := runtimePolicyFixture(t, h, first, firstToken)
	effective(first, "ops", false, false, 0)
	unattendedReceipt := signedPolicyFixture(t, unattended, firstKey, "applied")
	post(firstToken, unattendedReceipt, 204, "")
	effective(first, "ops", true, true, specific.Version, "input", "view")
	effective(first, "other", true, false, specific.Version, "input", "view")

	stable := request(h, "GET", "/v1/admin/devices/"+first, "", nil, admin, "")
	checkCode(t, stable, 200, "approved")
	if got := responseValue[device](t, stable.Body.Bytes()); got.ExpectedPolicy == nil || got.AppliedPolicy == nil ||
		*got.ExpectedPolicy != specific.Version || *got.AppliedPolicy != specific.Version {
		t.Fatalf("M1 device detail is out of sync: %+v", got)
	}
	rotation := rotationFixture(t, h, admin, csrf, responseValue[device](t, stable.Body.Bytes()), false)
	_, newKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	checkCode(t, request(h, "POST", "/v1/enrollment/rotate", "", signRotation(t, rotation, newKey), nil, ""), 200, "approved")
	effective(first, "ops", false, false, 0)
	post(firstToken, unattendedReceipt, 401, errManagedIdentity.Error())
	freshToken := runtimeCredentialFixture(t, h, first, newKey)
	fresh := runtimePolicyFixture(t, h, first, freshToken)
	if fresh.ID == unattended.ID || fresh.KeyEpoch <= unattended.KeyEpoch || fresh.Version != unattended.Version {
		t.Fatal("rotation failed to renew delivery with current key epoch")
	}
	post(freshToken, unattendedReceipt, 409, errPolicyStale.Error())
	post(freshToken, signedPolicyFixture(t, fresh, newKey, "applied"), 204, "")
	effective(first, "ops", true, true, specific.Version, "input", "view")

	checkCode(t, connectionRulePost(h, admin, path, map[string]any{"revision": 3, "policy_version": nil}), 200, `"revision":4`)
	policyStatusFixture(t, h, admin, second, 0, upgrade.Version, "unassigned")
	if runtimePolicyFixture(t, h, second, secondToken) != nil {
		t.Fatal("cleared assignment still exposes a policy")
	}
	effective(second, "ops", false, false, 0)
	post(secondToken, signedPolicyFixture(t, upSecond, secondKey, "applied"), 409, errPolicyStale.Error())
	effective(first, "ops", true, true, specific.Version, "input", "view")

	var count int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events e JOIN outbox o ON o.event_id=e.id
		WHERE e.action='device.policy_reported' AND e.reason=$1 AND e.resource_id=$2`, firstPolicy.ID, first).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected one original receipt audit, got %d: %v", count, err)
	}
}
