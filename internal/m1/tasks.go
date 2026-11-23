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
}

type taskTarget struct {
	ID         string `json:"id"`
	ObjectKey  string `json:"object_key"`
	State      string `json:"state"`
	ResultCode string `json:"result_code"`
	CanRetry   bool   `json:"can_retry"`
	Version    int64  `json:"version"`
}

type taskAttempt struct {
	ID         string     `json:"id,omitempty"`
	TargetID   string     `json:"target_id"`
	Sequence   int        `json:"sequence"`
	ResultCode string     `json:"result_code,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

const taskScope = `t.organization_id=$1 AND ($3 OR (t.kind='user_import' AND t.owner_id=$2)
	OR (t.kind='device_enrollment' AND ` + deviceGroupRead + `))`

const scopedTaskFrom = ` FROM tasks t LEFT JOIN devices d ON d.id=t.device_id
	LEFT JOIN device_groups dg ON dg.id=d.group_id `

const taskSummaries = `SELECT t.id,t.kind,t.created_at,COALESCE(d.id::text,''),COALESCE(dg.name,''),
	count(*) FILTER(WHERE tt.state='waiting' AND tt.result_code<>'install_running'),count(*) FILTER(WHERE tt.state='queued'),
	count(*) FILTER(WHERE tt.state='running' OR tt.result_code='install_running'),count(*) FILTER(WHERE tt.state='succeeded'),
	count(*) FILTER(WHERE tt.state='failed')` + scopedTaskFrom + ` LEFT JOIN task_targets tt ON tt.task_id=t.id
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
		Tasks      []taskSummary `json:"tasks"`
		NextCursor string        `json:"next_cursor,omitempty"`
	}{}
	err := a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		if retry == "available" && !who.PlatformAdmin {
			return errForbidden
		}
		rows, err := tx.Query(ctx, taskSummaries+` AND ($4::timestamptz IS NULL OR (t.created_at,t.id)<($4,NULLIF($5,'')::uuid))`+
			` AND (NOT $7 OR (t.kind='user_import' AND EXISTS (
				SELECT 1 FROM task_targets target WHERE target.task_id=t.id AND target.retryable AND target.attempt_count<$8
			)))`+taskSummaryGroup+` ORDER BY t.created_at DESC,t.id DESC LIMIT $6`,
			who.Organization, who.ID, who.PlatformAdmin, before, beforeID, limit+1, retry == "available", maxTaskAttempts)
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
		Targets      []taskTarget          `json:"targets"`
		Attempts     []taskAttempt         `json:"attempts"`
		Installation *installrecord.Record `json:"installation,omitempty"`
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
		rows, err = tx.Query(ctx, `SELECT id,object_key,state,result_code,($2 AND retryable),version
			FROM task_targets WHERE task_id=$1 ORDER BY object_key,id`, id, who.PlatformAdmin && result.Kind == "user_import")
		if err != nil {
			return err
		}
		result.Targets, err = pgx.AppendRows(make([]taskTarget, 0), rows, pgx.RowToStructByPos[taskTarget])
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT CASE WHEN a.finished_at IS NULL THEN '' ELSE a.id::text END,
			a.target_id,a.sequence,COALESCE(a.result_code,''),a.started_at,a.finished_at
			FROM task_attempts a JOIN task_targets tt ON tt.id=a.target_id WHERE tt.task_id=$1
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
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "retry task", errTaskMissing)
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
		failure(w, "retry task", errTaskInvalid)
		return
	}
	ids := make([]string, 0, len(in.Targets))
	versions := make([]int64, 0, len(in.Targets))
	seen := make(map[string]bool, len(in.Targets))
	for _, target := range in.Targets {
		target.ID = strings.ToLower(target.ID)
		if !userIDPattern.MatchString(target.ID) || target.Version < 1 || seen[target.ID] {
			failure(w, "retry task", errTaskInvalid)
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
		if !who.PlatformAdmin || kind != "user_import" {
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
		failure(w, "retry task", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"queued": len(ids)})
}
