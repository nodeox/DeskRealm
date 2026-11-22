package m1

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testOrigin = "https://deskrealm.example.test"
const testPassword = "deskrealm test passphrase"

func postgres(t *testing.T) (context.Context, Store, string) {
	t.Helper()
	dsn := os.Getenv("DESKREALM_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("DESKREALM_REQUIRE_DB_TESTS") == "1" {
			t.Fatal("DESKREALM_TEST_DATABASE_URL required")
		}
		t.Skip("run scripts/deskrealm-m1-test bootstrap-api for PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	root, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect test database:", err)
	}
	random, err := token()
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + strings.ReplaceAll(strings.ToLower(random[:22]), "-", "_")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := root.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := root.Exec(clean, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
		root.Close(clean)
	})
	connection, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	params := connection.Query()
	params.Set("search_path", schema)
	connection.RawQuery = params.Encode()
	dsn = connection.String()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return ctx, Store{DB: pool}, dsn
}

func request(handler http.Handler, method, path, origin string, value any, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	var body *bytes.Reader
	if value == nil {
		body = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(value)
		if err != nil {
			panic(err)
		}
		body = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, testOrigin+path, body)
	if value != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, r)
	return res
}

func checkCode(t *testing.T, r *httptest.ResponseRecorder, status int, errorCode string) {
	t.Helper()
	if r.Code != status || (errorCode != "" && !strings.Contains(r.Body.String(), errorCode)) {
		t.Fatalf("response %d %s, expected %d %q", r.Code, r.Body.String(), status, errorCode)
	}
}

func issue(t *testing.T, ctx context.Context, s Store) string {
	t.Helper()
	value, err := s.IssueTicket(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func fixture(t *testing.T, ctx context.Context, s Store) (http.Handler, bootstrapRequest) {
	t.Helper()
	origin, err := ParseOrigin(testOrigin, false)
	if err != nil {
		t.Fatal(err)
	}
	app := NewHandler(s, origin, fstest.MapFS{"index.html": {Data: []byte("<!doctype html>DeskRealm shell")}})
	return app, bootstrapRequest{"端域测试", "Admin", testPassword, issue(t, ctx, s)}
}

func TestBootstrapPostgres(t *testing.T) {
	ctx, s, dsn := postgres(t)
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatal("migration rerun:", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migrations, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{"90000_fail.sql": {Data: []byte("-- +goose Up\nCREATE TABLE never_committed (id int);\nSELECT 1/0;\n")}}
	for _, path := range migrations {
		data, err := migrationFiles.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files[strings.TrimPrefix(path, "migrations/")] = &fstest.MapFile{Data: data}
	}
	if err := migrateFrom(ctx, db, files); err == nil {
		t.Fatal("invalid SQL migration unexpectedly succeeded")
	}
	var absent bool
	if err := s.DB.QueryRow(ctx, "SELECT to_regclass('never_committed') IS NULL").Scan(&absent); err != nil || !absent {
		t.Fatalf("failure migration leaked DDL: %v", err)
	}
	h, input := fixture(t, ctx, s)
	checkCode(t, request(h, "GET", "/v1/bootstrap/status", "", nil, nil, ""), http.StatusOK, `"initialized":false`)
	expired := input
	if _, err := s.DB.Exec(ctx, "UPDATE bootstrap_state SET ticket_expires_at=now()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	checkCode(t, request(h, "POST", "/v1/bootstrap", testOrigin, expired, nil, ""), http.StatusUnauthorized, errTicket.Error())
	input.Ticket = issue(t, ctx, s)
	checkCode(t, request(h, "POST", "/v1/bootstrap", testOrigin, expired, nil, ""), http.StatusUnauthorized, errTicket.Error())

	if _, err := s.DB.Exec(ctx, "ALTER TABLE audit_events ADD CONSTRAINT forced_failure CHECK (action <> 'organization.initialized')"); err != nil {
		t.Fatal(err)
	}
	checkCode(t, request(h, "POST", "/v1/bootstrap", testOrigin, input, nil, ""), http.StatusInternalServerError, "service_error")
	var count int
	if err := s.DB.QueryRow(ctx, "SELECT count(*) FROM organizations").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed audit did not roll back organization: %d %v", count, err)
	}
	if _, err := s.DB.Exec(ctx, "ALTER TABLE audit_events DROP CONSTRAINT forced_failure"); err != nil {
		t.Fatal(err)
	}

	results := make(chan int, 2)
	for range 2 {
		go func() { results <- request(h, "POST", "/v1/bootstrap", testOrigin, input, nil, "").Code }()
	}
	a, b := <-results, <-results
	if !((a == http.StatusCreated && b == http.StatusConflict) || (b == http.StatusCreated && a == http.StatusConflict)) {
		t.Fatalf("concurrent ticket redemption returned %d and %d", a, b)
	}
	checkCode(t, request(h, "POST", "/v1/bootstrap", testOrigin, input, nil, ""), http.StatusConflict, errInitialized.Error())
	checkCode(t, request(h, "GET", "/v1/bootstrap/status", "", nil, nil, ""), http.StatusOK, `"initialized":true`)
	if _, err := s.IssueTicket(ctx); err != errInitialized {
		t.Fatalf("initialization still issues ticket: %v", err)
	}
	var username, hash string
	if err := s.DB.QueryRow(ctx, "SELECT username,password_hash FROM users").Scan(&username, &hash); err != nil {
		t.Fatal(err)
	}
	if username != "admin" || !strings.HasPrefix(hash, hashPrefix) || strings.Contains(hash, input.Password) {
		t.Fatal("invalid stored local account")
	}
	if err := s.DB.QueryRow(ctx, "SELECT count(*) FROM audit_events e JOIN outbox o ON e.id=o.event_id WHERE e.action='organization.initialized'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("initialization audit not written once: %d %v", count, err)
	}
}

func TestBrowserSessionPostgres(t *testing.T) {
	ctx, s, _ := postgres(t)
	h, input := fixture(t, ctx, s)
	checkCode(t, request(h, "POST", "/v1/bootstrap", testOrigin, input, nil, ""), http.StatusCreated, "")
	checkCode(t, request(h, "GET", "/v1/unknown", "", nil, nil, ""), http.StatusNotFound, "not_found")
	checkCode(t, request(h, "GET", "/initialize", "", nil, nil, ""), http.StatusOK, "DeskRealm shell")
	credentials := map[string]string{"username": input.Username, "password": input.Password}
	checkCode(t, request(h, "POST", "/v1/auth/login", "https://external.example.test", credentials, nil, ""), http.StatusForbidden, "origin_denied")
	checkCode(t, request(h, "POST", "/v1/auth/login", testOrigin, map[string]string{"username": "admin", "password": "incorrect"}, nil, ""), http.StatusUnauthorized, errCredentials.Error())
	login := request(h, "POST", "/v1/auth/login", testOrigin, credentials, nil, "")
	checkCode(t, login, http.StatusNoContent, "")
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].Name != "__Host-deskrealm" || cookies[0].Path != "/" {
		t.Fatal("session cookie is missing required boundaries")
	}
	old := cookies[0]
	login = request(h, "POST", "/v1/auth/login", testOrigin, credentials, old, "")
	checkCode(t, login, http.StatusNoContent, "")
	cookie := login.Result().Cookies()[0]
	if cookie.Value == old.Value {
		t.Fatal("successful login did not rotate session")
	}
	checkCode(t, request(h, "GET", "/v1/auth/session", "", nil, old, ""), http.StatusUnauthorized, errSession.Error())
	origin, _ := ParseOrigin(testOrigin, false)
	restarted := NewHandler(s, origin, nil)
	response := request(restarted, "GET", "/v1/auth/session", "", nil, cookie, "")
	checkCode(t, response, http.StatusOK, "platform_admin")
	var actor principal
	if err := json.Unmarshal(response.Body.Bytes(), &actor); err != nil || actor.Organization != input.Organization || actor.CSRF == "" {
		t.Fatalf("invalid persisted session after restart: %v", err)
	}
	checkCode(t, request(restarted, "GET", "/v1/overview", "", nil, cookie, ""), http.StatusOK, `"can_manage":true`)
	checkCode(t, request(restarted, "POST", "/v1/auth/logout", testOrigin, nil, cookie, "wrong"), http.StatusForbidden, "csrf_denied")
	checkCode(t, request(restarted, "POST", "/v1/auth/logout", testOrigin, nil, cookie, actor.CSRF), http.StatusNoContent, "")
	checkCode(t, request(h, "GET", "/v1/auth/session", "", nil, cookie, ""), http.StatusUnauthorized, errSession.Error())
	login = request(h, "POST", "/v1/auth/login", testOrigin, credentials, nil, "")
	checkCode(t, login, http.StatusNoContent, "")
	cookie = login.Result().Cookies()[0]
	if _, err := s.DB.Exec(ctx, "UPDATE browser_sessions SET expires_at=now()-interval '1 second' WHERE token_digest=$1", digest(cookie.Value)); err != nil {
		t.Fatal(err)
	}
	checkCode(t, request(h, "GET", "/v1/auth/session", "", nil, cookie, ""), http.StatusUnauthorized, errSession.Error())
}

func TestOriginAndPassword(t *testing.T) {
	for _, tc := range []struct {
		url       string
		dev, want bool
	}{
		{"https://deskrealm.example.test", false, true},
		{"http://127.0.0.1:8080", false, false},
		{"http://127.0.0.1:8080", true, true},
		{"http://example.test:8080", true, false},
		{"https://deskrealm.example.test/path", false, false},
	} {
		_, err := ParseOrigin(tc.url, tc.dev)
		if (err == nil) != tc.want {
			t.Errorf("%s dev=%v: %v", tc.url, tc.dev, err)
		}
	}
	hashes := make(map[string]bool, 2)
	for range 2 {
		hash, err := hashPassword(testPassword)
		if err != nil || !passwordMatches(testPassword, hash) ||
			passwordMatches("wrong password", hash) || hashes[hash] {
			t.Fatal(fmt.Errorf("invalid password behavior: %v", err))
		}
		hashes[hash] = true
	}
}
