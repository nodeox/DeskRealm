package m1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func adminSession(t *testing.T, ctx context.Context, store Store) (http.Handler, *http.Cookie, string, string) {
	t.Helper()
	handler, input := fixture(t, ctx, store)
	checkCode(t, request(handler, "POST", "/v1/bootstrap", testOrigin, input, nil, ""), http.StatusCreated, "")
	res := loginRequest(handler, "admin", testPassword)
	checkCode(t, res, http.StatusNoContent, "")
	cookie := res.Result().Cookies()[0]
	info := request(handler, "GET", "/v1/auth/session", "", nil, cookie, "")
	checkCode(t, info, http.StatusOK, "platform_admin")
	var user principal
	if err := json.Unmarshal(info.Body.Bytes(), &user); err != nil || user.CSRF == "" || user.UserID == "" {
		t.Fatalf("invalid administrator session: %v", err)
	}
	return handler, cookie, user.CSRF, user.UserID
}

func loginRequest(h http.Handler, username, password string) *httptest.ResponseRecorder {
	return request(h, "POST", "/v1/auth/login", testOrigin,
		map[string]string{"username": username, "password": password}, nil, "")
}

func createMember(t *testing.T, handler http.Handler, admin *http.Cookie, csrf string) managedUser {
	t.Helper()
	params := map[string]string{"username": "member", "password": testPassword}
	res := request(handler, "POST", "/v1/admin/users", testOrigin, params, admin, csrf)
	checkCode(t, res, http.StatusCreated, `"status":"active"`)
	var member managedUser
	if err := json.Unmarshal(res.Body.Bytes(), &member); err != nil || member.ID == "" || member.PlatformAdmin {
		t.Fatalf("invalid member: %v", err)
	}
	return member
}

func TestUserStatusPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	handler, admin, csrf, adminID := adminSession(t, ctx, store)
	path := "/v1/admin/users"
	reason := map[string]string{"reason": "测试账号生命周期"}
	params := map[string]string{"username": "member", "password": testPassword}

	checkCode(t, request(handler, "GET", path, "", nil, nil, ""), http.StatusUnauthorized, errSession.Error())
	checkCode(t, request(handler, "POST", path, testOrigin, params, admin, ""), http.StatusForbidden, "csrf_denied")
	member := createMember(t, handler, admin, csrf)
	checkCode(t, request(handler, "POST", path, testOrigin, params, admin, csrf), http.StatusConflict, errUserExists.Error())
	checkCode(t, request(handler, "GET", path, "", nil, admin, ""), http.StatusOK, member.ID)

	res := loginRequest(handler, "member", testPassword)
	checkCode(t, res, http.StatusNoContent, "")
	cookie := res.Result().Cookies()[0]
	checkCode(t, request(handler, "GET", path, "", nil, cookie, ""), http.StatusForbidden, errForbidden.Error())
	checkCode(t, request(handler, "POST", path+"/"+adminID+"/disable", testOrigin, reason, admin, csrf),
		http.StatusConflict, errLastAdmin.Error())

	checkCode(t, request(handler, "POST", path+"/"+member.ID+"/disable", testOrigin, reason, admin, csrf), http.StatusOK, `"status":"disabled"`)
	checkCode(t, request(handler, "GET", "/v1/auth/session", "", nil, cookie, ""), http.StatusUnauthorized, errSession.Error())
	checkCode(t, loginRequest(handler, "member", testPassword), http.StatusUnauthorized, errCredentials.Error())
	checkCode(t, request(handler, "POST", path+"/"+member.ID+"/disable", testOrigin, reason, admin, csrf), http.StatusOK, `"status":"disabled"`)
	var count int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events a JOIN outbox o ON o.event_id=a.id
		WHERE a.actor_id=$1 AND a.resource_id=$2 AND a.action='user.disabled' AND a.reason=$3`,
		adminID, member.ID, reason["reason"]).Scan(&count); err != nil || count != 1 {
		t.Fatalf("idempotent state audit missing or duplicate: %d %v", count, err)
	}

	checkCode(t, request(handler, "POST", path+"/"+member.ID+"/enable", testOrigin, reason, admin, csrf), http.StatusOK, `"status":"active"`)
	checkCode(t, request(handler, "GET", "/v1/auth/session", "", nil, cookie, ""), http.StatusUnauthorized, errSession.Error())
	checkCode(t, loginRequest(handler, "member", testPassword), http.StatusNoContent, "")
	checkCode(t, request(handler, "POST", path+"/"+member.ID+"/archive", testOrigin, reason, admin, csrf), http.StatusOK, `"status":"archived"`)
	checkCode(t, request(handler, "GET", path, "", nil, admin, ""), http.StatusOK, member.ID)
	checkCode(t, request(handler, "POST", path+"/"+member.ID+"/enable", testOrigin, reason, admin, csrf),
		http.StatusConflict, errUserTransition.Error())
}

func TestAccountRevocationPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	handler, admin, csrf, _ := adminSession(t, ctx, store)
	member := createMember(t, handler, admin, csrf)
	res := loginRequest(handler, member.Username, testPassword)
	checkCode(t, res, http.StatusNoContent, "")
	first := res.Result().Cookies()[0]

	action := "/v1/admin/users/" + member.ID + "/disable"
	reason := map[string]string{"reason": "并发停用回归"}
	if _, err := store.DB.Exec(ctx, "ALTER TABLE audit_events ADD CONSTRAINT reject_disable CHECK (action <> 'user.disabled')"); err != nil {
		t.Fatal(err)
	}
	checkCode(t, request(handler, "POST", action, testOrigin, reason, admin, csrf), http.StatusInternalServerError, "service_error")
	checkCode(t, request(handler, "GET", "/v1/auth/session", "", nil, first, ""), http.StatusOK, member.Username)
	if _, err := store.DB.Exec(ctx, "ALTER TABLE audit_events DROP CONSTRAINT reject_disable"); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	loginResult, disableResult := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	go func() { <-start; loginResult <- loginRequest(handler, member.Username, testPassword) }()
	go func() { <-start; disableResult <- request(handler, "POST", action, testOrigin, reason, admin, csrf) }()
	close(start)
	checkCode(t, <-disableResult, http.StatusOK, `"status":"disabled"`)
	login := <-loginResult
	checkCode(t, request(handler, "GET", "/v1/auth/session", "", nil, first, ""), http.StatusUnauthorized, errSession.Error())
	if login.Code == http.StatusNoContent {
		recent := login.Result().Cookies()[0]
		checkCode(t, request(handler, "GET", "/v1/auth/session", "", nil, recent, ""), http.StatusUnauthorized, errSession.Error())
	} else {
		checkCode(t, login, http.StatusUnauthorized, errCredentials.Error())
	}
	var live int
	if err := store.DB.QueryRow(ctx, "SELECT count(*) FROM browser_sessions WHERE user_id=$1 AND revoked_at IS NULL", member.ID).Scan(&live); err != nil || live != 0 {
		t.Fatalf("%d live sessions after disable: %v", live, err)
	}
}
