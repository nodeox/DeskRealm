package m1

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func responseValue[T any](t *testing.T, responseBody []byte) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(responseBody, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestManagementScopesPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, adminID := adminSession(t, ctx, store)
	const users = "/v1/admin/users"
	const groups = "/v1/admin/groups"
	const grants = "/v1/admin/role-grants"
	for _, name := range []string{"manager", "viewer", "teamone", "teamtwo", "orgviewer"} {
		r := request(h, "POST", users, testOrigin, map[string]string{"username": name, "password": testPassword}, admin, csrf)
		checkCode(t, r, 201, name)
	}

	groupA := request(h, "POST", groups, testOrigin, map[string]string{"name": "一组"}, admin, csrf)
	groupB := request(h, "POST", groups, testOrigin, map[string]string{"name": "二组"}, admin, csrf)
	checkCode(t, groupA, 201, "")
	checkCode(t, groupB, 201, "")
	one := responseValue[group](t, groupA.Body.Bytes())
	two := responseValue[group](t, groupB.Body.Bytes())

	member := func(g group, name string, remove bool) {
		t.Helper()
		r := request(h, "POST", groups+"/"+g.ID+"/members", testOrigin, map[string]any{"username": name, "remove": remove}, admin, csrf)
		checkCode(t, r, 204, "")
	}
	member(one, "teamone", false)
	member(one, "admin", false)
	member(two, "teamtwo", false)

	grant := func(name, role, scope, groupID string) managementGrant {
		t.Helper()
		r := request(h, "POST", grants, testOrigin, managementGrant{Username: name, Role: role, Scope: scope, GroupID: groupID}, admin, csrf)
		checkCode(t, r, 201, name)
		return responseValue[managementGrant](t, r.Body.Bytes())
	}
	scope := grant("manager", "user_manager", "group", one.ID)
	grant("viewer", "user_viewer", "self", "")
	grant("orgviewer", "user_viewer", "organization", "")

	manager := loginRequest(h, "manager", testPassword).Result().Cookies()[0]
	viewer := loginRequest(h, "viewer", testPassword).Result().Cookies()[0]
	orgViewer := loginRequest(h, "orgviewer", testPassword).Result().Cookies()[0]
	checkCode(t, request(h, "GET", "/v1/overview", "", nil, manager, ""), 200, `"can_read_users":true`)
	checkCode(t, request(h, "GET", users, "", nil, manager, ""), 200, "teamone")
	checkCode(t, request(h, "GET", groups, "", nil, manager, ""), 403, "forbidden")

	find := func(cookie *http.Cookie, name string) managedUser {
		t.Helper()
		r := request(h, "GET", users, "", nil, cookie, "")
		checkCode(t, r, 200, "")
		for _, u := range responseValue[userList](t, r.Body.Bytes()).Users {
			if u.Username == name {
				return u
			}
		}
		t.Fatal("missing authorized user", name)
		return managedUser{}
	}
	first := find(manager, "teamone")
	second := find(admin, "teamtwo")
	if !first.CanChange || find(manager, "admin").CanChange || find(viewer, "viewer").CanChange {
		t.Fatal("incorrect row capabilities")
	}
	for _, pair := range []struct {
		session   *http.Cookie
		forbidden string
	}{{viewer, "teamone"}, {manager, "teamtwo"}} {
		rows := request(h, "GET", users, "", nil, pair.session, "")
		if strings.Contains(rows.Body.String(), pair.forbidden) {
			t.Fatal("out-of-scope user listed")
		}
		checkCode(t, request(h, "GET", users+"/"+second.ID, "", nil, pair.session, ""), 404, "user_not_found")
	}
	checkCode(t, request(h, "GET", users+"/"+second.ID, "", nil, orgViewer, ""), 200, `"can_change":false`)
	checkCode(t, request(h, "GET", users+"/"+first.ID, "", nil, manager, ""), 200, `"can_change":true`)
	payload := map[string]string{"reason": "scope regression"}
	for _, id := range []string{second.ID, adminID} {
		checkCode(t, request(h, "POST", users+"/"+id+"/disable", testOrigin, payload, manager, csrfToken(manager.Value)), 404, "user_not_found")
	}
	checkCode(t, request(h, "POST", users+"/"+first.ID+"/disable", testOrigin, payload, manager, csrfToken(manager.Value)), 200, "disabled")
	checkCode(t, request(h, "POST", grants, testOrigin, scope, manager, csrfToken(manager.Value)), 403, "forbidden")

	member(one, "teamone", true)
	member(two, "teamone", false)
	checkCode(t, request(h, "GET", users+"/"+first.ID, "", nil, manager, ""), 404, "user_not_found")
	checkCode(t, request(h, "POST", grants+"/"+scope.ID+"/revoke", testOrigin, nil, admin, csrf), 204, "")
	checkCode(t, request(h, "GET", users, "", nil, manager, ""), 403, "forbidden")
	checkCode(t, request(h, "GET", "/v1/overview", "", nil, manager, ""), 200, `"can_read_users":false`)
}

func TestPlatformAdminTransferPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, adminID := adminSession(t, ctx, store)
	member := createMember(t, h, admin, csrf)
	memberLogin := loginRequest(h, member.Username, testPassword)
	checkCode(t, memberLogin, 204, "")
	memberSession := memberLogin.Result().Cookies()[0]
	path := "/v1/admin/users/" + member.ID + "/transfer-platform-admin"
	in := map[string]string{"password": "wrong", "reason": "authorized handover"}
	checkCode(t, request(h, "POST", path, testOrigin, in, admin, csrf), 401, "invalid_credentials")
	in["password"] = testPassword
	checkCode(t, request(h, "POST", path, testOrigin, in, memberSession, csrfToken(memberSession.Value)), 403, "forbidden")
	checkCode(t, request(h, "POST", "/v1/admin/users/"+adminID+"/transfer-platform-admin", testOrigin, in, admin, csrf), 409, "invalid_admin_transfer")
	checkCode(t, request(h, "POST", path, testOrigin, in, admin, csrf), 204, "")
	for _, old := range []*http.Cookie{admin, memberSession} {
		checkCode(t, request(h, "GET", "/v1/auth/session", "", nil, old, ""), 401, "session_required")
	}
	newLogin := loginRequest(h, member.Username, testPassword)
	checkCode(t, newLogin, 204, "")
	checkCode(t, request(h, "GET", "/v1/auth/session", "", nil, newLogin.Result().Cookies()[0], ""), 200, "platform_admin")
	oldLogin := loginRequest(h, "admin", testPassword)
	checkCode(t, oldLogin, 204, "")
	checkCode(t, request(h, "GET", "/v1/admin/groups", "", nil, oldLogin.Result().Cookies()[0], ""), 403, "forbidden")
	var count int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events e JOIN outbox o ON o.event_id=e.id
		WHERE e.actor_id=$1 AND e.resource_id=$2 AND e.action='authorization.platform_admin_transferred'`, adminID, member.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("transfer audit and outbox: %d %v", count, err)
	}
}

func TestControlRolesPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, _ := adminSession(t, ctx, store)
	member := createMember(t, h, admin, csrf)
	login := loginRequest(h, member.Username, testPassword)
	checkCode(t, login, 204, "")
	cookie := login.Result().Cookies()[0]
	const path = "/v1/admin/control-roles"
	checkCode(t, request(h, "GET", path, "", nil, cookie, ""), 403, "forbidden")
	input := controlRole{Name: "只读角色", Actions: []string{"view", "view"}}
	checkCode(t, request(h, "POST", path, testOrigin, input, admin, csrf), 400, "invalid_control_role")
	input.Actions = []string{"view"}
	checkCode(t, request(h, "POST", path, testOrigin, input, cookie, csrfToken(cookie.Value)), 403, "forbidden")
	created := request(h, "POST", path, testOrigin, input, admin, csrf)
	checkCode(t, created, 201, `"version":1`)
	role := responseValue[controlRole](t, created.Body.Bytes())
	checkCode(t, request(h, "GET", path, "", nil, admin, ""), 200, role.ID)
	input.Version = 1
	input.Actions = []string{"audio", "view"}
	checkCode(t, request(h, "POST", path+"/"+role.ID, testOrigin, input, admin, csrf), 200, `"version":2`)
	checkCode(t, request(h, "POST", path+"/"+role.ID, testOrigin, input, admin, csrf), 409, "control_role_stale")
}
