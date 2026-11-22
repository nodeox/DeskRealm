package m1

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"golang.org/x/oauth2"
)

func TestTOTPReplayPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	_, _, _, adminID := adminSession(t, ctx, store)
	key, err := ParseTOTPKey(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := ParseOrigin(testOrigin, false)
	handler := NewHandler(store, origin, nil, AuthConfig{TOTP: key})
	login := loginRequest(handler, "admin", testPassword)
	checkCode(t, login, 204, "")
	cookie := login.Result().Cookies()[0]
	csrf := csrfToken(cookie.Value)
	setup := request(handler, "POST", "/v1/auth/totp/setup", testOrigin, map[string]string{"password": testPassword}, cookie, csrf)
	checkCode(t, setup, 201, "secret")
	var enrollment struct{ Secret string }
	if err := json.Unmarshal(setup.Body.Bytes(), &enrollment); err != nil || enrollment.Secret == "" {
		t.Fatal("missing TOTP enrollment", err)
	}
	var encrypted []byte
	if err := store.DB.QueryRow(ctx, "SELECT ciphertext FROM user_totp WHERE user_id=$1", adminID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encrypted), enrollment.Secret) {
		t.Fatal("stored plaintext TOTP key")
	}
	if _, err := key.decrypt("another-account", encrypted); err == nil {
		t.Fatal("encrypted TOTP factor could be transferred between users")
	}
	verify := func(challenge *http.Cookie, code string) *httptest.ResponseRecorder {
		return request(handler, "POST", "/v1/auth/totp/verify", testOrigin, map[string]string{"code": code}, challenge, "")
	}
	now := time.Now()
	past, _ := totp.GenerateCode(enrollment.Secret, now)
	current, _ := totp.GenerateCode(enrollment.Secret, now.Add(30*time.Second))
	checkCode(t, request(handler, "POST", "/v1/auth/totp/confirm", testOrigin, map[string]string{"password": "wrong", "code": past}, cookie, csrf), 401, "invalid_credentials")
	login = loginRequest(handler, "admin", testPassword)
	checkCode(t, login, 204, "")
	cookie = login.Result().Cookies()[0]
	csrf = csrfToken(cookie.Value)
	checkCode(t, request(handler, "POST", "/v1/auth/totp/confirm", testOrigin, map[string]string{"password": testPassword, "code": past}, cookie, csrf), 204, "")
	login = loginRequest(handler, "admin", testPassword)
	checkCode(t, login, 202, "totp")
	challenge := login.Result().Cookies()[0]
	checkCode(t, request(handler, "GET", "/v1/auth/session", "", nil, challenge, ""), 401, "session_required")
	checkCode(t, verify(challenge, past), 401, "invalid_verification_code")
	ch := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { ch <- verify(challenge, current) }()
	}
	a, b := <-ch, <-ch
	if a.Code != 204 {
		a, b = b, a
	}
	checkCode(t, a, 204, "")
	checkCode(t, b, 401, "invalid_verification_code")
	login = loginRequest(handler, "admin", testPassword)
	checkCode(t, login, 202, "totp")
	checkCode(t, verify(login.Result().Cookies()[0], current), 401, "invalid_verification_code")
	reset := "/v1/admin/users/" + adminID + "/totp-reset"
	resetInput := map[string]string{"password": "wrong", "reason": "Authenticator replacement"}
	checkCode(t, request(handler, "POST", reset, testOrigin, resetInput, cookie, csrf), 401, "invalid_credentials")
	resetInput["password"] = testPassword
	checkCode(t, request(handler, "POST", reset, testOrigin, resetInput, cookie, csrf), 204, "")
	checkCode(t, request(handler, "GET", "/v1/auth/session", "", nil, cookie, ""), 401, "session_required")
	checkCode(t, loginRequest(handler, "admin", testPassword), 204, "")
	var count int
	if err := store.DB.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action='user.totp_reset' AND actor_id=$1 AND resource_id=$1 AND reason=$2", adminID, resetInput["reason"]).Scan(&count); err != nil || count != 1 {
		t.Fatalf("missing verification reset audit: %d, %v", count, err)
	}
}

func csrfToken(value string) string {
	return base64.RawURLEncoding.EncodeToString(digest("csrf\x00" + value))
}

type issuerCase struct {
	subject     string
	issuer      string
	audience    string
	nonce       string
	expired     bool
	signedWrong bool
}

func TestOIDCBindingPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	adminHandler, admin, csrf, _ := adminSession(t, ctx, store)
	member := createMember(t, adminHandler, admin, csrf)
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var upstream *httptest.Server
	query := make(chan url.Values, 1)
	claimsCase := make(chan issuerCase, 1)
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			writeJSON(w, 200, map[string]any{"issuer": upstream.URL, "authorization_endpoint": upstream.URL + "/authorize",
				"token_endpoint": upstream.URL + "/token", "jwks_uri": upstream.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
				"token_endpoint_auth_methods_supported": []string{"client_secret_basic"}})
		case "/keys":
			writeJSON(w, 200, map[string]any{"keys": []map[string]string{{"kid": "fixture", "kty": "RSA", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(private.N.Bytes()), "e": "AQAB"}}})
		case "/token":
			if err := r.ParseForm(); err != nil {
				w.WriteHeader(400)
				return
			}
			params, testCase := <-query, <-claimsCase
			id, secret, ok := r.BasicAuth()
			if !ok || id != "deskrealm" || secret != "test-only" || r.Form.Get("code") != "fixture-code" ||
				r.Form.Get("redirect_uri") != testOrigin+"/v1/auth/oidc/callback" ||
				oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != params.Get("code_challenge") {
				t.Error("invalid OIDC code exchange, client, redirect URI or PKCE")
				w.WriteHeader(400)
				return
			}
			if testCase.issuer == "" {
				testCase.issuer = upstream.URL
			}
			if testCase.audience == "" {
				testCase.audience = "deskrealm"
			}
			if testCase.nonce == "" {
				testCase.nonce = params.Get("nonce")
			}
			expiry := time.Now().Add(time.Minute)
			if testCase.expired {
				expiry = time.Now().Add(-time.Minute)
			}
			raw, _ := json.Marshal(map[string]any{"iss": testCase.issuer, "sub": testCase.subject, "aud": testCase.audience,
				"nonce": testCase.nonce, "iat": time.Now().Unix(), "exp": expiry.Unix(), "email": "shared@example.test"})
			encode := base64.RawURLEncoding.EncodeToString
			payload := encode([]byte(`{"alg":"RS256","kid":"fixture"}`)) + "." + encode(raw)
			sum := sha256.Sum256([]byte(payload))
			key := private
			if testCase.signedWrong {
				key = wrongKey
			}
			signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			writeJSON(w, 200, map[string]string{"access_token": "fixture-access-token", "token_type": "Bearer",
				"id_token": payload + "." + encode(signature)})
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	if err := store.BindOIDC(ctx, upstream.URL, "known-subject", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindOIDC(ctx, upstream.URL, "member-subject", member.Username); err != nil {
		t.Fatal(err)
	}
	if err := store.BindOIDC(ctx, upstream.URL, "known-subject", member.Username); err == nil {
		t.Fatal("rebound existing issuer and subject")
	}
	provider, err := OpenOIDC(context.Background(), upstream.URL, "deskrealm", "test-only", testOrigin, true)
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse(testOrigin)
	handler := NewHandler(store, origin, nil, AuthConfig{OIDC: provider})
	run := func(tc issuerCase, want string) (*http.Cookie, string, *http.Cookie) {
		t.Helper()
		start := request(handler, "GET", "/v1/auth/oidc/start", "", nil, nil, "")
		checkCode(t, start, 303, "")
		dest, err := url.Parse(start.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		p := dest.Query()
		if p.Get("code_challenge_method") != "S256" || !validToken(p.Get("state")) || !validToken(p.Get("nonce")) {
			t.Fatal("OIDC redirect lacks state, nonce or PKCE")
		}
		cookie := start.Result().Cookies()[0]
		path := "/v1/auth/oidc/callback?" + url.Values{"state": {p.Get("state")}, "code": {"fixture-code"}}.Encode()
		missing := request(handler, "GET", path, "", nil, nil, "")
		if missing.Header().Get("Location") != "/login?oidc=failed" {
			t.Fatal("unbound browser accepted")
		}
		query <- p
		claimsCase <- tc
		res := request(handler, "GET", path, "", nil, cookie, "")
		if res.Code != 303 || res.Header().Get("Location") != want {
			t.Fatalf("OIDC callback %d %s expected %s", res.Code, res.Header().Get("Location"), want)
		}
		for _, value := range res.Result().Cookies() {
			if value.Name == "__Host-deskrealm" {
				return value, path, cookie
			}
		}
		return nil, path, cookie
	}
	for _, tc := range []issuerCase{
		{subject: "same-email-unbound"},
		{subject: "known-subject", issuer: "https://wrong.example"},
		{subject: "known-subject", audience: "another-client"},
		{subject: "known-subject", nonce: "wrong"},
		{subject: "known-subject", expired: true},
		{subject: "known-subject", signedWrong: true},
	} {
		if cookie, _, _ := run(tc, "/login?oidc=failed"); cookie != nil {
			t.Fatal("invalid external identity obtained session")
		}
	}
	checkCode(t, request(adminHandler, "POST", "/v1/admin/users/"+member.ID+"/disable", testOrigin, map[string]string{"reason": "test"}, admin, csrf), 200, "disabled")
	if cookie, _, _ := run(issuerCase{subject: "member-subject"}, "/login?oidc=failed"); cookie != nil {
		t.Fatal("disabled account obtained session")
	}
	cookie, path, state := run(issuerCase{subject: "known-subject"}, "/overview")
	if cookie == nil {
		t.Fatal("valid external identity missing session")
	}
	checkCode(t, request(handler, "GET", "/v1/auth/session", "", nil, cookie, ""), 200, "admin")
	replay := request(handler, "GET", path, "", nil, state, "")
	if replay.Header().Get("Location") != "/login?oidc=failed" {
		t.Fatal("consumed authorization state replayed")
	}
}
