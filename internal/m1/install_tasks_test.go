package m1

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"deskrealm/internal/installrecord"
)

func TestInstallationTaskImportPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, _ := adminSession(t, ctx, store)
	createMember(t, h, admin, csrf)
	login := loginRequest(h, "member", testPassword)
	checkCode(t, login, 204, "")
	member := login.Result().Cookies()[0]

	record := installrecord.New("0123456789abcdef0123456789abcdef")
	record.Revision = 9
	now := time.Unix(1_770_000_000, 0).UTC()
	for i := 0; i < 3; i++ {
		record.Steps[i].State = "completed"
		record.Steps[i].Attempts = 1
		record.Steps[i].FirstSuccess = now
		record.Steps[i].UpdatedAt = now
	}
	record.Steps[3] = installrecord.Step{Name: "health", State: "interrupted", Attempts: 1, UpdatedAt: now, FailureCode: "interrupted"}
	encode := func(record installrecord.Record) []byte {
		t.Helper()
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	importAs := func(who string, data []byte) (InstallationImport, error) {
		return store.ImportInstallation(ctx, who, bytes.NewReader(data))
	}
	data := encode(record)
	if _, err := importAs("member", data); !errors.Is(err, ErrInstallationRecordActor) {
		t.Fatalf("non-admin actor accepted: %v", err)
	}
	if _, err := importAs("admin", nil); !errors.Is(err, ErrInstallationRecordInvalid) {
		t.Fatalf("missing record accepted: %v", err)
	}
	unsafe := strings.TrimSuffix(string(data), "}") + `,"secret":"postgres://hidden:pw@database"}`
	if _, err := importAs("admin", []byte(unsafe)); !errors.Is(err, ErrInstallationRecordInvalid) {
		t.Fatalf("unexpected record fields accepted: %v", err)
	}

	var imports atomic.Int32
	var group sync.WaitGroup
	for range 6 {
		group.Go(func() {
			result, err := importAs("admin", data)
			if err != nil || result.TaskID != record.TaskID {
				t.Errorf("concurrent import: %+v %v", result, err)
			}
			if result.Imported {
				imports.Add(1)
			}
		})
	}
	group.Wait()
	if imports.Load() != 1 {
		t.Fatalf("expected one new snapshot, got %d", imports.Load())
	}
	assertAuditAndTargets := func(wantAudits int) map[string]string {
		t.Helper()
		var audits, count int
		if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events e JOIN outbox o ON e.id=o.event_id
			WHERE e.task_id=$1 AND e.action='task.install_record_imported' AND e.actor_id IS NOT NULL`, record.TaskID).Scan(&audits); err != nil || audits != wantAudits {
			t.Fatalf("unexpected audit count: %d %v", audits, err)
		}
		_, targets, attempts := taskDetail(t, h, admin, record.TaskID)
		targetIDs := make(map[string]string)
		for _, target := range targets {
			count++
			if target.CanRetry {
				t.Fatal("host step was exposed as a worker retry target")
			}
			targetIDs[target.ObjectKey] = target.ID
		}
		if count != 4 || len(attempts) != 0 {
			t.Fatalf("unexpected installation targets or worker attempts: %v %v", targets, attempts)
		}
		checkCode(t, taskRetry(h, admin, csrf, record.TaskID, targets[0]), 403, "forbidden")
		return targetIDs
	}
	initial := assertAuditAndTargets(1)
	checkCode(t, request(h, "GET", tasksPath+"/"+record.TaskID, "", nil, admin, ""), 200, "interrupted")
	checkCode(t, request(h, "GET", tasksPath+"/"+record.TaskID, "", nil, member, ""), 404, "task_not_found")
	visible := request(h, "GET", tasksPath, "", nil, member, "")
	checkCode(t, visible, 200, `"tasks":[]`)

	record.Steps[3].UpdatedAt = now.Add(time.Second)
	if _, err := importAs("admin", encode(record)); !errors.Is(err, ErrInstallationRecordConflict) {
		t.Fatalf("same revision with different content was accepted: %v", err)
	}
	record.Revision = 8
	if _, err := importAs("admin", encode(record)); !errors.Is(err, ErrInstallationRecordConflict) {
		t.Fatalf("unknown stale revision accepted: %v", err)
	}
	record.Revision = 11
	record.Steps[3] = installrecord.Step{Name: "health", State: "completed", Attempts: 2,
		UpdatedAt: now.Add(time.Minute), FirstSuccess: now.Add(time.Minute)}
	if result, err := importAs("admin", encode(record)); err != nil || !result.Imported || result.Revision != 11 {
		t.Fatalf("completed snapshot: %+v %v", result, err)
	}
	if replay, err := importAs("admin", data); err != nil || replay.Imported || replay.Revision != 9 {
		t.Fatalf("known historical replay: %+v %v", replay, err)
	}
	for name, id := range assertAuditAndTargets(2) {
		if id != initial[name] {
			t.Fatalf("target ID changed at %s", name)
		}
	}
	checkCode(t, request(h, "GET", tasksPath+"/"+record.TaskID, "", nil, admin, ""), 200, `"succeeded":4`)
	checkCode(t, request(h, "GET", tasksPath+"/"+record.TaskID+"?revision=9", "", nil, admin, ""), 200, "interrupted")
	checkCode(t, request(h, "GET", tasksPath+"/"+record.TaskID+"?revision=9", "", nil, member, ""), 404, "task_not_found")
	checkCode(t, request(h, "GET", tasksPath+"/"+record.TaskID+"?revision=10", "", nil, admin, ""), 404, "task_not_found")
}
