package m1

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"deskrealm/internal/windowsdelivery"
)

func TestWindowsReleasePostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	spool, center, verifier, private := windowsFixtureVerifier(t)
	store.WindowsDelivery = verifier
	publishedFiles := t.TempDir()
	var err error
	store.WindowsReleases, err = verifier.ConfigureReleases(publishedFiles)
	if err != nil {
		t.Fatal(err)
	}
	h, admin, csrf, _ := adminSession(t, ctx, store)
	reader := createMember(t, h, admin, csrf)
	member := loginRequest(h, reader.Username, testPassword).Result().Cookies()[0]
	group := func(name string) string {
		t.Helper()
		response := request(h, "POST", "/v1/admin/device-groups", testOrigin, map[string]string{"name": name}, admin, csrf)
		checkCode(t, response, 201, "")
		return responseValue[struct{ ID string }](t, response.Body.Bytes()).ID
	}
	allowedGroup, otherGroup := group("发布范围"), group("其他范围")
	grant := func(id string, revoke bool) {
		t.Helper()
		checkCode(t, request(h, "POST", "/v1/admin/device-groups/"+id+"/grants", testOrigin,
			deviceGroupGrant{Username: reader.Username, Revoke: revoke}, admin, csrf), 204, "")
	}
	grant(otherGroup, false)
	approval := windowsdelivery.Approval{Schema: 1, Version: "0.2.0-dev.1", Center: center,
		Brand: windowsdelivery.Brand{ID: "deskrealm", Revision: 1, Name: "端域"}, Groups: []string{allowedGroup},
		Source: "1234567890abcdef1234567890abcdef12345678"}
	register := func(key string) (string, string) {
		t.Helper()
		sha := windowsFixtureStage(t, spool, approval, private)
		response := request(h, "POST", tasksPath+"/windows-delivery", testOrigin,
			map[string]string{"request_key": key, "manifest_sha256": sha}, admin, csrf)
		checkCode(t, response, 202, "")
		return responseValue[struct{ ID string }](t, response.Body.Bytes()).ID, sha
	}
	verify := func(task string) {
		t.Helper()
		claim, worked, err := store.claimTaskTarget(ctx, time.Minute)
		if err != nil || !worked || claim.Task != task {
			t.Fatalf("cannot claim candidate: %+v %v", claim, err)
		}
		if err := store.runWindowsDelivery(ctx, claim); err != nil {
			t.Fatal(err)
		}
	}
	publish := func(task, token string, user *http.Cookie) *httptest.ResponseRecorder {
		return request(h, "POST", tasksPath+"/"+task+"/windows-release", testOrigin, struct{}{}, user, token)
	}
	task, sha := register("release-initial")
	checkCode(t, publish(task, csrf, admin), 409, errTaskConflict.Error())
	verify(task)
	checkCode(t, publish(task, csrfToken(member.Value), member), 403, "forbidden")
	checkCode(t, publish(task, "", admin), 403, "")

	// Two publishers can race while copying the same candidate. There must
	// be one release ID, one snapshot and one publication event.
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { responses <- publish(task, csrf, admin) }()
	}
	first, second := <-responses, <-responses
	if first.Code == 200 {
		first, second = second, first
	}
	checkCode(t, first, 201, "")
	checkCode(t, second, 200, "")
	release := responseValue[windowsRelease](t, first.Body.Bytes())
	if responseValue[windowsRelease](t, second.Body.Bytes()).ID != release.ID {
		t.Fatal("concurrent replay changed the release ID")
	}
	base := "/v1/windows-releases"
	link := base + "/" + release.ID
	checkCode(t, request(h, "GET", base, "", nil, member, ""), 200, `"releases":[]`)
	checkCode(t, request(h, "GET", link+"/x64", "", nil, member, ""), 404, errWindowsReleaseMissing.Error())
	checkCode(t, request(h, "GET", link+"/x64", "", nil, nil, ""), 401, errSession.Error())
	grant(allowedGroup, false)
	checkCode(t, request(h, "GET", base, "", nil, member, ""), 200, release.ID)
	checkCode(t, request(h, "GET", link+"/unlisted", "", nil, member, ""), 404, errWindowsReleaseMissing.Error())

	expected := make(map[string][]byte)
	for arch, pkg := range release.Packages {
		path := filepath.Join(spool, sha, "bundle", pkg.File)
		expected[arch], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// A bad later build or mutable staging bytes cannot replace any
		// published archive or affect its downloads.
		windowsFixtureWrite(t, path, []byte("changed staging bytes"))
		response := request(h, "GET", link+"/"+arch, "", nil, member, "")
		checkCode(t, response, 200, "")
		if !bytes.Equal(response.Body.Bytes(), expected[arch]) || response.Header().Get("Cache-Control") != "no-store" ||
			response.Header().Get("ETag") != `"`+pkg.SHA+`"` {
			t.Fatalf("download bytes or headers differ for %s: %v", arch, response.Header())
		}
	}
	checkCode(t, publish(task, csrf, admin), 200, release.ID)
	store.WindowsReleases, err = verifier.ConfigureReleases(publishedFiles)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := ParseOrigin(testOrigin, false)
	if err != nil {
		t.Fatal(err)
	}
	h = NewHandler(store, origin, nil)
	ranged := httptest.NewRequest("GET", testOrigin+link+"/arm64", nil)
	ranged.AddCookie(member)
	ranged.Header.Set("Range", "bytes=1-3")
	partial := httptest.NewRecorder()
	h.ServeHTTP(partial, ranged)
	checkCode(t, partial, 206, "")
	if !bytes.Equal(partial.Body.Bytes(), expected["arm64"][1:4]) {
		t.Fatalf("incorrect range: %x", partial.Body.Bytes())
	}

	approval.Version = "0.2.0-dev.2"
	newTask, newSHA := register("release-bad-update")
	verify(newTask)
	windowsFixtureWrite(t, filepath.Join(spool, newSHA, "release.sig"), []byte("changed signature"))
	checkCode(t, publish(newTask, csrf, admin), 400, errWindowsCandidateInvalid.Error())
	checkCode(t, request(h, "GET", link+"/x64", "", nil, member, ""), 200, "")
	grant(allowedGroup, true)
	checkCode(t, request(h, "GET", base, "", nil, member, ""), 200, `"releases":[]`)
	checkCode(t, request(h, "GET", link+"/x64", "", nil, member, ""), 404, errWindowsReleaseMissing.Error())
	grant(allowedGroup, false)

	revoke := "/v1/admin/windows-releases/" + release.ID + "/revoke"
	checkCode(t, request(h, "POST", revoke, testOrigin, struct{}{}, member, csrfToken(member.Value)), 403, "forbidden")
	for range 2 {
		checkCode(t, request(h, "POST", revoke, testOrigin, struct{}{}, admin, csrf), 204, "")
	}
	checkCode(t, request(h, "GET", base, "", nil, member, ""), 200, `"releases":[]`)
	checkCode(t, request(h, "GET", base, "", nil, admin, ""), 200, `"revoked_at"`)
	checkCode(t, request(h, "GET", link+"/x64", "", nil, admin, ""), 404, errWindowsReleaseMissing.Error())
	checkCode(t, publish(task, csrf, admin), 409, errTaskConflict.Error())
	var published, revoked int
	err = store.DB.QueryRow(ctx, `SELECT count(*) FILTER(WHERE action='windows_release.published'),
		count(*) FILTER(WHERE action='windows_release.revoked') FROM audit_events WHERE resource_id=$1`, release.ID).
		Scan(&published, &revoked)
	if err != nil || published != 1 || revoked != 1 {
		t.Fatalf("unexpected audit counts: %d %d %v", published, revoked, err)
	}
}
