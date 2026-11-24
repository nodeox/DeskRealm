package m1

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"deskrealm/internal/windowsdelivery"
)

func windowsFixtureSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func windowsFixtureJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func windowsFixtureWrite(t *testing.T, path string, value []byte) {
	t.Helper()
	if err := os.WriteFile(path, value, 0600); err != nil {
		t.Fatal(err)
	}
}

func windowsFixtureStage(t *testing.T, directory string, approval windowsdelivery.Approval, private ed25519.PrivateKey) string {
	t.Helper()
	icon := []byte("fixture icon")
	approval.Brand.IconSHA = windowsFixtureSHA(icon)
	profile := windowsFixtureJSON(t, map[string]any{
		"schema": 1, "version": approval.Version, "brand": approval.Brand, "center": approval.Center,
	})
	approval.Builds = make(map[string]windowsdelivery.Build)
	manifest := windowsdelivery.Manifest{Schema: 1, Scope: "development", Approval: approval, Packages: make(map[string]windowsdelivery.Package)}
	archives := make(map[string][]byte)
	for _, arch := range []string{"x64", "arm64"} {
		client := []byte("fixture client " + arch)
		approval.Builds[arch] = windowsdelivery.Build{ManifestSHA: windowsFixtureSHA([]byte("evidence " + arch)), ClientSHA: windowsFixtureSHA(client)}
		var archive bytes.Buffer
		z := zip.NewWriter(&archive)
		for _, entry := range []struct {
			name string
			data []byte
		}{{"client.zip", client}, {"deskrealm-public.json", profile}, {"deskrealm-brand.ico", icon}} {
			header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
			header.SetMode(0644)
			member, err := z.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := member.Write(entry.data); err != nil {
				t.Fatal(err)
			}
		}
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		name := "DeskRealm-" + arch + ".zip"
		archives[name] = archive.Bytes()
		manifest.Packages[arch] = windowsdelivery.Package{File: name, SHA: windowsFixtureSHA(archive.Bytes()), Size: int64(archive.Len())}
	}
	raw := windowsFixtureJSON(t, manifest)
	id := windowsFixtureSHA(raw)
	candidate := filepath.Join(directory, id)
	if err := os.MkdirAll(filepath.Join(candidate, "bundle"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, archive := range archives {
		windowsFixtureWrite(t, filepath.Join(candidate, "bundle", name), archive)
	}
	windowsFixtureWrite(t, filepath.Join(candidate, "release.json"), raw)
	windowsFixtureWrite(t, filepath.Join(candidate, "release.sig"), ed25519.Sign(private, append([]byte("DeskRealm/windows-delivery/v1\x00"), raw...)))
	return id
}

func windowsFixtureVerifier(t *testing.T) (string, windowsdelivery.Center, *windowsdelivery.Verifier, ed25519.PrivateKey) {
	t.Helper()
	spool := t.TempDir()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	_, managed := managedTestKey(t)
	route, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	center := windowsdelivery.Center{Origin: testOrigin, IDServer: "deskrealm.example.test:21116",
		RendezvousKey: base64.StdEncoding.EncodeToString(route), SigningKeyID: managed.id,
		SigningKey: base64.RawURLEncoding.EncodeToString(managed.key.Public().(ed25519.PublicKey))}
	keyPath := filepath.Join(spool, "public.pem")
	windowsFixtureWrite(t, keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded}))
	verifier, err := ConfigureWindowsDelivery(spool, keyPath, center.Origin, center.IDServer, center.RendezvousKey, "", "", managed)
	if err != nil {
		t.Fatal(err)
	}
	return spool, center, verifier, private
}

func TestWindowsDeliveryPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	spool, center, verifier, private := windowsFixtureVerifier(t)
	store.WindowsDelivery = verifier
	h, admin, csrf, _ := adminSession(t, ctx, store)
	groupResponse := request(h, "POST", "/v1/admin/device-groups", testOrigin, map[string]string{"name": "版本验证"}, admin, csrf)
	checkCode(t, groupResponse, 201, "")
	group := responseValue[struct{ ID string }](t, groupResponse.Body.Bytes()).ID
	approval := windowsdelivery.Approval{Schema: 1, Version: "0.1.0-dev.2", Center: center, Source: strings.Repeat("c", 40),
		Brand: windowsdelivery.Brand{ID: "deskrealm", Revision: 2, Name: "端域"}, Groups: []string{group}}
	id := windowsFixtureStage(t, spool, approval, private)
	submit := func(who *http.Cookie, csrf, key, sha string) *http.Response {
		t.Helper()
		return request(h, "POST", tasksPath+"/windows-delivery", testOrigin,
			map[string]string{"request_key": key, "manifest_sha256": sha}, who, csrf).Result()
	}
	memberAccount := createMember(t, h, admin, csrf)
	member := loginRequest(h, memberAccount.Username, testPassword).Result().Cookies()[0]
	if got := submit(member, csrfToken(member.Value), "first", id); got.StatusCode != 403 {
		t.Fatalf("member submission HTTP %d", got.StatusCode)
	}
	foreign := approval
	foreign.Groups = []string{"55555555-5555-4555-8555-555555555555"}
	if got := submit(admin, csrf, "outside", windowsFixtureStage(t, spool, foreign, private)); got.StatusCode != 403 {
		t.Fatalf("unknown group HTTP %d", got.StatusCode)
	}
	foreign = approval
	foreign.Center.IDServer = "elsewhere.example.test:21116"
	if got := submit(admin, csrf, "wrong-center", windowsFixtureStage(t, spool, foreign, private)); got.StatusCode != 400 {
		t.Fatalf("wrong center HTTP %d", got.StatusCode)
	}
	queue := func(key, sha string) string {
		t.Helper()
		out := request(h, "POST", tasksPath+"/windows-delivery", testOrigin,
			map[string]string{"request_key": key, "manifest_sha256": sha}, admin, csrf)
		checkCode(t, out, 202, "")
		return responseValue[struct{ ID string }](t, out.Body.Bytes()).ID
	}
	task := queue("first", id)
	if queue("first", id) != task || submit(admin, csrf, "first", strings.Repeat("f", 64)).StatusCode != 409 {
		t.Fatal("idempotency did not bind request key to the manifest")
	}
	checkCode(t, request(h, "GET", tasksPath+"/"+task, "", nil, member, ""), 404, "task_not_found")

	run := func(expected string) {
		t.Helper()
		claim, worked, err := store.claimTaskTarget(ctx, time.Minute)
		if err != nil || !worked || claim.Task != task {
			t.Fatalf("cannot claim Windows verification: %+v, %v", claim, err)
		}
		if err := store.runWindowsDelivery(ctx, claim); err != nil {
			t.Fatal(err)
		}
		checkCode(t, request(h, "GET", tasksPath+"/"+task, "", nil, admin, ""), 200, expected)
	}
	sig := filepath.Join(spool, id, "release.sig")
	original, err := os.ReadFile(sig)
	if err != nil {
		t.Fatal(err)
	}
	windowsFixtureWrite(t, sig, []byte("invalid development signature"))
	run("windows_signature_invalid")
	retry := func() {
		t.Helper()
		var target string
		var version int64
		if err := store.DB.QueryRow(ctx, "SELECT id,version FROM task_targets WHERE task_id=$1", task).Scan(&target, &version); err != nil {
			t.Fatal(err)
		}
		checkCode(t, request(h, "POST", tasksPath+"/"+task+"/retry", testOrigin,
			map[string]any{"targets": []map[string]any{{"id": target, "version": version}}}, admin, csrf), 202, "")
	}
	retry()
	windowsFixtureWrite(t, sig, original)
	arm := filepath.Join(spool, id, "bundle", "DeskRealm-arm64.zip")
	archive, err := os.ReadFile(arm)
	if err != nil {
		t.Fatal(err)
	}
	windowsFixtureWrite(t, arm, bytes.Repeat([]byte{'*'}, len(archive)))
	run("windows_package_invalid")
	retry()
	windowsFixtureWrite(t, arm, archive)
	stale, worked, err := store.claimTaskTarget(ctx, time.Minute)
	if err != nil || !worked {
		t.Fatalf("cannot claim interrupted attempt: %v", err)
	}
	taskSQL(t, ctx, store, "UPDATE task_targets SET lease_until=clock_timestamp()-interval '1 second' WHERE task_id=$1", task)
	if err := store.runWindowsDelivery(ctx, stale); !errors.Is(err, errTaskLease) {
		t.Fatalf("stale verification should roll back: %v", err)
	}
	pending := request(h, "GET", tasksPath+"/"+task, "", nil, admin, "")
	if bytes.Contains(pending.Body.Bytes(), []byte("verified_at")) {
		t.Fatal("stale worker persisted verification")
	}
	run("windows_verified")
	verified := request(h, "GET", tasksPath+"/"+task, "", nil, admin, "")
	checkCode(t, verified, 200, store.WindowsDelivery.KeySHA())
	if !bytes.Contains(verified.Body.Bytes(), []byte("verified_at")) || !bytes.Contains(verified.Body.Bytes(), []byte(group)) {
		t.Fatal("verified evidence missing from task detail")
	}
	approval.Brand.Name = "不同品牌"
	task = queue("different-brand", windowsFixtureStage(t, spool, approval, private))
	run("windows_brand_conflict")
	task = queue("role-transferred-after-submit", id)
	checkCode(t, request(h, "POST", "/v1/admin/users/"+memberAccount.ID+"/transfer-platform-admin", testOrigin,
		map[string]string{"password": testPassword, "reason": "交付验权"}, admin, csrf), 204, "")
	nextLogin := loginRequest(h, memberAccount.Username, testPassword)
	checkCode(t, nextLogin, 204, "")
	admin = nextLogin.Result().Cookies()[0]
	run("authorization_changed")
	var unchanged bool
	if err := store.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM windows_delivery_requests WHERE verified_at IS NOT NULL)=1`).
		Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("rejected candidates changed verified records: %v", err)
	}
}
