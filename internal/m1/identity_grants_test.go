package m1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func linkTicket(t *testing.T, res *httptest.ResponseRecorder, path string) string {
	t.Helper()
	checkCode(t, res, http.StatusCreated, "")
	var grant issuedGrant
	if err := json.Unmarshal(res.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(grant.URL)
	if err != nil || parsed.Path != path || parsed.RawQuery != "" || grant.ExpiresAt.IsZero() {
		t.Fatal("invalid account link")
	}
	query, err := url.ParseQuery(parsed.Fragment)
	if err != nil || !validToken(query.Get("ticket")) {
		t.Fatal("account link lacks fragment ticket")
	}
	return query.Get("ticket")
}

func TestIdentityLifecyclePostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	handler, admin, csrf, _ := adminSession(t, ctx, store)
	invitations := "/v1/admin/user-invitations"
	redeem := "/v1/auth/accept-invitation"
	name := "invited"
	inviteParams := map[string]string{"username": name}
	old := linkTicket(t, request(handler, "POST", invitations, testOrigin, inviteParams, admin, csrf), "/accept-invitation")
	next := linkTicket(t, request(handler, "POST", invitations, testOrigin, inviteParams, admin, csrf), "/accept-invitation")
	params := func(ticket, password string) map[string]string {
		return map[string]string{"ticket": ticket, "password": password}
	}
	checkCode(t, request(handler, "POST", redeem, testOrigin, params(old, testPassword), nil, ""), http.StatusUnauthorized, errIdentityGrant.Error())
	checkCode(t, request(handler, "POST", "/v1/auth/recover", testOrigin, params(next, testPassword), nil, ""), http.StatusUnauthorized, errIdentityGrant.Error())
	var userID, state string
	if err := store.DB.QueryRow(ctx, "SELECT id,status FROM users WHERE username=$1", name).Scan(&userID, &state); err != nil || state != "pending" {
		t.Fatalf("invitation did not create pending account: %s %v", state, err)
	}
	checkCode(t, loginRequest(handler, name, testPassword), http.StatusUnauthorized, errCredentials.Error())
	if _, err := store.DB.Exec(ctx, "UPDATE identity_grants SET expires_at=now()-interval '1 second' WHERE token_digest=$1", digest(next)); err != nil {
		t.Fatal(err)
	}
	checkCode(t, request(handler, "POST", redeem, testOrigin, params(next, testPassword), nil, ""), http.StatusUnauthorized, errIdentityGrant.Error())

	next = linkTicket(t, request(handler, "POST", invitations, testOrigin, inviteParams, admin, csrf), "/accept-invitation")
	outcomes := make(chan int, 2)
	for range 2 {
		go func() {
			outcomes <- request(handler, "POST", redeem, testOrigin, params(next, testPassword), nil, "").Code
		}()
	}
	a, b := <-outcomes, <-outcomes
	if (a != http.StatusNoContent || b != http.StatusUnauthorized) && (b != http.StatusNoContent || a != http.StatusUnauthorized) {
		t.Fatalf("invitation redemption is not single use: %d, %d", a, b)
	}
	var sameID string
	if err := store.DB.QueryRow(ctx, "SELECT id,status FROM users WHERE username=$1", name).Scan(&sameID, &state); err != nil || sameID != userID || state != "active" {
		t.Fatal("invitation redemption changed the stable user identity")
	}
	res := loginRequest(handler, name, testPassword)
	checkCode(t, res, http.StatusNoContent, "")
	oldCookie := res.Result().Cookies()[0]

	resetPath := "/v1/admin/users/" + userID + "/recovery"
	reset := linkTicket(t, request(handler, "POST", resetPath, testOrigin, nil, admin, csrf), "/recover")
	const replacement = "replacement test passphrase"
	checkCode(t, request(handler, "POST", "/v1/auth/recover", testOrigin, params(reset, replacement), nil, ""), http.StatusNoContent, "")
	checkCode(t, request(handler, "GET", "/v1/auth/session", "", nil, oldCookie, ""), http.StatusUnauthorized, errSession.Error())
	checkCode(t, loginRequest(handler, name, testPassword), http.StatusUnauthorized, errCredentials.Error())
	checkCode(t, loginRequest(handler, name, replacement), http.StatusNoContent, "")
	checkCode(t, request(handler, "POST", "/v1/auth/recover", testOrigin, params(reset, testPassword), nil, ""), http.StatusUnauthorized, errIdentityGrant.Error())

	path := "/v1/admin/users/imports"
	payload := map[string]any{"idempotency_key": "batch-test-1234", "usernames": []string{"Invited", "from-import"}}
	created := request(handler, "POST", path, testOrigin, payload, admin, csrf)
	checkCode(t, created, http.StatusOK, `"result":"pending"`)
	checkCode(t, created, http.StatusOK, `"result":"exists"`)
	same := request(handler, "POST", path, testOrigin, payload, admin, csrf)
	checkCode(t, same, http.StatusOK, "")
	if same.Body.String() != created.Body.String() {
		t.Fatal("identical batch did not return the original results")
	}
	payload["usernames"] = []string{"different"}
	checkCode(t, request(handler, "POST", path, testOrigin, payload, admin, csrf), http.StatusConflict, errImportConflict.Error())
	var count int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events a JOIN outbox o ON o.event_id=a.id
		WHERE a.action IN ('user.invitation_redeemed','user.recovery_redeemed','user.import_batch')`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("expected one committed audit for each operation: %d, %v", count, err)
	}
}
