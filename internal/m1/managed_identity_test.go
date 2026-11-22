package m1

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"deskrealm/internal/managedproto"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	deviceChallengeURL = "/v1/managed/devices/challenge"
	deviceCompleteURL  = "/v1/managed/devices/complete"
	deviceHeartbeatURL = "/v1/managed/devices/heartbeat"
	userChallengeURL   = "/v1/managed/users/challenge"
	userCompleteURL    = "/v1/managed/users/complete"
	userRuntimeURL     = "/v1/managed/users/session"
	signingKeyURL      = "/v1/managed/signing-key"
)

func managedTestKey(t *testing.T) (string, *ManagedSigningKey) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "signing.seed")
	value, err := token()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := LoadManagedSigningKey(path, "test-2026")
	if err != nil {
		t.Fatal(err)
	}
	return path, key
}

func managedHandler(t *testing.T, store Store, key *ManagedSigningKey) http.Handler {
	t.Helper()
	origin, err := ParseOrigin(testOrigin, false)
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(store, origin, nil, AuthConfig{ManagedKey: key})
}

func nativeRequest(h http.Handler, method, path, bearer string, input any) *httptest.ResponseRecorder {
	var body []byte
	if input != nil {
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			panic(err)
		}
	}
	r := httptest.NewRequest(method, testOrigin+path, bytes.NewReader(body))
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, r)
	return res
}

func runtimeFixture(t *testing.T, h http.Handler, admin *http.Cookie, csrf, rustdeskID string, approve bool) (string, ed25519.PrivateKey) {
	t.Helper()
	group := deviceGroupFixture(t, h, admin, csrf, "group "+rustdeskID)
	cred := credentialFixture(t, h, admin, csrf, group.ID)
	in, key := deviceProofFixture(t, h, group.ID, cred, rustdeskID)
	reply := request(h, "POST", "/v1/enrollment/complete", "", in, nil, "")
	checkCode(t, reply, 200, "pending")
	id := responseValue[enrollmentResult](t, reply.Body.Bytes()).DeviceID
	if approve {
		checkCode(t, request(h, "POST", "/v1/admin/devices/"+id+"/approve", testOrigin, struct{}{}, admin, csrf), 200, "approved")
	}
	return id, key
}

func managedChallengeFixture(t *testing.T, h http.Handler, device string) identityChallenge {
	t.Helper()
	reply := nativeRequest(h, "POST", deviceChallengeURL, "", map[string]any{"device_id": device, "protocol_version": 1})
	checkCode(t, reply, 200, device)
	if reply.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing no-store on managed challenge")
	}
	return responseValue[identityChallenge](t, reply.Body.Bytes())
}

func signManagedFixture(t *testing.T, c identityChallenge, key ed25519.PrivateKey) map[string]string {
	t.Helper()
	// Use the public wire specification, independent of the server's proof assembler.
	uuid := func(id string) []byte {
		b, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	decode := func(s string) []byte {
		b, err := base64.RawURLEncoding.Strict().DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	number := func(n int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(n)) }
	var proof []byte
	var err error
	if c.User == "" {
		proof, err = managedproto.Encode(managedproto.DeviceChallenge, uuid(c.Organization), uuid(c.Device), uuid(c.ID),
			decode(c.Nonce), number(c.KeyEpoch), number(c.ExpiresMS), binary.BigEndian.AppendUint16(nil, c.Protocol))
	} else {
		proof, err = managedproto.Encode(managedproto.UserBinding, uuid(c.Organization), uuid(c.Device), uuid(c.User), uuid(c.ID),
			decode(c.Nonce), digest(string(decode(c.Flow))), number(c.KeyEpoch), number(c.ExpiresMS))
	}
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"device_id": c.Device, "challenge_id": c.ID,
		"signature": base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, proof))}
}

func runtimeCredentialFixture(t *testing.T, h http.Handler, id string, key ed25519.PrivateKey) string {
	t.Helper()
	c := managedChallengeFixture(t, h, id)
	reply := nativeRequest(h, "POST", deviceCompleteURL, "", signManagedFixture(t, c, key))
	checkCode(t, reply, 201, managedDevicePrefix)
	return responseValue[managedCredential](t, reply.Body.Bytes()).Value
}

func TestManagedDeviceIdentityPostgres(t *testing.T) {
	ctx, store, dsn := postgres(t)
	plain, admin, csrf, _ := adminSession(t, ctx, store)
	path, key := managedTestKey(t)
	h := managedHandler(t, store, key)
	first, firstKey := runtimeFixture(t, h, admin, csrf, "10400001", true)
	second, secondKey := runtimeFixture(t, h, admin, csrf, "10400002", false)
	body := map[string]any{"device_id": first, "protocol_version": 1}

	checkCode(t, nativeRequest(plain, "POST", deviceChallengeURL, "", body), 503, errManagedUnavailable.Error())
	checkCode(t, nativeRequest(plain, "GET", signingKeyURL, "", nil), 503, errManagedUnavailable.Error())
	checkCode(t, request(h, "POST", deviceChallengeURL, testOrigin, body, admin, ""), 403, "origin_denied")
	checkCode(t, request(h, "POST", deviceChallengeURL, "https://other.example.test", body, nil, ""), 403, "origin_denied")
	checkCode(t, nativeRequest(h, "POST", deviceChallengeURL, "", map[string]any{"device_id": second, "protocol_version": 1}), 401, errManagedIdentity.Error())
	checkCode(t, nativeRequest(h, "POST", deviceChallengeURL, "", map[string]any{"device_id": first, "protocol_version": 2}), 401, errManagedIdentity.Error())
	checkCode(t, request(h, "POST", "/v1/admin/devices/"+second+"/approve", testOrigin, struct{}{}, admin, csrf), 200, "approved")

	challenge := managedChallengeFixture(t, h, first)
	if got := managedChallengeFixture(t, h, first); got != challenge {
		t.Fatal("repeat request changed the outstanding challenge")
	}
	wrong := signManagedFixture(t, challenge, secondKey)
	checkCode(t, nativeRequest(h, "POST", deviceCompleteURL, "", wrong), 401, errManagedIdentity.Error())
	right := signManagedFixture(t, challenge, firstKey)
	wrong = signManagedFixture(t, challenge, firstKey)
	wrong["device_id"] = second
	checkCode(t, nativeRequest(h, "POST", deviceCompleteURL, "", wrong), 401, errManagedIdentity.Error())

	runSQL := func(query string, args ...any) {
		t.Helper()
		if _, err := store.DB.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	runSQL(`ALTER TABLE audit_events ADD CONSTRAINT managed_test_audit_reject CHECK (action <> 'device.runtime_authenticated') NOT VALID`)
	var logs bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	// Restore the process logger before exercising concurrent requests.
	failure := nativeRequest(h, "POST", deviceCompleteURL, "", right)
	slog.SetDefault(prior)
	checkCode(t, failure, 500, "service_error")
	if !strings.Contains(logs.String(), "complete managed challenge") || strings.Contains(logs.String(), right["signature"]) ||
		strings.Contains(logs.String(), challenge.Nonce) || strings.Contains(logs.String(), "managed_test_audit_reject") {
		t.Fatal("managed SQL failure was not logged safely")
	}
	runSQL("ALTER TABLE audit_events DROP CONSTRAINT managed_test_audit_reject")

	requests := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { requests <- nativeRequest(h, "POST", deviceCompleteURL, "", right) }()
	}
	var credential managedCredential
	issued := 0
	for range 2 {
		reply := <-requests
		if reply.Code == 201 {
			issued++
			credential = responseValue[managedCredential](t, reply.Body.Bytes())
		} else {
			checkCode(t, reply, 401, errManagedIdentity.Error())
		}
	}
	if issued != 1 || managedDigest(credential.Value, managedDevicePrefix) == nil || time.Until(credential.ExpiresAt) > 15*time.Minute {
		t.Fatal("one bounded device credential must win the challenge")
	}
	var count int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM managed_device_credentials WHERE device_id=$1 AND digest=$2`,
		first, digest(credential.Value)).Scan(&count); err != nil || count != 1 {
		t.Fatalf("credential digest missing: count %d error %v", count, err)
	}
	expired := managedChallengeFixture(t, h, first)
	runSQL(`UPDATE managed_identity_challenges SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, expired.ID)
	checkCode(t, nativeRequest(h, "POST", deviceCompleteURL, "", signManagedFixture(t, expired, firstKey)), 401, errManagedIdentity.Error())

	keyBefore := nativeRequest(h, "GET", signingKeyURL, "", nil)
	checkCode(t, keyBefore, 200, "test-2026")
	reloaded, err := LoadManagedSigningKey(path, "test-2026")
	if err != nil {
		t.Fatal(err)
	}
	newPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer newPool.Close()
	restarted := managedHandler(t, Store{DB: newPool}, reloaded)
	if keyAfter := nativeRequest(restarted, "GET", signingKeyURL, "", nil); keyBefore.Body.String() != keyAfter.Body.String() {
		t.Fatal("signing public key changed after reloading the deployment")
	}

	heartbeat := func(h http.Handler, id, cred string, status int) {
		t.Helper()
		checkCode(t, nativeRequest(h, "POST", deviceHeartbeatURL, cred, map[string]string{"device_id": id, "reported_build": "1.5.3-managed.1"}), status, "")
	}
	heartbeat(restarted, second, credential.Value, 401)
	heartbeat(restarted, first, credential.Value, 204)
	readDevice := func(want string) device {
		t.Helper()
		reply := request(h, "GET", "/v1/admin/devices/"+first, "", nil, admin, "")
		checkCode(t, reply, 200, `"connection_state":"`+want+`"`)
		return responseValue[device](t, reply.Body.Bytes())
	}
	current := readDevice("online")
	var build string
	if err := store.DB.QueryRow(ctx, "SELECT managed_reported_build FROM devices WHERE id=$1", first).Scan(&build); err != nil || build != "1.5.3-managed.1" {
		t.Fatalf("reported build not stored: %v", err)
	}
	runSQL(`UPDATE devices SET managed_heartbeat_at=clock_timestamp()-interval '46 seconds' WHERE id=$1`, first)
	readDevice("offline")
	heartbeat(restarted, first, credential.Value, 204)
	readDevice("online")
	transitionProof := signManagedFixture(t, managedChallengeFixture(t, h, first), firstKey)
	rotation := rotationFixture(t, h, admin, csrf, current, false)
	_, newPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	checkCode(t, request(h, "POST", "/v1/enrollment/rotate", "", signRotation(t, rotation, newPrivate), nil, ""), 200, "approved")
	heartbeat(h, first, credential.Value, 401)
	checkCode(t, nativeRequest(h, "POST", deviceCompleteURL, "", transitionProof), 401, errManagedIdentity.Error())
	rotated := readDevice("offline")
	fresh := runtimeCredentialFixture(t, h, first, newPrivate)
	heartbeat(h, first, fresh, 204)

	runSQL(`UPDATE managed_device_credentials SET expires_at=clock_timestamp()-interval '1 second' WHERE digest=$1`, digest(fresh))
	heartbeat(h, first, fresh, 401)
	fresh = runtimeCredentialFixture(t, h, first, newPrivate)
	checkCode(t, request(h, "POST", "/v1/admin/devices/"+first+"/lifecycle/disable", testOrigin,
		map[string]int64{"version": rotated.Version}, admin, csrf), 200, "disabled")
	heartbeat(h, first, fresh, 401)
	checkCode(t, nativeRequest(h, "POST", deviceChallengeURL, "", body), 401, errManagedIdentity.Error())
	readDevice("offline")
}

func TestManagedControllerBindingPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	_, admin, adminCSRF, _ := adminSession(t, ctx, store)
	_, key := managedTestKey(t)
	h := managedHandler(t, store, key)
	first, firstKey := runtimeFixture(t, h, admin, adminCSRF, "10400003", true)
	second, secondKey := runtimeFixture(t, h, admin, adminCSRF, "10400004", true)
	portable := runtimeCredentialFixture(t, h, first, firstKey)
	otherDevice := runtimeCredentialFixture(t, h, second, secondKey)
	binding := map[string]string{"device_id": first, "device_credential": portable}

	member := createMember(t, h, admin, adminCSRF)
	login := func() (*http.Cookie, string) {
		t.Helper()
		reply := loginRequest(h, member.Username, testPassword)
		checkCode(t, reply, 204, "")
		cookie := reply.Result().Cookies()[0]
		return cookie, csrfToken(cookie.Value)
	}
	user, csrf := login()
	sameUserDifferentSession, secondCSRF := login()
	checkCode(t, request(h, "POST", userChallengeURL, testOrigin, binding, nil, ""), 401, errSession.Error())
	checkCode(t, request(h, "POST", userChallengeURL, testOrigin, binding, user, ""), 403, "csrf_denied")
	checkCode(t, request(h, "POST", userChallengeURL, "", binding, user, csrf), 403, "origin_denied")

	issue := request(h, "POST", "/v1/admin/api-tokens", testOrigin,
		map[string]any{"purpose": "test access", "action": "users.read", "scope": "organization", "lifetime_minutes": 10}, admin, adminCSRF)
	checkCode(t, issue, 201, "")
	var access struct{ Token string }
	if err := json.Unmarshal(issue.Body.Bytes(), &access); err != nil || !validToken(access.Token) {
		t.Fatal("could not acquire fixture API token")
	}
	checkCode(t, nativeRequest(h, "GET", "/v1/admin/users", access.Token, nil), 200, member.Username)
	checkCode(t, nativeRequest(h, "GET", userRuntimeURL, portable, nil), 401, errManagedIdentity.Error())
	checkCode(t, nativeRequest(h, "GET", userRuntimeURL, access.Token, nil), 401, errManagedIdentity.Error())
	checkCode(t, request(h, "POST", userChallengeURL, testOrigin,
		map[string]string{"device_id": first, "device_credential": otherDevice}, user, csrf), 401, errManagedIdentity.Error())
	checkCode(t, request(h, "POST", userChallengeURL, testOrigin,
		map[string]string{"device_id": first, "device_credential": access.Token}, user, csrf), 401, errManagedIdentity.Error())

	startBinding := func() identityChallenge {
		t.Helper()
		reply := request(h, "POST", userChallengeURL, testOrigin, binding, user, csrf)
		checkCode(t, reply, 200, member.ID)
		c := responseValue[identityChallenge](t, reply.Body.Bytes())
		if !validToken(c.Flow) || c.User != member.ID {
			t.Fatal("invalid browser binding")
		}
		return c
	}
	c := startBinding()
	if again := startBinding(); again != c {
		t.Fatal("same binding retry returned a different challenge")
	}
	proof := signManagedFixture(t, c, firstKey)
	checkCode(t, request(h, "POST", userCompleteURL, testOrigin, proof, sameUserDifferentSession, secondCSRF), 401, errManagedIdentity.Error())
	checkCode(t, request(h, "POST", userCompleteURL, testOrigin, proof, admin, adminCSRF), 401, errManagedIdentity.Error())
	checkCode(t, nativeRequest(h, "POST", deviceCompleteURL, "", proof), 401, errManagedIdentity.Error())
	if _, err := store.DB.Exec(ctx, `UPDATE browser_sessions SET expires_at=clock_timestamp()+interval '3 minutes'
		WHERE token_digest=$1`, digest(user.Value)); err != nil {
		t.Fatal(err)
	}

	complete := func(proof map[string]string) string {
		t.Helper()
		reply := request(h, "POST", userCompleteURL, testOrigin, proof, user, csrf)
		checkCode(t, reply, 201, managedUserPrefix)
		got := responseValue[managedCredential](t, reply.Body.Bytes())
		if !strings.HasPrefix(got.Value, managedUserPrefix) || time.Until(got.ExpiresAt) > 3*time.Minute {
			t.Fatal("controller credential escaped the parent login deadline")
		}
		return got.Value
	}
	bound := complete(proof)
	checkCode(t, request(h, "POST", userCompleteURL, testOrigin, proof, user, csrf), 401, errManagedIdentity.Error())
	checkCode(t, nativeRequest(h, "GET", userRuntimeURL, bound, nil), 200, member.ID)
	checkCode(t, nativeRequest(h, "GET", "/v1/admin/users", bound, nil), 401, errAPIToken.Error())
	checkCode(t, nativeRequest(h, "POST", deviceHeartbeatURL, bound,
		map[string]string{"device_id": first, "reported_build": "1.5.3"}), 401, errManagedIdentity.Error())
	checkCode(t, request(h, "POST", "/v1/auth/logout", testOrigin, nil, user, csrf), 204, "")
	checkCode(t, nativeRequest(h, "GET", userRuntimeURL, bound, nil), 401, errManagedIdentity.Error())

	user, csrf = login()
	if _, err := store.DB.Exec(ctx, `UPDATE browser_sessions SET expires_at=clock_timestamp()+interval '3 minutes'
		WHERE token_digest=$1`, digest(user.Value)); err != nil {
		t.Fatal(err)
	}
	bound = complete(signManagedFixture(t, startBinding(), firstKey))
	checkCode(t, request(h, "POST", "/v1/admin/users/"+member.ID+"/disable", testOrigin,
		map[string]string{"reason": "test account revocation"}, admin, adminCSRF), 200, "disabled")
	checkCode(t, nativeRequest(h, "GET", userRuntimeURL, bound, nil), 401, errManagedIdentity.Error())
	checkCode(t, nativeRequest(h, "POST", deviceHeartbeatURL, portable,
		map[string]string{"device_id": first, "reported_build": "1.5.3"}), 204, "")
}

func TestManagedSigningKeyConfig(t *testing.T) {
	disabled, err := LoadManagedSigningKey("", "")
	if disabled != nil || err != nil {
		t.Fatalf("empty config must disable signing: %v", err)
	}
	path, _ := managedTestKey(t)
	for _, in := range [][2]string{{"", "safe"}, {path, ""}, {path, "INVALID"}, {t.TempDir(), "safe"}} {
		if _, err := LoadManagedSigningKey(in[0], in[1]); err == nil || strings.Contains(err.Error(), path) {
			t.Fatal("invalid signing config accepted or exposed")
		}
	}
	if err := os.WriteFile(path, []byte("bad key content"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManagedSigningKey(path, "safe"); err == nil || strings.Contains(err.Error(), "bad key content") {
		t.Fatal("invalid key content accepted or exposed")
	}
}
