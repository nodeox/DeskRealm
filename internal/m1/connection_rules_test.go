package m1

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const accessRulesURL = "/v1/admin/access-rules"

func connectionRulePost(h http.Handler, cookie *http.Cookie, path string, body any) *httptest.ResponseRecorder {
	return request(h, "POST", path, testOrigin, body, cookie, csrfToken(cookie.Value))
}

func createTestControlRole(t *testing.T, h http.Handler, admin *http.Cookie, actions ...string) controlRole {
	t.Helper()
	res := connectionRulePost(h, admin, "/v1/admin/control-roles", controlRole{Name: "会话控制", Actions: actions})
	checkCode(t, res, 201, "")
	return responseValue[controlRole](t, res.Body.Bytes())
}

func makeRule(t *testing.T, h http.Handler, admin *http.Cookie, spec connectionRuleSpec) connectionRule {
	t.Helper()
	response := connectionRulePost(h, admin, accessRulesURL, spec)
	checkCode(t, response, 201, `"published_version":null`)
	return responseValue[connectionRule](t, response.Body.Bytes())
}

func changeTestRule(t *testing.T, h http.Handler, admin *http.Cookie, rule connectionRule, action string) {
	t.Helper()
	checkCode(t, connectionRulePost(h, admin, accessRulesURL+"/"+rule.ID+"/"+action,
		map[string]int64{"version": rule.Version}), 204, "")
}

func previewTestRule(t *testing.T, h http.Handler, admin *http.Cookie, in connectionRequest, reason string) connectionDecision {
	t.Helper()
	res := connectionRulePost(h, admin, accessRulesURL+"/preview", in)
	checkCode(t, res, 200, reason)
	result := responseValue[connectionDecision](t, res.Body.Bytes())
	if result.Reason != reason || result.Allowed != (reason == "matched_allow") {
		t.Fatalf("unexpected decision: %+v", result)
	}
	return result
}

func TestConnectionRuleEvaluationPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	_, admin, csrf, adminID := adminSession(t, ctx, store)
	_, key := managedTestKey(t)
	h := managedHandler(t, store, key)
	member := createMember(t, h, admin, csrf)
	login := loginRequest(h, member.Username, testPassword)
	checkCode(t, login, 204, "")
	memberCookie := login.Result().Cookies()[0]

	controller, controllerKey := runtimeFixture(t, h, admin, csrf, "10500001", true)
	controllerCredential := runtimeCredentialFixture(t, h, controller, controllerKey)
	target, targetKey := runtimeFixture(t, h, admin, csrf, "10500002", true)
	targetCredential := runtimeCredentialFixture(t, h, target, targetKey)
	outside, outsideKey := runtimeFixture(t, h, admin, csrf, "10500003", true)
	outsideCredential := runtimeCredentialFixture(t, h, outside, outsideKey)
	pending, _ := runtimeFixture(t, h, admin, csrf, "10500004", false)
	heart := func(id, credential string) {
		t.Helper()
		checkCode(t, nativeRequest(h, "POST", deviceHeartbeatURL, credential,
			map[string]string{"device_id": id, "reported_build": "1.5.3-fixture"}), 204, "")
	}
	heart(controller, controllerCredential)
	heart(target, targetCredential)
	heart(outside, outsideCredential)
	targetInfo := request(h, "GET", "/v1/admin/devices/"+target, "", nil, admin, "")
	checkCode(t, targetInfo, 200, `"connection_state":"online"`)
	targetGroup := responseValue[device](t, targetInfo.Body.Bytes()).GroupID
	groupReply := connectionRulePost(h, admin, "/v1/admin/groups", map[string]string{"name": "允许值班"})
	checkCode(t, groupReply, 201, "")
	userGroup := responseValue[group](t, groupReply.Body.Bytes()).ID
	memberPath := "/v1/admin/groups/" + userGroup + "/members"
	checkCode(t, connectionRulePost(h, admin, memberPath, map[string]string{"username": member.Username}), 204, "")

	in := connectionRequest{User: member.ID, Controller: controller, Target: target, Purpose: "helpdesk", Actions: []string{"view"}}
	checkCode(t, request(h, "POST", accessRulesURL+"/preview", testOrigin, in, nil, ""), 401, errSession.Error())
	checkCode(t, connectionRulePost(h, memberCookie, accessRulesURL+"/preview", in), 403, errForbidden.Error())
	checkCode(t, request(h, "GET", accessRulesURL, "", nil, memberCookie, ""), 403, errForbidden.Error())
	checkCode(t, request(h, "POST", accessRulesURL+"/preview", testOrigin, in, admin, ""), 403, "csrf_denied")
	malformed := in
	malformed.RustDeskID = "10500002"
	checkCode(t, connectionRulePost(h, admin, accessRulesURL+"/preview", malformed), 400, errConnectionInput.Error())
	previewTestRule(t, h, admin, in, "no_matching_allow")
	role := createTestControlRole(t, h, admin, "view", "input")
	spec := connectionRuleSpec{Name: "本组运维", SourceGroup: userGroup, Controller: controller, TargetGroup: targetGroup,
		Purpose: in.Purpose, Effect: "allow", Role: role.ID, RoleVersion: role.Version, Actions: []string{"view"}}
	first := makeRule(t, h, admin, spec)
	previewTestRule(t, h, admin, in, "no_matching_allow")
	changeTestRule(t, h, admin, first, "publish")
	decision := previewTestRule(t, h, admin, in, "matched_allow")
	if decision.Target != target || decision.Rule != first.ID || decision.RuleVersion != 1 || decision.Role != role.ID {
		t.Fatalf("wrong authorization trace: %+v", decision)
	}
	spec.Actions = []string{"input"}
	second := makeRule(t, h, admin, spec)
	changeTestRule(t, h, admin, second, "publish")
	in.Actions = []string{"view", "input"}
	previewTestRule(t, h, admin, in, "no_matching_allow")
	spec.Actions = []string{"view", "input"}
	full := makeRule(t, h, admin, spec)
	changeTestRule(t, h, admin, full, "publish")
	previewTestRule(t, h, admin, in, "matched_allow")

	denial := makeRule(t, h, admin, connectionRuleSpec{Name: "明确禁止", SourceUser: member.ID, Target: target, Purpose: in.Purpose, Effect: "deny"})
	changeTestRule(t, h, admin, denial, "publish")
	decision = previewTestRule(t, h, admin, in, "explicit_deny")
	if decision.Rule != denial.ID || decision.Target != "" || len(decision.Actions) != 0 {
		t.Fatalf("deny did not override or exposed allowed capabilities: %+v", decision)
	}
	changeTestRule(t, h, admin, denial, "archive")
	in.Actions = []string{"view"}
	copy := in
	copy.User = adminID
	previewTestRule(t, h, admin, copy, "no_matching_allow")
	copy = in
	copy.Target = outside
	previewTestRule(t, h, admin, copy, "no_matching_allow")
	copy.Target = pending
	previewTestRule(t, h, admin, copy, "endpoint_unavailable")
	copy = in
	copy.Controller = outside
	previewTestRule(t, h, admin, copy, "no_matching_allow")
	copy = in
	copy.Purpose = "other"
	previewTestRule(t, h, admin, copy, "no_matching_allow")
	copy = in
	copy.Target, copy.RustDeskID = "", "10500002"
	previewTestRule(t, h, admin, copy, "matched_allow")

	if _, err := store.DB.Exec(ctx, `UPDATE devices SET managed_heartbeat_protocol=2 WHERE id=$1`, target); err != nil {
		t.Fatal(err)
	}
	previewTestRule(t, h, admin, in, "endpoint_unavailable")
	heart(target, targetCredential)
	if _, err := store.DB.Exec(ctx, `UPDATE devices SET managed_heartbeat_at=statement_timestamp()-interval '1 minute'
		WHERE id=$1`, controller); err != nil {
		t.Fatal(err)
	}
	previewTestRule(t, h, admin, in, "endpoint_unavailable")
	heart(controller, controllerCredential)

	bookReply := connectionRulePost(h, admin, "/v1/address-books", map[string]string{"name": "运维通讯录"})
	checkCode(t, bookReply, 201, "")
	book := responseValue[addressBook](t, bookReply.Body.Bytes()).ID
	bookPath := "/v1/address-books/" + book
	entryReply := connectionRulePost(h, admin, bookPath+"/entries", entryInput{DeviceID: target, Alias: "前台"})
	checkCode(t, entryReply, 201, "")
	entry := responseValue[addressEntry](t, entryReply.Body.Bytes()).ID
	bookRequest := in
	bookRequest.Target, bookRequest.Book, bookRequest.Entry = "", book, entry
	previewTestRule(t, h, admin, bookRequest, "address_book_unavailable")
	sharePath := bookPath + "/shares"
	checkCode(t, connectionRulePost(h, admin, sharePath,
		map[string]any{"kind": "user", "name": member.Username, "permission": bookView}), 204, "")
	previewTestRule(t, h, admin, bookRequest, "address_book_unavailable")
	deviceGrantPath := "/v1/admin/device-groups/" + targetGroup + "/grants"
	checkCode(t, connectionRulePost(h, admin, deviceGrantPath, deviceGroupGrant{Username: member.Username}), 204, "")
	previewTestRule(t, h, admin, bookRequest, "matched_allow")
	shares := request(h, "GET", sharePath, "", nil, admin, "")
	checkCode(t, shares, 200, member.Username)
	share := responseValue[grantPage](t, shares.Body.Bytes()).Shares[0]
	checkCode(t, connectionRulePost(h, admin, sharePath+"/"+share.ID+"/revoke", struct{}{}), 204, "")
	previewTestRule(t, h, admin, bookRequest, "address_book_unavailable")
	previewTestRule(t, h, admin, in, "matched_allow")
	checkCode(t, connectionRulePost(h, admin, memberPath,
		map[string]any{"username": member.Username, "remove": true}), 204, "")
	previewTestRule(t, h, admin, in, "no_matching_allow")

	checkCode(t, connectionRulePost(h, admin, "/v1/admin/users/"+member.ID+"/disable",
		map[string]string{"reason": "验证实时撤权"}), 200, "disabled")
	previewTestRule(t, h, admin, in, "subject_inactive")

	var allowed, denied int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE e.result='allowed'),
	count(*) FILTER (WHERE e.result='denied') FROM audit_events e JOIN outbox o ON e.id=o.event_id
	WHERE e.action='connection_rule.previewed' AND e.actor_id=$1`, adminID).Scan(&allowed, &denied); err != nil || allowed == 0 || denied == 0 {
		t.Fatalf("preview and outbox audit missing: %d %d %v", allowed, denied, err)
	}
}

func TestConnectionRuleRevisionsPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	_, admin, csrf, adminID := adminSession(t, ctx, store)
	_, key := managedTestKey(t)
	h := managedHandler(t, store, key)
	controller, controllerKey := runtimeFixture(t, h, admin, csrf, "10500101", true)
	target, targetKey := runtimeFixture(t, h, admin, csrf, "10500102", true)
	for _, pair := range []struct {
		id  string
		key []byte
	}{{controller, controllerKey}, {target, targetKey}} {
		cred := runtimeCredentialFixture(t, h, pair.id, pair.key)
		checkCode(t, nativeRequest(h, "POST", deviceHeartbeatURL, cred,
			map[string]string{"device_id": pair.id, "reported_build": "managed-policy-fixture"}), 204, "")
	}
	role := createTestControlRole(t, h, admin, "view")
	base := connectionRuleSpec{Name: "允许发起", SourceUser: adminID, Target: target, Purpose: "service",
		Effect: "allow", Role: role.ID, RoleVersion: 1, Actions: []string{"view"}}
	bad := base
	bad.Actions = []string{"view", "input"}
	checkCode(t, connectionRulePost(h, admin, accessRulesURL, bad), 400, errConnectionInput.Error())
	bad = base
	bad.RoleVersion = 99
	checkCode(t, connectionRulePost(h, admin, accessRulesURL, bad), 409, errControlRoleVersion.Error())
	bad = base
	bad.Target = "00000000-0000-0000-0000-000000000010"
	checkCode(t, connectionRulePost(h, admin, accessRulesURL, bad), 400, errConnectionInput.Error())
	list := request(h, "GET", accessRulesURL, "", nil, admin, "")
	checkCode(t, list, 200, `"rules":[]`)

	rule := makeRule(t, h, admin, base)
	path := accessRulesURL + "/" + rule.ID
	change := func(action string, version int64) *httptest.ResponseRecorder {
		return connectionRulePost(h, admin, path+"/"+action, map[string]int64{"version": version})
	}
	edit := func(spec connectionRuleSpec, version int64) *httptest.ResponseRecorder {
		return connectionRulePost(h, admin, path, struct {
			connectionRuleSpec
			Version int64 `json:"version"`
		}{spec, version})
	}
	in := connectionRequest{User: adminID, Controller: controller, Target: target, Purpose: "service", Actions: []string{"view"}}

	if _, err := store.DB.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT rule_test_audit_reject
		CHECK (action <> 'connection_rule.published') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	checkCode(t, change("publish", 1), 500, "service_error")
	checkCode(t, request(h, "GET", path, "", nil, admin, ""), 200, `"published_version":null`)
	previewTestRule(t, h, admin, in, "no_matching_allow")
	if _, err := store.DB.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT rule_test_audit_reject`); err != nil {
		t.Fatal(err)
	}
	checkCode(t, change("publish", 1), 204, "")
	checkCode(t, change("publish", 1), 204, "")
	want := previewTestRule(t, h, admin, in, "matched_allow")
	var org string
	if err := store.DB.QueryRow(ctx, "SELECT organization_id FROM users WHERE id=$1", adminID).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, store.DB, func(tx pgx.Tx) error {
		got, err := evaluateConnectionRules(ctx, tx, org, in)
		if err == nil && (!got.Allowed || got.Rule != want.Rule || got.RuleVersion != want.RuleVersion || got.Target != want.Target) {
			t.Errorf("transaction evaluator differs from HTTP preview: %+v", got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	results := make(chan *httptest.ResponseRecorder, 2)
	for _, name := range []string{"值班窗口甲", "值班窗口乙"} {
		go func() {
			next := base
			next.Name = name
			next.Purpose = "service-v2"
			results <- edit(next, 1)
		}()
	}
	won := 0
	for range 2 {
		res := <-results
		if res.Code == 200 {
			won++
			checkCode(t, res, 200, `"version":2`)
		} else {
			checkCode(t, res, 409, errConnectionStale.Error())
		}
	}
	if won != 1 {
		t.Fatal("concurrent edits did not produce exactly one winner")
	}
	old := request(h, "GET", path+"?version=1", "", nil, admin, "")
	checkCode(t, old, 200, base.Name)
	oldRule := responseValue[connectionRule](t, old.Body.Bytes())
	if oldRule.Version != 1 || oldRule.Latest != 2 || oldRule.Published == nil || *oldRule.Published != 1 {
		t.Fatalf("unexpected version history: %+v", oldRule)
	}
	previewTestRule(t, h, admin, in, "matched_allow")
	nextInput := in
	nextInput.Purpose = "service-v2"
	previewTestRule(t, h, admin, nextInput, "no_matching_allow")
	checkCode(t, change("publish", 1), 409, errConnectionStale.Error())
	checkCode(t, change("archive", 1), 409, errConnectionStale.Error())
	checkCode(t, change("publish", 2), 204, "")
	previewTestRule(t, h, admin, in, "no_matching_allow")
	previewTestRule(t, h, admin, nextInput, "matched_allow")

	changedRole := controlRole{Name: role.Name, Actions: []string{"input"}, Version: role.Version}
	checkCode(t, connectionRulePost(h, admin, "/v1/admin/control-roles/"+role.ID, changedRole), 200, `"version":2`)
	previewTestRule(t, h, admin, nextInput, "no_matching_allow")
	checkCode(t, change("publish", 2), 409, errControlRoleVersion.Error())
	checkCode(t, change("archive", 2), 204, "")
	checkCode(t, request(h, "GET", path, "", nil, admin, ""), 404, errConnectionMissing.Error())
	out := request(h, "GET", accessRulesURL, "", nil, admin, "")
	checkCode(t, out, 200, `"rules":[]`)
	if strings.Contains(out.Body.String(), rule.ID) {
		t.Fatal("archived rule exposed in live list")
	}
	var count int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events e JOIN outbox o ON e.id=o.event_id
		WHERE e.resource_id=$1 AND e.action='connection_rule.published'`, rule.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("wrong publish audit count %d: %v", count, err)
	}
}
