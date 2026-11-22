package m1

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestAuditScopePostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, adminID := adminSession(t, ctx, store)
	createMember(t, h, admin, csrf)
	out := loginRequest(h, "member", testPassword)
	checkCode(t, out, 204, "")
	member := out.Result().Cookies()[0]
	post := func(who *http.Cookie, path string, value any) *httptest.ResponseRecorder {
		t.Helper()
		return request(h, "POST", path, testOrigin, value, who, csrfToken(who.Value))
	}
	get := func(who *http.Cookie, path string) *httptest.ResponseRecorder {
		t.Helper()
		return request(h, "GET", path, "", nil, who, "")
	}
	const events = "/v1/admin/audit-events"
	const grantRoot = "/v1/admin/audit-grants"
	checkCode(t, get(member, events), 403, "forbidden")
	a := deviceGroupFixture(t, h, admin, csrf, "历史设备组")
	b := deviceGroupFixture(t, h, admin, csrf, "当前设备组")
	c := deviceGroupFixture(t, h, admin, csrf, "查询设备组")
	device, _ := approvedDevice(t, h, admin, csrf, a.ID, "40101001")
	checkCode(t, post(admin, "/v1/admin/devices/"+device.ID+"/lifecycle/move", deviceChange{GroupID: b.ID, Version: device.Version}), 200, b.ID)
	other, _ := approvedDevice(t, h, admin, csrf, c.ID, "40101002")
	var firstEvent, secondEvent, sessionID, taskID string
	if err := store.DB.QueryRow(ctx, "SELECT gen_random_uuid(),gen_random_uuid()").Scan(&sessionID, &taskID); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRow(ctx, `SELECT id FROM audit_events WHERE resource_id=$1 AND action='device.approved'`, device.ID).Scan(&firstEvent); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRow(ctx, `SELECT id FROM audit_events WHERE resource_id=$1 AND action='device.approved'`, other.ID).Scan(&secondEvent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec(ctx, "UPDATE audit_events SET session_id=$3,task_id=$4 WHERE id IN ($1,$2)", firstEvent, secondEvent, sessionID, taskID); err != nil {
		t.Fatal(err)
	}

	checkCode(t, post(admin, grantRoot, auditGrant{ID: "unexpected", Username: "member", Scope: "device_group", Group: a.Name, CanExport: true}), 400, "invalid_audit_request")
	granted := map[string]any{"username": "member", "scope": "device_group", "group": a.Name, "can_export": true}
	checkCode(t, post(member, grantRoot, granted), 403, "forbidden")
	checkCode(t, post(admin, grantRoot, granted), 204, "")
	checkCode(t, post(admin, grantRoot, map[string]any{"username": "member", "scope": "device_group", "group": c.Name, "can_export": false}), 204, "")
	options := get(member, "/v1/admin/audit-options")
	checkCode(t, options, 200, `"can_export":true`)
	checkCode(t, options, 200, `"can_administer":false`)
	filters := url.Values{"action": {"device.approved"}, "session_id": {sessionID}, "task_id": {taskID},
		"from": {time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)},
		"to":   {time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}}.Encode()
	list := get(member, events+"?"+filters)
	checkCode(t, list, 200, firstEvent)
	checkCode(t, list, 200, secondEvent)
	first := get(member, events+"/"+firstEvent)
	checkCode(t, first, 200, a.ID)
	checkCode(t, first, 200, device.ID)
	for _, forbidden := range []string{b.ID, "reason", "source_key"} {
		if strings.Contains(first.Body.String(), forbidden) {
			t.Fatalf("scope or private metadata visible: %s", forbidden)
		}
	}
	checkCode(t, get(member, events+"?action=device.moved"), 200, device.ID)
	checkCode(t, get(member, events+"?resource_id=invalid"), 400, "invalid_audit_request")
	checkCode(t, get(member, events+"?status=bogus"), 400, "invalid_audit_request")
	checkCode(t, get(member, events+"?status=pending&status=confirmed"), 400, "invalid_audit_request")
	checkCode(t, post(member, events+"/export?action=audit.exported;status=pending", nil), 400, "invalid_audit_request")
	checkCode(t, get(member, events+"?from=2026-01-02T01:00:00Z&to=2026-01-01T01:00:00Z"), 400, "invalid_audit_request")
	checkCode(t, request(h, "POST", events+"/export?"+filters, testOrigin, nil, member, ""), 403, "csrf_denied")
	export := post(member, events+"/export?"+filters, nil)
	checkCode(t, export, 200, firstEvent)
	if export.Header().Get("Content-Type") != "text/csv; charset=utf-8" || strings.Contains(export.Body.String(), secondEvent) {
		t.Fatal("export exposed read-only scope")
	}
	var exports int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events e JOIN outbox o ON o.event_id=e.id
		WHERE actor_id<>$1 AND action='audit.exported' AND reason='1'`, adminID).Scan(&exports); err != nil || exports != 1 {
		t.Fatalf("export audit: %d %v", exports, err)
	}
	listGrant := get(admin, grantRoot)
	checkCode(t, listGrant, 200, a.Name)
	for _, grant := range responseValue[[]auditGrant](t, listGrant.Body.Bytes()) {
		if grant.Group == a.Name {
			checkCode(t, post(admin, grantRoot+"/"+grant.ID+"/revoke", nil), 204, "")
		}
	}
	checkCode(t, get(member, events+"/"+firstEvent), 404, "audit_event_not_found")
	checkCode(t, post(member, events+"/export", nil), 403, "forbidden")

	userGroupResponse := post(admin, "/v1/admin/groups", map[string]string{"name": "历史账号组"})
	checkCode(t, userGroupResponse, 201, "")
	userGroup := responseValue[group](t, userGroupResponse.Body.Bytes())
	created := post(admin, "/v1/admin/users", map[string]string{"username": "audited-user", "password": testPassword})
	checkCode(t, created, 201, "")
	audited := responseValue[managedUser](t, created.Body.Bytes())
	memberPath := "/v1/admin/groups/" + userGroup.ID + "/members"
	checkCode(t, post(admin, memberPath, map[string]string{"username": audited.Username}), 204, "")
	checkCode(t, post(admin, "/v1/admin/users/"+audited.ID+"/disable", map[string]string{"reason": "operator notes are private"}), 200, "disabled")
	checkCode(t, post(admin, memberPath, map[string]any{"username": audited.Username, "remove": true}), 204, "")
	checkCode(t, post(admin, grantRoot, map[string]any{"username": "member", "scope": "user_group", "group": userGroup.Name, "can_export": true}), 204, "")
	history := get(member, events+"?action=user.disabled")
	checkCode(t, history, 200, audited.ID)
	if strings.Contains(history.Body.String(), "operator notes are private") {
		t.Fatal("operator note leaked")
	}
}

func TestAuditReceiptsPostgres(t *testing.T) {
	ctx, store, _ := postgres(t)
	h, admin, csrf, adminID := adminSession(t, ctx, store)
	var orgID string
	if err := store.DB.QueryRow(ctx, "SELECT id FROM organizations WHERE singleton").Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	insert := func(event auditEvent) (string, error) {
		t.Helper()
		var id string
		err := pgx.BeginFunc(ctx, store.DB, func(tx pgx.Tx) error {
			var err error
			id, err = addAuditEvent(ctx, tx, event)
			return err
		})
		return id, err
	}
	appendEvent := func(key, receipt string) string {
		t.Helper()
		id, err := insert(auditEvent{OrganizationID: orgID, ActorID: adminID, ResourceID: adminID, Action: "task.dispatched", Result: "accepted",
			Source: "dispatch", SourceKey: key, ReceiptSource: receipt})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	var stable = auditEvent{OrganizationID: orgID, ActorID: adminID, Action: "task.dispatched", Result: "accepted",
		Source: "dispatch", SourceKey: "stable-1", ReceiptSource: "worker"}
	id, err := insert(stable)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := insert(stable); err != nil || again != id {
		t.Fatal("source idempotency failed", err)
	}
	stable.Result = "failure"
	if _, err := insert(stable); !errors.Is(err, errAuditConflict) {
		t.Fatalf("conflicting source event: %v", err)
	}
	receive := func(event, source, key, result string) error {
		t.Helper()
		return pgx.BeginFunc(ctx, store.DB, func(tx pgx.Tx) error {
			return receiveAuditReceipt(ctx, tx, orgID, event, source, key, result)
		})
	}
	const root = "/v1/admin/audit-events"
	get := func(path string) *httptest.ResponseRecorder { return request(h, "GET", path, "", nil, admin, "") }
	post := func(path string, value any) *httptest.ResponseRecorder {
		return request(h, "POST", path, testOrigin, value, admin, csrf)
	}
	checkCode(t, get(root+"?status=pending"), 200, id)
	if err := receive(id, "unknown", "receipt1", "success"); !errors.Is(err, errAuditConflict) {
		t.Fatalf("unexpected reporter accepted: %v", err)
	}
	checkCode(t, get(root+"/"+id), 200, `"receipt_state":"pending"`)

	if err := pgx.BeginFunc(ctx, store.DB, func(tx pgx.Tx) error {
		if err := receiveAuditReceipt(ctx, tx, orgID, id, "worker", "receipt1", "success"); err != nil {
			return err
		}
		return errAuditInvalid
	}); !errors.Is(err, errAuditInvalid) {
		t.Fatal("receipt rollback failed", err)
	}
	checkCode(t, get(root+"/"+id), 200, `"receipt_state":"pending"`)
	for range 2 {
		if err := receive(id, "worker", "receipt1", "success"); err != nil {
			t.Fatal(err)
		}
	}
	checkCode(t, get(root+"?status=duplicate"), 200, `"duplicate_receipts":1`)
	checkCode(t, get(root+"/"+id), 200, `"receipt_result":"success"`)
	if err := receive(id, "worker", "receipt1", "failure"); !errors.Is(err, errAuditConflict) {
		t.Fatal("conflicting result accepted", err)
	}
	pending := appendEvent("pending-2", "worker")
	if err := receive(pending, "worker", "receipt1", "success"); !errors.Is(err, errAuditConflict) {
		t.Fatal("reused receipt identity accepted", err)
	}

	recent := appendEvent("recent-3", "")
	undelivered := appendEvent("outbox-4", "")
	receivedRecently := appendEvent("received-5", "")
	acknowledgedRecently := appendEvent("acknowledged-6", "worker")
	if err := receive(acknowledgedRecently, "worker", "receipt2", "success"); err != nil {
		t.Fatal(err)
	}
	checkCode(t, post("/v1/admin/audit-retention", map[string]int{"days": 0}), 400, "invalid_audit_request")
	checkCode(t, post("/v1/admin/audit-retention", map[string]int{"days": 1}), 204, "")
	checkCode(t, get("/v1/admin/audit-options"), 200, `"retention_days":1`)
	for _, event := range []string{id, pending, undelivered, receivedRecently, acknowledgedRecently} {
		if _, err := store.DB.Exec(ctx, "UPDATE audit_events SET occurred_at=now()-interval '2 days',received_at=now()-interval '2 days' WHERE id=$1", event); err != nil {
			t.Fatal(err)
		}
		if event != undelivered {
			if _, err := store.DB.Exec(ctx, "UPDATE outbox SET delivered_at=now() WHERE event_id=$1", event); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := store.DB.Exec(ctx, "UPDATE audit_events SET received_at=now() WHERE id=$1", receivedRecently); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec(ctx, "UPDATE audit_events SET acknowledged_at=now()-interval '2 days' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	checkCode(t, post("/v1/admin/audit-retention/prune", nil), 200, `"removed":1`)
	checkCode(t, get(root+"/"+id), 404, "audit_event_not_found")
	for _, event := range []string{pending, recent, undelivered, receivedRecently, acknowledgedRecently} {
		checkCode(t, get(root+"/"+event), 200, event)
	}
	checkCode(t, get(root+"?action=audit.retention_pruned"), 200, "audit.retention_pruned")

	// Repeated timestamps still have a deterministic next page ordered by ID.
	if _, err := store.DB.Exec(ctx, `INSERT INTO audit_events(organization_id,action,result,occurred_at)
		SELECT $1,'test.pagination','success',statement_timestamp() FROM generate_series(1,101)`, orgID); err != nil {
		t.Fatal(err)
	}
	page := responseValue[auditPage](t, get(root+"?action=test.pagination").Body.Bytes())
	if len(page.Events) != 100 || page.Next == "" {
		t.Fatal("missing audit pagination")
	}
	last := responseValue[auditPage](t, get(root+"?action=test.pagination&before="+url.QueryEscape(page.Next)).Body.Bytes())
	if len(last.Events) != 1 || last.Next != "" {
		t.Fatal("wrong audit next page")
	}
	for _, event := range page.Events {
		if event.ID == last.Events[0].ID {
			t.Fatal("audit cursor repeated an event")
		}
	}
}
