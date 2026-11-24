package m1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"deskrealm/internal/windowsdelivery"
)

func TestWindowsRolloutPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	spool, center, verifier, key := windowsFixtureVerifier(t)
	store.WindowsDelivery = verifier
	var err error
	store.WindowsReleases, err = verifier.ConfigureReleases(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	signer := strings.Repeat("b", 64)
	installerVerifier, keyInstaller := configureInstallerFixture(t, verifier, signer)
	store.WindowsInstallers = installerVerifier
	_, admin, csrf, _ := adminSession(t, ctx, store)
	_, managed := managedTestKey(t)
	h := managedHandler(t, store, managed)
	group := deviceGroupFixture(t, h, admin, csrf, "发布目标").ID
	outside := deviceGroupFixture(t, h, admin, csrf, "范围之外").ID
	post := func(path string, body any, status int) *httptest.ResponseRecorder {
		t.Helper()
		reply := request(h, "POST", path, testOrigin, body, admin, csrf)
		checkCode(t, reply, status, "")
		return reply
	}
	publish := func(version string, rollout bool) windowsRelease {
		t.Helper()
		sha := windowsFixtureStage(t, spool, windowsdelivery.Approval{Schema: 1, Version: version, Center: center,
			Groups: []string{group}, Source: strings.Repeat("a", 40),
			Brand: windowsdelivery.Brand{ID: "deskrealm", Revision: 1, Name: "端域"}}, key)
		reply := post(tasksPath+"/windows-delivery", map[string]string{"request_key": version, "manifest_sha256": sha}, 202)
		task := responseValue[struct{ ID string }](t, reply.Body.Bytes()).ID
		claim, ok, err := store.claimTaskTarget(ctx, time.Minute)
		if err != nil || !ok || claim.Task != task {
			t.Fatalf("claim release validation: %+v %v", claim, err)
		}
		if err := store.runWindowsDelivery(ctx, claim); err != nil {
			t.Fatal(err)
		}
		reply = post(tasksPath+"/"+task+"/windows-release", map[string]bool{"rollout_only": rollout}, 201)
		release := responseValue[windowsRelease](t, reply.Body.Bytes())
		installer, _, _ := windowsInstallerFixture(t, spool, sha, signer, keyInstaller, version)
		reply = post("/v1/admin/windows-releases/"+release.ID+"/installers", map[string]string{"evidence_sha256": installer}, 201)
		return responseValue[windowsRelease](t, reply.Body.Bytes())
	}
	prior := publish("0.2.0-dev.1", false)
	next := publish("0.2.0-dev.2", true)
	type endpoint struct{ id, credential string }
	device := func(group, rustdesk string) endpoint {
		t.Helper()
		in, private := deviceProofFixture(t, h, group, credentialFixture(t, h, admin, csrf, group), rustdesk)
		reply := request(h, "POST", "/v1/enrollment/complete", "", in, nil, "")
		checkCode(t, reply, 200, "pending")
		id := responseValue[enrollmentResult](t, reply.Body.Bytes()).DeviceID
		post("/v1/admin/devices/"+id+"/approve", struct{}{}, 200)
		return endpoint{id, runtimeCredentialFixture(t, h, id, private)}
	}
	first, broken, later, foreign := device(group, "10743001"), device(group, "10743002"), device(group, "10743003"), device(outside, "10743004")
	taskSQL(t, ctx, store, "UPDATE devices SET platform='windows-arm64' WHERE id=$1", later.id)
	arch := func(d endpoint) string {
		if d.id == later.id {
			return "arm64"
		}
		return "x64"
	}
	heartbeat := func(d endpoint, bundle string) {
		t.Helper()
		checkCode(t, nativeRequest(h, "POST", deviceHeartbeatURL, d.credential,
			map[string]string{"device_id": d.id, "reported_build": "1.5.3", "bundle_sha256": bundle}), 204, "")
	}
	for _, d := range []endpoint{first, broken, later, foreign} {
		heartbeat(d, prior.Packages[arch(d)].SHA)
	}
	path := "/v1/admin/windows-releases/" + next.ID + "/rollouts"
	in := windowsRolloutRequest{RequestID: "abd18a20-ddb2-4dcd-baf8-08da09e2b6d9", Group: group, Rollback: prior.ID,
		Batches: [][]string{{first.id, broken.id}, {foreign.id}}}
	post(path, in, 403)
	in.Batches[1][0] = later.id
	member := createMember(t, h, admin, csrf)
	mCookie := loginRequest(h, member.Username, testPassword).Result().Cookies()[0]
	checkCode(t, request(h, "POST", path, testOrigin, in, mCookie, csrfToken(mCookie.Value)), 403, "")
	out := responseValue[windowsRollout](t, post(path, in, 201).Body.Bytes())
	in.Batches = [][]string{{broken.id, first.id}, {later.id}}
	if again := responseValue[windowsRollout](t, post(path, in, 200).Body.Bytes()); again.ID != out.ID {
		t.Fatal("identical request did not retain the task UUID")
	}
	in.Batches = [][]string{{first.id}, {later.id}}
	post(path, in, 409)
	issueToken := func(group string) string {
		t.Helper()
		reply := post("/v1/admin/api-tokens", apiTokenInput{Purpose: "Windows 分批验证", Action: "windows.deploy",
			Scope: "device_group", DeviceGroupID: group, LifetimeMinutes: 30}, 201)
		return responseValue[struct{ Token string }](t, reply.Body.Bytes()).Token
	}
	token, other := issueToken(group), issueToken(outside)
	download := func(d endpoint, credential string, code int) {
		t.Helper()
		path := deploymentReleasePrefix + next.ID + "/rollouts/" + out.ID + "/" + d.id
		reply := nativeRequest(h, "GET", path, credential, nil)
		checkCode(t, reply, code, "")
		if code == 200 && windowsFixtureSHA(reply.Body.Bytes()) != next.Installer.Installers[arch(d)].SHA {
			t.Fatal("incorrect architecture or installer digest")
		}
	}
	for _, path := range []string{"/v1/windows-releases/" + next.ID + "/x64", "/v1/windows-releases/" + next.ID + "/installers/x64"} {
		checkCode(t, request(h, "GET", path, "", nil, admin, ""), 404, "")
	}
	checkCode(t, nativeRequest(h, "GET", deploymentReleasePrefix+next.ID+"/installers/x64", token, nil), 404, "")
	download(first, other, 404)
	download(later, token, 404)
	download(first, token, 200)
	download(broken, token, 200)
	change := func(action string, d endpoint, code int, state string) {
		t.Helper()
		reply := post(tasksPath+"/"+out.ID+"/windows-rollout", windowsRolloutChange{Action: action, Revision: out.Revision, Device: d.id}, code)
		if code == http.StatusOK {
			out = responseValue[windowsRollout](t, reply.Body.Bytes())
			if out.State != state {
				t.Fatalf("%s resulted in %s, want %s", action, out.State, state)
			}
		}
	}
	change("advance", endpoint{}, 409, "")
	change("fail", broken, 200, "paused")
	change("advance", endpoint{}, 409, "")
	change("confirm_rollback", broken, 409, "")
	heartbeat(broken, prior.Packages["x64"].SHA)
	change("confirm_rollback", broken, 200, "paused")
	if os.Getenv("DESKREALM_WINDOWS_ROLLOUT_BROWSER") == "1" {
		checkRolloutBrowser(t, ctx, store, out.ID, next.Version)
	}
	heartbeat(first, next.Packages["x64"].SHA)
	change("review", endpoint{}, 200, "paused")
	change("retry", endpoint{}, 200, "active")
	download(first, token, 404)
	download(later, token, 404)
	download(broken, token, 200)
	heartbeat(broken, next.Packages["x64"].SHA)
	change("review", endpoint{}, 200, "ready")
	change("advance", endpoint{}, 200, "active")
	// A prior heartbeat with the right digest cannot satisfy the next batch.
	heartbeat(later, next.Packages["arm64"].SHA)
	download(later, token, 200)
	change("review", endpoint{}, 200, "active")
	taskSQL(t, ctx, store, "UPDATE windows_rollouts SET opened_at=clock_timestamp()-interval '31 minutes' WHERE task_id=$1", out.ID)
	download(later, token, 404)
	change("review", endpoint{}, 200, "paused")
	change("retry", endpoint{}, 200, "active")
	download(later, token, 200)
	heartbeat(later, next.Packages["arm64"].SHA)
	change("review", endpoint{}, 200, "succeeded")
	result := request(h, "GET", tasksPath+"/"+out.ID, "", nil, admin, "")
	checkCode(t, result, 200, "windows_batch_healthy")
	if strings.Count(result.Body.String(), `"result_code":"windows_batch_install_failed"`) != 1 {
		t.Fatal("original failed attempt was not retained on retry")
	}
	t.Log("Verified batch authorization, failure pause, compatible rollback proof, and retry preserving successful devices")
}

func checkRolloutBrowser(t *testing.T, ctx context.Context, store Store, task, version string) {
	t.Helper()
	root := filepath.Join("..", "..", "web")
	server := httptest.NewUnstartedServer(nil)
	origin, err := ParseOrigin("http://"+server.Listener.Addr().String(), true)
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = NewHandler(store, origin, os.DirFS(filepath.Join(root, "dist")))
	server.Start()
	defer server.Close()
	scratch := t.TempDir()
	cmd := exec.CommandContext(ctx, "npx", "--no-install", "playwright", "test", "tests/windows-rollout.spec.ts")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "DESKREALM_BROWSER_BASE="+server.URL, "DESKREALM_BROWSER_OUTPUT="+scratch,
		"DESKREALM_BROWSER_REPORT="+filepath.Join(scratch, "result.json"), "DESKREALM_BROWSER_ROLLOUT_TASK="+task,
		"DESKREALM_BROWSER_ROLLOUT_VERSION="+version)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Chromium Windows rollout: %v\n%s", err, out)
	} else {
		t.Logf("%s", out)
	}
}
