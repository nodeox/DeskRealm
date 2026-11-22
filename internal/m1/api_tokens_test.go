package m1

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type issuedAPIToken struct {
	apiTokenRecord
	Token string `json:"token"`
}

func apiCall(h http.Handler, method, path, token string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, testOrigin+path, nil)
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Authorization", "Bearer "+token)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, r)
	return response
}

func TestAPITokenScopesPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, adminID := adminSession(t, ctx, store)
	member := createMember(t, h, admin, csrf)
	groups := "/v1/admin/groups"
	newGroup := request(h, "POST", groups, testOrigin, map[string]string{"name": "Team A"}, admin, csrf)
	checkCode(t, newGroup, 201, "")
	g := responseValue[group](t, newGroup.Body.Bytes())
	checkCode(t, request(h, "POST", groups+"/"+g.ID+"/members", testOrigin,
		map[string]string{"username": member.Username}, admin, csrf), 204, "")
	tokens := "/v1/admin/api-tokens"
	const users = "/v1/admin/users"
	issue := func(p apiTokenInput, cookie *http.Cookie) issuedAPIToken {
		t.Helper()
		res := request(h, "POST", tokens, testOrigin, p, cookie, csrfToken(cookie.Value))
		checkCode(t, res, 201, "")
		value := responseValue[issuedAPIToken](t, res.Body.Bytes())
		if !validToken(value.Token) || value.ID == "" {
			t.Fatal("invalid API token issuance")
		}
		return value
	}
	input := apiTokenInput{Purpose: "nightly inventory", Action: "users.read", Scope: "group", GroupID: g.ID, LifetimeMinutes: 60}
	scoped := issue(input, admin)
	checkCode(t, apiCall(h, "GET", users+"/"+member.ID, scoped.Token, nil), 200, `"can_change":false`)
	checkCode(t, apiCall(h, "GET", users+"/"+adminID, scoped.Token, admin), 404, "user_not_found")
	checkCode(t, apiCall(h, "POST", users+"/"+member.ID+"/disable", scoped.Token, admin), 401, "session_required")
	checkCode(t, apiCall(h, "GET", tokens, scoped.Token, admin), 401, "session_required")
	checkCode(t, apiCall(h, "GET", users, "invalid", admin), 401, "invalid_api_token")
	list := request(h, "GET", tokens, "", nil, admin, "")
	checkCode(t, list, 200, scoped.ID)
	if strings.Contains(list.Body.String(), scoped.Token) {
		t.Fatal("token revealed more than once")
	}
	expired := issue(input, admin)
	if _, err := store.DB.Exec(ctx, "UPDATE api_tokens SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", expired.ID); err != nil {
		t.Fatal(err)
	}
	checkCode(t, apiCall(h, "GET", users, expired.Token, nil), 401, "invalid_api_token")
	checkCode(t, request(h, "POST", tokens+"/"+scoped.ID+"/revoke", testOrigin, nil, admin, csrf), 204, "")
	checkCode(t, apiCall(h, "GET", users, scoped.Token, nil), 401, "invalid_api_token")

	grantRes := request(h, "POST", "/v1/admin/role-grants", testOrigin, managementGrant{Username: member.Username, Role: "user_viewer", Scope: "self"}, admin, csrf)
	checkCode(t, grantRes, 201, "")
	grantID := responseValue[managementGrant](t, grantRes.Body.Bytes()).ID
	login := loginRequest(h, member.Username, testPassword)
	checkCode(t, login, 204, "")
	session := login.Result().Cookies()[0]
	checkCode(t, request(h, "POST", tokens, testOrigin, input, session, csrfToken(session.Value)), 403, "forbidden")
	input.Scope, input.GroupID = "self", ""
	self := issue(input, session)
	checkCode(t, apiCall(h, "GET", users+"/"+member.ID, self.Token, nil), 200, member.Username)
	checkCode(t, request(h, "POST", "/v1/admin/role-grants/"+grantID+"/revoke", testOrigin, nil, admin, csrf), 204, "")
	checkCode(t, apiCall(h, "GET", users, self.Token, nil), 403, "forbidden")
	checkCode(t, request(h, "POST", users+"/"+member.ID+"/disable", testOrigin, map[string]string{"reason": "test"}, admin, csrf), 200, "disabled")
	checkCode(t, apiCall(h, "GET", users, self.Token, nil), 401, "invalid_api_token")
	checkCode(t, request(h, "POST", users+"/"+member.ID+"/enable", testOrigin, map[string]string{"reason": "test"}, admin, csrf), 200, "active")
	checkCode(t, apiCall(h, "GET", users, self.Token, nil), 401, "invalid_api_token")
}
