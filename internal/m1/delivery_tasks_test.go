package m1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"deskrealm/internal/windowsdelivery"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The external fixture keeps the executed effect and its receipt together on disk.
// It can be reopened by a new worker without retaining any center process state.
type diskDelivery struct {
	directory string
	mu        sync.Mutex
	dropReply bool
}

func (d *diskDelivery) read(op DeliveryOperation) ([]DeliveryReceipt, error) {
	if !userIDPattern.MatchString(op.ID) {
		return nil, errDeliveryReceipt
	}
	data, err := os.ReadFile(filepath.Join(d.directory, op.ID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var receipts []DeliveryReceipt
	if err := json.Unmarshal(data, &receipts); err != nil {
		return nil, err
	}
	return receipts, nil
}

func (d *diskDelivery) Lookup(_ context.Context, op DeliveryOperation) (*DeliveryReceipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	receipts, err := d.read(op)
	if err != nil || len(receipts) == 0 {
		return nil, err
	}
	return &receipts[len(receipts)-1], nil
}

func (d *diskDelivery) Execute(_ context.Context, op DeliveryOperation, predecessor string) (DeliveryReceipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	history, err := d.read(op)
	if err != nil {
		return DeliveryReceipt{}, err
	}
	expected := ""
	for _, receipt := range history {
		if expected == predecessor {
			return receipt, nil
		}
		expected = receipt.ID
	}
	if expected != predecessor || (len(history) != 0 && history[len(history)-1].Succeeded) {
		return DeliveryReceipt{}, errDeliveryReceipt
	}
	key := digest(op.ID + ":" + predecessor)
	result := DeliveryReceipt{
		ID:        fmt.Sprintf("%x-%x-%x-%x-%x", key[:4], key[4:6], key[6:8], key[8:10], key[10:16]),
		Operation: op, Succeeded: op.Object != "retry-target" || predecessor != "",
	}
	data, err := json.Marshal(append(history, result))
	if err != nil {
		return DeliveryReceipt{}, err
	}
	stage, err := os.CreateTemp(d.directory, ".receipt-")
	if err != nil {
		return DeliveryReceipt{}, err
	}
	defer os.Remove(stage.Name())
	_, writeErr := stage.Write(data)
	if err := errors.Join(writeErr, stage.Sync(), stage.Close()); err != nil {
		return DeliveryReceipt{}, err
	}
	if err := os.Rename(stage.Name(), filepath.Join(d.directory, op.ID)); err != nil {
		return DeliveryReceipt{}, err
	}
	if d.dropReply && op.Object == "crash-target" {
		return DeliveryReceipt{}, io.ErrUnexpectedEOF
	}
	return result, nil
}

func createDeliveryFixture(h http.Handler, user *http.Cookie, csrf, key string, objects ...string) *httptest.ResponseRecorder {
	return request(h, "POST", tasksPath+"/delivery", testOrigin,
		map[string]any{"request_key": key, "objects": objects}, user, csrf)
}

func cancelDeliveryFixture(h http.Handler, user *http.Cookie, csrf, task string, targets ...taskTarget) *httptest.ResponseRecorder {
	items := make([]map[string]any, 0, len(targets))
	for _, target := range targets {
		items = append(items, map[string]any{"id": target.ID, "version": target.Version})
	}
	return request(h, "POST", tasksPath+"/"+task+"/cancel", testOrigin, map[string]any{"targets": items}, user, csrf)
}

func TestDeliveryTaskRecoveryPostgres(t *testing.T) {
	ctx, store, dsn := postgres(t)
	external := &diskDelivery{directory: t.TempDir(), dropReply: true}
	store.Delivery = external
	h, admin, csrf, _ := adminSession(t, ctx, store)
	createMember(t, h, admin, csrf)
	memberLogin := loginRequest(h, "member", testPassword)
	checkCode(t, memberLogin, 204, "")
	member := memberLogin.Result().Cookies()[0]
	checkCode(t, createDeliveryFixture(h, member, csrfToken(member.Value), "not-allowed", "target"), 403, "forbidden")
	checkCode(t, createDeliveryFixture(h, admin, csrf, "bad-targets", "https://wrong.invalid"), 400, "invalid_task_request")

	first := createDeliveryFixture(h, admin, csrf, "delivery-run", "retry-target", "cancel-target", "crash-target", "keep-queued")
	checkCode(t, first, 202, "")
	task := responseValue[struct{ ID string }](t, first.Body.Bytes()).ID
	replay := createDeliveryFixture(h, admin, csrf, "delivery-run", "keep-queued", "crash-target", "cancel-target", "retry-target")
	checkCode(t, replay, 202, task)
	checkCode(t, createDeliveryFixture(h, admin, csrf, "delivery-run", "changed"), 409, "task_target_conflict")
	checkCode(t, request(h, "GET", tasksPath+"/"+task, "", nil, member, ""), 404, "task_not_found")
	_, targets, _ := taskDetail(t, h, admin, task)
	byName := make(map[string]taskTarget)
	for _, target := range targets {
		if !target.CanCancel || target.Operation == "" {
			t.Fatalf("target has no operation or cancel capability: %+v", target)
		}
		byName[target.ObjectKey] = target
	}
	checkCode(t, cancelDeliveryFixture(h, member, csrfToken(member.Value), task, byName["cancel-target"]), 404, "task_not_found")
	stale := byName["keep-queued"]
	stale.Version++
	checkCode(t, cancelDeliveryFixture(h, admin, csrf, task, byName["cancel-target"], stale), 409, "task_target_conflict")
	checkCode(t, cancelDeliveryFixture(h, admin, csrf, task, byName["cancel-target"], byName["keep-queued"]), 200, `"canceled":2`)

	type claimResult struct {
		taskClaim
		error
	}
	claims := make(chan claimResult, 8)
	for range cap(claims) {
		go func() {
			claim, _, err := store.claimTaskTarget(ctx, time.Minute)
			claims <- claimResult{claim, err}
		}()
	}
	claimed := make(map[string]taskClaim)
	for range cap(claims) {
		item := <-claims
		if item.error != nil {
			t.Fatal(item.error)
		}
		if item.Token != "" {
			if _, exists := claimed[item.ObjectKey]; exists {
				t.Fatal("workers claimed the same target twice")
			}
			claimed[item.ObjectKey] = item.taskClaim
		}
	}
	if len(claimed) != 2 {
		t.Fatalf("expected two remaining claims, got %d", len(claimed))
	}
	crash, retry := claimed["crash-target"], claimed["retry-target"]
	checkCode(t, cancelDeliveryFixture(h, admin, csrf, task,
		taskTarget{ID: crash.Target, Version: crash.Version}), 409, "task_target_conflict")
	if err := store.runDelivery(ctx, crash); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected missing reply after external write: %v", err)
	}
	if err := store.runDelivery(ctx, retry); err != nil {
		t.Fatal(err)
	}
	recorded, err := external.Lookup(ctx, crash.deliveryOperation())
	if err != nil || recorded == nil || !recorded.Succeeded {
		t.Fatal("external effect did not survive loss of worker reply")
	}
	taskSQL(t, ctx, store, `UPDATE task_targets SET lease_until=clock_timestamp()-interval '1s' WHERE id=$1`, crash.Target)
	restartedPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer restartedPool.Close()
	restarted := Store{DB: restartedPool, Delivery: &diskDelivery{directory: external.directory}}
	next, _, err := restarted.claimTaskTarget(ctx, time.Minute)
	if err != nil || next.Operation != crash.Operation || next.Token == crash.Token || next.Version <= crash.Version {
		t.Fatalf("invalid takeover: %+v %v", next, err)
	}
	if err := restarted.commitDelivery(ctx, crash, recorded, ""); !errors.Is(err, errTaskLease) {
		t.Fatalf("old lease receipt accepted: %v", err)
	}
	wrongObject := *recorded
	wrongObject.Operation.Object = retry.ObjectKey
	if err := restarted.commitDelivery(ctx, next, &wrongObject, ""); !errors.Is(err, errDeliveryReceipt) {
		t.Fatalf("wrong-object receipt accepted: %v", err)
	}
	if err := restarted.runDelivery(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := restarted.commitDelivery(ctx, next, recorded, ""); err != nil {
		t.Fatalf("exact receipt replay failed: %v", err)
	}
	summary, targets, _ := taskDetail(t, h, admin, task)
	if summary.Succeeded != 1 || summary.Failed != 1 || summary.Canceled != 2 {
		t.Fatalf("wrong mixed outcome: %+v", summary)
	}
	for _, target := range targets {
		if target.ObjectKey == retry.ObjectKey {
			checkCode(t, taskRetry(h, admin, csrf, task, target), 202, `"queued":1`)
		}
	}
	retryNext, _, err := restarted.claimTaskTarget(ctx, time.Minute)
	if err != nil || retryNext.Operation != retry.Operation {
		t.Fatalf("retry changed stable operation: %+v %v", retryNext, err)
	}
	failedReceipt, err := external.Lookup(ctx, retry.deliveryOperation())
	if err != nil || failedReceipt == nil {
		t.Fatal("failed receipt was lost")
	}
	if err := restarted.commitDelivery(ctx, retryNext, failedReceipt, ""); !errors.Is(err, errDeliveryReceipt) {
		t.Fatalf("previous receipt applied to the new lease: %v", err)
	}
	if err := restarted.commitDelivery(ctx, retry, failedReceipt, ""); !errors.Is(err, errTaskLease) {
		t.Fatalf("old completed lease accepted replay after a new claim: %v", err)
	}
	if err := restarted.runDelivery(ctx, retryNext); err != nil {
		t.Fatal(err)
	}
	summary, _, attempts := taskDetail(t, h, admin, task)
	var receipts, audit int
	for _, attempt := range attempts {
		if attempt.ReceiptID != "" {
			receipts++
		}
	}
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE task_id=$1 AND action='task.target_finished'`, task).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	history, err := external.read(crash.deliveryOperation())
	if err != nil || len(history) != 1 || receipts != 3 || audit != 3 || summary.Succeeded != 2 {
		t.Fatalf("external effect, receipt or audit duplicated: outcomes=%+v history=%d receipts=%d audit=%d", summary, len(history), receipts, audit)
	}

	// A lost reply on the last regular lease still has a lookup-only recovery.
	last := createDeliveryFixture(h, admin, csrf, "delivery-last-lease", "crash-target")
	checkCode(t, last, 202, "")
	lastTask := responseValue[struct{ ID string }](t, last.Body.Bytes()).ID
	taskSQL(t, ctx, store, `UPDATE task_targets SET attempt_count=$2 WHERE task_id=$1`, lastTask, maxTaskAttempts-1)
	final, _, err := store.claimTaskTarget(ctx, time.Minute)
	if err != nil || final.Sequence != maxTaskAttempts {
		t.Fatalf("expected final dispatch lease: %+v %v", final, err)
	}
	if err := store.runDelivery(ctx, final); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected lost final response: %v", err)
	}
	taskSQL(t, ctx, store, `UPDATE task_targets SET lease_until=clock_timestamp()-interval '1s' WHERE id=$1`, final.Target)
	reconcile, _, err := restarted.claimTaskTarget(ctx, time.Minute)
	if err != nil || reconcile.Sequence != maxTaskAttempts+1 || reconcile.Operation != final.Operation {
		t.Fatalf("expected final reconciliation: %+v %v", reconcile, err)
	}
	if err := restarted.runDelivery(ctx, reconcile); err != nil {
		t.Fatalf("could not reconcile success after last regular lease: %v", err)
	}
	summary, _, _ = taskDetail(t, h, admin, lastTask)
	if summary.Succeeded != 1 {
		t.Fatalf("final reconciliation lost the external outcome: %+v", summary)
	}
}

func TestDeliveryTaskBrowserPostgres(t *testing.T) {
	if os.Getenv("DESKREALM_DELIVERY_BROWSER") != "1" {
		t.Skip("run deskrealm-m3-test delivery-tasks")
	}
	ctx, store, _ := postgres(t)
	store.Delivery = &diskDelivery{directory: t.TempDir()}
	spool, center, verifier, private := windowsFixtureVerifier(t)
	store.WindowsDelivery = verifier
	h, admin, csrf, _ := adminSession(t, ctx, store)
	groupResponse := request(h, "POST", "/v1/admin/device-groups", testOrigin, map[string]string{"name": "Windows 工作台"}, admin, csrf)
	checkCode(t, groupResponse, 201, "")
	group := responseValue[struct{ ID string }](t, groupResponse.Body.Bytes()).ID
	manifest := windowsFixtureStage(t, spool, windowsdelivery.Approval{
		Schema: 1, Version: "0.1.0-dev.3", Brand: windowsdelivery.Brand{ID: "deskrealm", Name: "端域测试", Revision: 3},
		Groups: []string{group}, Source: "1234567890abcdef1234567890abcdef12345678", Center: center,
	}, private)
	first := createDeliveryFixture(h, admin, csrf, "browser-retry", "retry-target")
	checkCode(t, first, 202, "")
	task := responseValue[struct{ ID string }](t, first.Body.Bytes()).ID
	claim, _, err := store.claimTaskTarget(ctx, time.Minute)
	if err != nil || claim.Task != task {
		t.Fatalf("missing browser fixture claim: %+v %v", claim, err)
	}
	if err := store.runDelivery(ctx, claim); err != nil {
		t.Fatal(err)
	}
	second := createDeliveryFixture(h, admin, csrf, "browser-cancel", "cancel-target")
	checkCode(t, second, 202, "")
	cancelTask := responseValue[struct{ ID string }](t, second.Body.Bytes()).ID
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
	report := filepath.Join(scratch, "browser.json")
	command := exec.CommandContext(ctx, "npm", "run", "test:delivery-tasks")
	command.Dir = root
	command.Env = append(os.Environ(), "DESKREALM_BROWSER_BASE="+server.URL, "DESKREALM_BROWSER_OUTPUT="+scratch,
		"DESKREALM_BROWSER_REPORT="+report, "DESKREALM_BROWSER_DELIVERY_TASK="+task, "DESKREALM_BROWSER_CANCEL_TASK="+cancelTask,
		"DESKREALM_BROWSER_WINDOWS_MANIFEST="+manifest)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Chromium delivery task regression: %v\n%s", err, output)
	}
	reportData, err := os.ReadFile(report)
	if err != nil || !bytes.Contains(reportData, []byte(`"expected": 1`)) || !bytes.Contains(reportData, []byte(`"skipped": 0`)) {
		t.Fatalf("missing successful nonskipped Chromium delivery result: %v", err)
	}
	t.Logf("%s", output)
	summary, _, _ := taskDetail(t, h, admin, cancelTask)
	retried, _, _ := taskDetail(t, h, admin, task)
	if summary.Canceled != 1 || retried.Queued != 1 {
		t.Fatalf("browser actions did not persist: cancel=%+v retry=%+v", summary, retried)
	}
	var queued int
	if err := store.DB.QueryRow(ctx, `SELECT count(*) FROM windows_delivery_requests w JOIN task_targets t ON t.task_id=w.task_id
		WHERE w.manifest_sha256=$1 AND t.state='queued'`, manifest).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("browser did not queue the reviewed manifest: %d, %v", queued, err)
	}
}
