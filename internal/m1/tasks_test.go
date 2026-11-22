package m1

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

const tasksPath = "/v1/admin/tasks"

func taskImport(h http.Handler, admin *http.Cookie, csrf, key string, names ...string) *httptest.ResponseRecorder {
	return request(h, "POST", "/v1/admin/users/imports", testOrigin,
		map[string]any{"idempotency_key": key, "usernames": names}, admin, csrf)
}

func taskRetry(h http.Handler, user *http.Cookie, csrf, task string, targets ...taskTarget) *httptest.ResponseRecorder {
	items := make([]map[string]any, 0, len(targets))
	for _, target := range targets {
		items = append(items, map[string]any{"id": target.ID, "version": target.Version})
	}
	return request(h, "POST", tasksPath+"/"+task+"/retry", testOrigin, map[string]any{"targets": items}, user, csrf)
}

func taskDetail(t *testing.T, h http.Handler, user *http.Cookie, id string) (taskSummary, []taskTarget, []taskAttempt) {
	t.Helper()
	res := request(h, "GET", tasksPath+"/"+id, "", nil, user, "")
	checkCode(t, res, 200, id)
	detail := responseValue[struct {
		taskSummary
		Targets  []taskTarget
		Attempts []taskAttempt
	}](t, res.Body.Bytes())
	return detail.taskSummary, detail.Targets, detail.Attempts
}

func taskSQL(t *testing.T, ctx context.Context, s Store, sql string, args ...any) {
	t.Helper()
	if _, err := s.DB.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func TestTaskRetryPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, _ := adminSession(t, ctx, store)
	taskSQL(t, ctx, store, `ALTER TABLE users ADD CONSTRAINT refuse_retry CHECK (username <> 'retry-target') NOT VALID`)
	res := taskImport(h, admin, csrf, "batch-fixture-2026", "retry-target", "stable-target")
	checkCode(t, res, 200, "import_failed")
	batch := responseValue[importResult](t, res.Body.Bytes())
	summary, targets, attempts := taskDetail(t, h, admin, batch.ID)
	if summary.Failed != 1 || summary.Succeeded != 1 || len(targets) != 2 || len(attempts) != 2 ||
		!targets[0].CanRetry || targets[0].ObjectKey != "retry-target" || targets[1].CanRetry {
		t.Fatalf("unexpected import task: %+v %+v %+v", summary, targets, attempts)
	}
	failed, good := targets[0], targets[1]
	checkCode(t, taskRetry(h, admin, "", batch.ID, failed), 403, "csrf_denied")
	checkCode(t, taskRetry(h, admin, csrf, batch.ID, failed, good), 409, "task_target_conflict")
	_, targets, _ = taskDetail(t, h, admin, batch.ID)
	if targets[0] != failed {
		t.Fatal("mixed retry modified a failed item before rejecting the request")
	}
	taskSQL(t, ctx, store, `ALTER TABLE users DROP CONSTRAINT refuse_retry`)
	checkCode(t, taskRetry(h, admin, csrf, batch.ID, failed), 202, `"queued":1`)
	claim, worked, err := store.claimTaskTarget(ctx, time.Minute)
	if err != nil || !worked || claim.Target != failed.ID {
		t.Fatalf("expected only failed target to be claimed: %+v %v %v", claim, worked, err)
	}
	if code, err := store.finishTaskAttempt(ctx, claim); err != nil || code != "pending" {
		t.Fatalf("retry: %s %v", code, err)
	}
	summary, targets, attempts = taskDetail(t, h, admin, batch.ID)
	if summary.Succeeded != 2 || targets[1] != good || len(attempts) != 3 {
		t.Fatalf("successful sibling changed or retry missing: %+v %+v %+v", summary, targets, attempts)
	}
	var events int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events e JOIN outbox o ON o.event_id=e.id
		WHERE e.task_id=$1 AND e.action IN ('task.retry_requested','task.target_finished','user.imported','user.import_batch')`, batch.ID).Scan(&events); err != nil || events != 5 {
		t.Fatalf("task audit and outbox entries: %d %v", events, err)
	}

	createMember(t, h, admin, csrf)
	group := deviceGroupFixture(t, h, admin, csrf, "任务设备组")
	outside := deviceGroupFixture(t, h, admin, csrf, "其他任务组")
	checkCode(t, request(h, "POST", "/v1/admin/device-groups/"+group.ID+"/grants", testOrigin,
		deviceGroupGrant{Username: "member", Approve: true}, admin, csrf), 204, "")
	login := loginRequest(h, "member", testPassword)
	checkCode(t, login, 204, "")
	member := login.Result().Cookies()[0]
	memberCSRF := csrfToken(member.Value)
	enroll := func(group, key string) (deviceID string, task taskSummary, target taskTarget) {
		cred := credentialFixture(t, h, admin, csrf, group)
		proof, _ := deviceProofFixture(t, h, group, cred, key)
		out := request(h, "POST", "/v1/enrollment/complete", "", proof, nil, "")
		checkCode(t, out, 200, "pending")
		deviceID = responseValue[enrollmentResult](t, out.Body.Bytes()).DeviceID
		var taskID string
		if err := store.DB.QueryRow(ctx, `SELECT id FROM tasks WHERE device_id=$1`, deviceID).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		task, targets, _ := taskDetail(t, h, admin, taskID)
		if task.Waiting != 1 || len(targets) != 1 || targets[0].ObjectKey != key || targets[0].CanRetry {
			t.Fatalf("missing approval path: %+v %+v", task, targets)
		}
		return deviceID, task, targets[0]
	}
	deviceID, ownTask, ownTarget := enroll(group.ID, "10004021")
	_, outsideTask, outsideTarget := enroll(outside.ID, "10004022")
	list := request(h, "GET", tasksPath, "", nil, member, "")
	checkCode(t, list, 200, ownTask.ID)
	if strings.Contains(list.Body.String(), outsideTask.ID) || strings.Contains(list.Body.String(), batch.ID) {
		t.Fatal("task list exposed another scope")
	}
	checkCode(t, request(h, "GET", tasksPath+"/"+outsideTask.ID, "", nil, member, ""), 404, "task_not_found")
	checkCode(t, taskRetry(h, member, memberCSRF, outsideTask.ID, outsideTarget), 404, "task_not_found")
	checkCode(t, request(h, "POST", "/v1/admin/devices/"+deviceID+"/reject", testOrigin,
		struct{}{}, member, memberCSRF), 200, "rejected")
	summary, targets, attempts = taskDetail(t, h, member, ownTask.ID)
	if summary.Failed != 1 || len(attempts) != 1 || attempts[0].ResultCode != "rejected" || targets[0].CanRetry || targets[0].Version != ownTarget.Version+1 {
		t.Fatalf("rejection task: %+v %+v %+v", summary, targets, attempts)
	}
	checkCode(t, taskRetry(h, admin, csrf, ownTask.ID, targets[0]), 403, "forbidden")
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND device_group_id=$2
		AND action IN ('device.enrollment_pending','device.rejected')`, ownTask.ID, group.ID).Scan(&events); err != nil || events != 2 {
		t.Fatalf("enrollment task audit: %d %v", events, err)
	}
}

func TestTaskLeaseAndReceiptsPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, _ := adminSession(t, ctx, store)
	taskSQL(t, ctx, store, `ALTER TABLE users ADD CONSTRAINT refuse_retry CHECK (username <> 'lease-target') NOT VALID`)
	const key = "lease-fixture-2026"
	res := taskImport(h, admin, csrf, key, "lease-target")
	checkCode(t, res, 200, "import_failed")
	batch := responseValue[importResult](t, res.Body.Bytes())
	assertReplay := func() {
		replay := taskImport(h, admin, csrf, key, "lease-target")
		checkCode(t, replay, 200, "import_failed")
		if replay.Body.String() != res.Body.String() {
			t.Fatal("original import result changed on replay")
		}
	}
	assertReplay()
	checkCode(t, taskImport(h, admin, csrf, key, "different-target"), 409, "import_request_conflict")
	_, targets, _ := taskDetail(t, h, admin, batch.ID)
	checkCode(t, taskRetry(h, admin, csrf, batch.ID, targets[0]), 202, "")
	checkCode(t, taskRetry(h, admin, csrf, batch.ID, targets[0]), 409, "task_target_conflict")

	type claimResult struct {
		claim taskClaim
		err   error
	}
	claims := make(chan claimResult, 12)
	for range cap(claims) {
		go func() {
			claim, _, err := store.claimTaskTarget(ctx, time.Minute)
			claims <- claimResult{claim, err}
		}()
	}
	var first taskClaim
	for range cap(claims) {
		v := <-claims
		if v.err != nil {
			t.Fatal(v.err)
		}
		if v.claim.Token != "" {
			if first.Token != "" {
				t.Fatal("two workers claimed the same target")
			}
			first = v.claim
		}
	}
	if first.Token == "" {
		t.Fatal("no worker claimed queued target")
	}
	taskSQL(t, ctx, store, `UPDATE task_targets SET lease_until=now()-interval '1 second' WHERE id=$1`, first.Target)
	taskSQL(t, ctx, store, `ALTER TABLE users DROP CONSTRAINT refuse_retry`)
	if _, err := store.finishTaskAttempt(ctx, first); !errors.Is(err, errTaskLease) {
		t.Fatalf("worker finished after its lease expired: %v", err)
	}
	second, _, err := store.claimTaskTarget(ctx, time.Minute)
	if err != nil || second.Token == first.Token || second.Version <= first.Version {
		t.Fatalf("takeover: %+v %v", second, err)
	}
	if _, err := store.finishTaskAttempt(ctx, first); !errors.Is(err, errTaskLease) {
		t.Fatalf("old receipt accepted after takeover: %v", err)
	}
	bogus := second
	bogus.Version++
	if _, err := store.finishTaskAttempt(ctx, bogus); !errors.Is(err, errTaskLease) {
		t.Fatalf("mismatched receipt version accepted: %v", err)
	}
	taskSQL(t, ctx, store, `ALTER TABLE audit_events ADD CONSTRAINT refuse_task_receipt CHECK (action <> 'task.target_finished') NOT VALID`)
	if _, err := store.finishTaskAttempt(ctx, second); err == nil || errors.Is(err, errTaskLease) {
		t.Fatalf("missing audit failure: %v", err)
	}
	var accountExists bool
	if err := store.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE username='lease-target')`).Scan(&accountExists); err != nil || accountExists {
		t.Fatalf("account escaped rolled-back receipt: %v %v", accountExists, err)
	}
	summary, _, attempts := taskDetail(t, h, admin, batch.ID)
	if summary.Running != 1 || len(attempts) != 3 || attempts[1].ResultCode != "lease_expired" || attempts[2].FinishedAt != nil || attempts[2].ID != "" {
		t.Fatalf("invalid rollback or leaked active token: %+v %+v", summary, attempts)
	}
	taskSQL(t, ctx, store, `ALTER TABLE audit_events DROP CONSTRAINT refuse_task_receipt`)
	for range 2 {
		if code, err := store.finishTaskAttempt(ctx, second); err != nil || code != "pending" {
			t.Fatalf("completion or replay: %s %v", code, err)
		}
	}
	summary, _, attempts = taskDetail(t, h, admin, batch.ID)
	if summary.Succeeded != 1 || len(attempts) != 3 || attempts[2].ID != second.Token || attempts[2].ResultCode != "pending" {
		t.Fatalf("durable receipt: %+v %+v", summary, attempts)
	}
	var receipts int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events e JOIN outbox o ON o.event_id=e.id
		WHERE e.task_id=$1 AND e.action='task.target_finished' AND e.source_key=$2`, batch.ID, second.Token).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("duplicate completion receipts: %d %v", receipts, err)
	}
	assertReplay()

	t.Run("exhausted lease", func(t *testing.T) {
		taskSQL(t, ctx, store, `ALTER TABLE users ADD CONSTRAINT refuse_exhausted CHECK (username <> 'exhaust-target') NOT VALID`)
		reply := taskImport(h, admin, csrf, "exhaust-fixture-2026", "exhaust-target")
		checkCode(t, reply, 200, "import_failed")
		exhausted := responseValue[importResult](t, reply.Body.Bytes())
		_, targets, _ := taskDetail(t, h, admin, exhausted.ID)
		checkCode(t, taskRetry(h, admin, csrf, exhausted.ID, targets[0]), 202, "")
		for range maxTaskAttempts {
			claim, _, err := store.claimTaskTarget(ctx, time.Minute)
			if err != nil || claim.Token == "" || claim.Task != exhausted.ID {
				t.Fatalf("missing replacement attempt: %+v %v", claim, err)
			}
			taskSQL(t, ctx, store, `UPDATE task_targets SET lease_until=now()-interval '1 second' WHERE id=$1`, claim.Target)
		}
		claim, worked, err := store.claimTaskTarget(ctx, time.Minute)
		if err != nil || !worked || claim.Token != "" {
			t.Fatalf("expired lease should close without another attempt: %+v %v %v", claim, worked, err)
		}
		summary, targets, attempts := taskDetail(t, h, admin, exhausted.ID)
		if summary.Failed != 1 || targets[0].CanRetry || len(attempts) != maxTaskAttempts+1 ||
			attempts[maxTaskAttempts].ResultCode != "lease_expired" {
			t.Fatalf("retry limit: %+v %+v %+v", summary, targets, attempts)
		}
		checkCode(t, taskRetry(h, admin, csrf, exhausted.ID, targets[0]), 409, "task_target_conflict")
	})
}

func TestTaskMigrationPostgres(t *testing.T) {
	ctx, store, dsn := postgres(t)
	h, admin, csrf, _ := adminSession(t, ctx, store)
	reply := taskImport(h, admin, csrf, "legacy-import-fixture", "legacy-account")
	checkCode(t, reply, 200, "pending")
	legacy := responseValue[importResult](t, reply.Body.Bytes())
	group := deviceGroupFixture(t, h, admin, csrf, "迁移设备组")
	credential := credentialFixture(t, h, admin, csrf, group.ID)
	proof, _ := deviceProofFixture(t, h, group.ID, credential, "10004023")
	reply = request(h, "POST", "/v1/enrollment/complete", "", proof, nil, "")
	checkCode(t, reply, 200, "pending")
	device := responseValue[enrollmentResult](t, reply.Body.Bytes()).DeviceID
	checkCode(t, request(h, "POST", "/v1/admin/devices/"+device+"/approve", testOrigin, struct{}{}, admin, csrf), 200, "approved")
	taskSQL(t, ctx, store, "UPDATE devices SET rustdesk_id='10004024' WHERE id=$1", device)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 9); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	summary, targets, attempts := taskDetail(t, h, admin, legacy.ID)
	if summary.Succeeded != 1 || targets[0].ObjectKey != "legacy-account" || len(attempts) != 1 || attempts[0].ResultCode != "pending" {
		t.Fatalf("legacy import missing: %+v %+v %+v", summary, targets, attempts)
	}
	var taskID string
	if err := store.DB.QueryRow(ctx, "SELECT id FROM tasks WHERE device_id=$1", device).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	summary, targets, attempts = taskDetail(t, h, admin, taskID)
	if summary.Succeeded != 1 || targets[0].ObjectKey != "10004023" || len(attempts) != 1 || attempts[0].ResultCode != "approved" {
		t.Fatalf("legacy enrollment lost initial RustDesk ID or decision: %+v %+v %+v", summary, targets, attempts)
	}
}
