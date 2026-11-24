package m1

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"deskrealm/internal/installrecord"
	"github.com/jackc/pgx/v5"
)

var (
	errTaskInvalid  = errors.New("invalid_task_request")
	errTaskMissing  = errors.New("task_not_found")
	errTaskConflict = errors.New("task_target_conflict")
)

type taskSummary struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	DeviceID  string    `json:"device_id,omitempty"`
	Group     string    `json:"group,omitempty"`
	Waiting   int       `json:"waiting"`
	Queued    int       `json:"queued"`
	Running   int       `json:"running"`
	Succeeded int       `json:"succeeded"`
	Failed    int       `json:"failed"`
	Canceled  int       `json:"canceled"`
}

type taskTarget struct {
	ID         string `json:"id"`
	ObjectKey  string `json:"object_key"`
	State      string `json:"state"`
	ResultCode string `json:"result_code"`
	CanRetry   bool   `json:"can_retry"`
	Version    int64  `json:"version"`
	CanCancel  bool   `json:"can_cancel,omitempty"`
	Operation  string `json:"operation_id,omitempty"`
}

type taskAttempt struct {
	ID         string     `json:"id,omitempty"`
	TargetID   string     `json:"target_id"`
	Sequence   int        `json:"sequence"`
	ResultCode string     `json:"result_code,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ReceiptID  string     `json:"receipt_id,omitempty"`
}

const taskScope = `t.organization_id=$1 AND ($3 OR (t.kind='user_import' AND t.owner_id=$2)
	OR (t.kind='device_enrollment' AND ` + deviceGroupRead + `))`

const scopedTaskFrom = ` FROM tasks t LEFT JOIN devices d ON d.id=t.device_id
	LEFT JOIN device_groups dg ON dg.id=d.group_id `

const taskSummaries = `SELECT t.id,t.kind,t.created_at,COALESCE(d.id::text,''),COALESCE(dg.name,''),
	count(*) FILTER(WHERE tt.state='waiting' AND tt.result_code<>'install_running'),count(*) FILTER(WHERE tt.state='queued'),
	count(*) FILTER(WHERE tt.state='running' OR tt.result_code='install_running'),count(*) FILTER(WHERE tt.state='succeeded'),
	count(*) FILTER(WHERE tt.state='failed'),count(*) FILTER(WHERE tt.state='canceled')` + scopedTaskFrom + ` LEFT JOIN task_targets tt ON tt.task_id=t.id
	WHERE ` + taskScope

const taskSummaryGroup = ` GROUP BY t.id,d.id,dg.name `

func (a *API) listTasks(w http.ResponseWriter, r *http.Request) {
	const limit = 50
	retry := r.URL.Query().Get("retry")
	if retry != "" && retry != "available" {
		failure(w, "list tasks", errTaskInvalid)
		return
	}
	var before *time.Time
	var beforeID string
	if cursor := r.URL.Query().Get("before"); cursor != "" {
		stamp, id, found := strings.Cut(cursor, ",")
		t, err := time.Parse(time.RFC3339Nano, stamp)
		if !found || err != nil || !userIDPattern.MatchString(id) {
			failure(w, "list tasks", errTaskInvalid)
			return
		}
		before, beforeID = &t, id
	}
	ctx := r.Context()
	result := struct {
		Tasks            []taskSummary `json:"tasks"`
		NextCursor       string        `json:"next_cursor,omitempty"`
		CanVerifyWindows bool          `json:"can_verify_windows,omitempty"`
	}{}
	err := a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		if retry == "available" && !who.PlatformAdmin {
			return errForbidden
		}
		result.CanVerifyWindows = who.PlatformAdmin && a.store.WindowsDelivery != nil
		rows, err := tx.Query(ctx, taskSummaries+` AND ($4::timestamptz IS NULL OR (t.created_at,t.id)<($4,NULLIF($5,'')::uuid))`+
			` AND (NOT $7 OR ((t.kind='user_import' OR ($9 AND t.kind='delivery') OR ($10 AND t.kind='windows_delivery')) AND EXISTS (
				SELECT 1 FROM task_targets target WHERE target.task_id=t.id AND target.retryable AND target.attempt_count<$8
			)))`+taskSummaryGroup+` ORDER BY t.created_at DESC,t.id DESC LIMIT $6`,
			who.Organization, who.ID, who.PlatformAdmin, before, beforeID, limit+1, retry == "available", maxTaskAttempts,
			a.store.Delivery != nil, a.store.WindowsDelivery != nil)
		if err != nil {
			return err
		}
		result.Tasks, err = pgx.AppendRows(make([]taskSummary, 0), rows, pgx.RowToStructByPos[taskSummary])
		return err
	})
	if err != nil {
		failure(w, "list tasks", err)
		return
	}
	if len(result.Tasks) > limit {
		last := result.Tasks[limit-1]
		result.NextCursor = last.CreatedAt.UTC().Format(time.RFC3339Nano) + "," + last.ID
		result.Tasks = result.Tasks[:limit]
	}
	writeJSON(w, 200, result)
}

func (a *API) getTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "view task", errTaskMissing)
		return
	}
	ctx := r.Context()
	var revision int64
	if value := r.URL.Query().Get("revision"); value != "" {
		number, err := strconv.ParseInt(value, 10, 64)
		if err != nil || number < 1 {
			failure(w, "view task", errTaskInvalid)
			return
		}
		revision = number
	}
	result := struct {
		taskSummary
		Targets         []taskTarget          `json:"targets"`
		Attempts        []taskAttempt         `json:"attempts"`
		Installation    *installrecord.Record `json:"installation,omitempty"`
		WindowsDelivery *windowsTaskDetail    `json:"windows_delivery,omitempty"`
		WindowsRollout  *windowsRollout       `json:"windows_rollout,omitempty"`
	}{}
	err := a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		rows, err := tx.Query(ctx, taskSummaries+` AND t.id=$4`+taskSummaryGroup, who.Organization, who.ID, who.PlatformAdmin, id)
		if err != nil {
			return err
		}
		result.taskSummary, err = pgx.CollectOneRow(rows, pgx.RowToStructByPos[taskSummary])
		if errors.Is(err, pgx.ErrNoRows) {
			return errTaskMissing
		}
		if err != nil {
			return err
		}
		if result.Kind == "center_install" {
			result.Installation = new(installrecord.Record)
			err := tx.QueryRow(ctx, `SELECT snapshot FROM installation_task_records WHERE task_id=$1
				AND ($2::bigint=0 OR revision=$2) ORDER BY revision DESC LIMIT 1`, id, revision).Scan(result.Installation)
			if errors.Is(err, pgx.ErrNoRows) {
				return errTaskMissing
			}
			if err != nil {
				return err
			}
		} else if revision != 0 {
			return errTaskMissing
		}
		if result.Kind == windowsDeliveryKind {
			result.WindowsDelivery, err = readWindowsTask(ctx, tx, id)
			if err != nil {
				return err
			}
		}
		if result.Kind == "windows_rollout" {
			rollout, err := readWindowsRollout(ctx, tx, who.Organization, id)
			if err != nil {
				return err
			}
			result.WindowsRollout = &rollout
		}
		canRetry := who.PlatformAdmin && a.store.taskRetryEnabled(result.Kind)
		rows, err = tx.Query(ctx, `SELECT id,object_key,state,result_code,($2 AND retryable),version,
			($3 AND state='queued' AND attempt_count=0),COALESCE(operation_id::text,'')
			FROM task_targets WHERE task_id=$1 ORDER BY object_key,id`, id, canRetry,
			who.PlatformAdmin && (result.Kind == "delivery" || result.Kind == windowsDeliveryKind))
		if err != nil {
			return err
		}
		result.Targets, err = pgx.AppendRows(make([]taskTarget, 0), rows, pgx.RowToStructByPos[taskTarget])
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT CASE WHEN a.finished_at IS NULL THEN '' ELSE a.id::text END,
			a.target_id,a.sequence,COALESCE(a.result_code,''),a.started_at,a.finished_at,COALESCE(r.receipt_id::text,'')
			FROM task_attempts a JOIN task_targets tt ON tt.id=a.target_id
			LEFT JOIN delivery_task_receipts r ON r.attempt_id=a.id WHERE tt.task_id=$1
			ORDER BY a.target_id,a.sequence`, id)
		if err != nil {
			return err
		}
		result.Attempts, err = pgx.AppendRows(make([]taskAttempt, 0), rows, pgx.RowToStructByPos[taskAttempt])
		return err
	})
	if err != nil {
		failure(w, "view task", err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *API) retryTask(w http.ResponseWriter, r *http.Request) {
	a.changeTaskTargets(w, r, false)
}

func (a *API) cancelTask(w http.ResponseWriter, r *http.Request) {
	a.changeTaskTargets(w, r, true)
}

func (a *API) changeTaskTargets(w http.ResponseWriter, r *http.Request, cancel bool) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "change task targets", errTaskMissing)
		return
	}
	var in struct {
		Targets []struct {
			ID      string `json:"id"`
			Version int64  `json:"version"`
		} `json:"targets"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if len(in.Targets) == 0 || len(in.Targets) > 100 {
		failure(w, "change task targets", errTaskInvalid)
		return
	}
	ids := make([]string, 0, len(in.Targets))
	versions := make([]int64, 0, len(in.Targets))
	seen := make(map[string]bool, len(in.Targets))
	for _, target := range in.Targets {
		target.ID = strings.ToLower(target.ID)
		if !userIDPattern.MatchString(target.ID) || target.Version < 1 || seen[target.ID] {
			failure(w, "change task targets", errTaskInvalid)
			return
		}
		seen[target.ID] = true
		ids, versions = append(ids, target.ID), append(versions, target.Version)
	}
	ctx := r.Context()
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		var kind string
		err := tx.QueryRow(ctx, "SELECT t.kind"+scopedTaskFrom+" WHERE "+taskScope+" AND t.id=$4",
			who.Organization, who.ID, who.PlatformAdmin, id).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return errTaskMissing
		}
		if err != nil {
			return err
		}
		if !who.PlatformAdmin {
			return errForbidden
		}
		if cancel {
			if kind != "delivery" && kind != windowsDeliveryKind {
				return errForbidden
			}
			tag, err := tx.Exec(ctx, `UPDATE task_targets tt SET state='canceled',result_code='canceled',version=tt.version+1
				FROM unnest($2::text[],$3::bigint[]) AS selected(id,version)
				WHERE tt.task_id=$1 AND tt.id=selected.id::uuid AND tt.version=selected.version
				AND tt.state='queued' AND tt.attempt_count=0`, id, ids, versions)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != int64(len(ids)) {
				return errTaskConflict
			}
			for _, target := range ids {
				if _, err := addAuditEvent(ctx, tx, auditEvent{OrganizationID: who.Organization, ActorID: who.ID, TaskID: id,
					ResourceID: target, Action: "task.target_canceled", Result: "accepted"}); err != nil {
					return err
				}
			}
			return nil
		}
		if !a.store.taskRetryEnabled(kind) {
			return errForbidden
		}
		tag, err := tx.Exec(ctx, `UPDATE task_targets tt SET state='queued',retryable=false,version=tt.version+1,requested_by=$2
			FROM unnest($3::text[],$4::bigint[]) AS selected(id,version)
			WHERE tt.task_id=$1 AND tt.id=selected.id::uuid AND tt.version=selected.version
			AND tt.state='failed' AND tt.retryable AND tt.attempt_count<$5`, id, who.ID, ids, versions, maxTaskAttempts)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != int64(len(ids)) {
			return errTaskConflict
		}
		_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: who.Organization, ActorID: who.ID, TaskID: id,
			ResourceID: id, Action: "task.retry_requested", Result: "accepted"})
		return err
	})
	if err != nil {
		failure(w, "change task targets", err)
		return
	}
	if cancel {
		writeJSON(w, http.StatusOK, map[string]int{"canceled": len(ids)})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"queued": len(ids)})
}

func (s Store) taskRetryEnabled(kind string) bool {
	return kind == "user_import" || kind == "delivery" && s.Delivery != nil ||
		kind == windowsDeliveryKind && s.WindowsDelivery != nil
}
