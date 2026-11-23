package m1

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

const maxTaskAttempts = 5

var errTaskLease = errors.New("task lease is stale or expired")

type taskClaim struct {
	Target, Task, Organization, Requester, ObjectKey, Token string
	Kind, Operation                                         string
	Version                                                 int64
	Sequence                                                int
}

func (s Store) claimTaskTarget(ctx context.Context, lease time.Duration) (taskClaim, bool, error) {
	var claim taskClaim
	var worked bool
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var prior string
		var count int
		err := tx.QueryRow(ctx, `SELECT tt.id,t.id,t.organization_id,tt.requested_by,tt.object_key,
			COALESCE(tt.lease_token::text,''),tt.attempt_count,t.kind,COALESCE(tt.operation_id::text,'')
			FROM task_targets tt JOIN tasks t ON t.id=tt.task_id
			WHERE (t.kind='user_import' OR ($1 AND t.kind='delivery'))
			AND (tt.state='queued' OR (tt.state='running' AND tt.lease_until<=clock_timestamp()))
			ORDER BY tt.lease_until NULLS LAST,tt.id LIMIT 1 FOR UPDATE OF tt SKIP LOCKED`, s.Delivery != nil).Scan(
			&claim.Target, &claim.Task, &claim.Organization, &claim.Requester, &claim.ObjectKey, &prior, &count,
			&claim.Kind, &claim.Operation)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		worked = true
		if prior != "" {
			if _, err := tx.Exec(ctx, `UPDATE task_attempts SET result_code='lease_expired',finished_at=clock_timestamp()
				WHERE id=$1`, prior); err != nil {
				return err
			}
		}
		limit := maxTaskAttempts
		if claim.Kind == "delivery" {
			limit++ // One final reconciliation claim. It cannot dispatch a new operation.
		}
		if count >= limit {
			_, err := tx.Exec(ctx, `UPDATE task_targets SET state='failed',result_code='lease_expired',
				version=version+1,lease_token=NULL,lease_until=NULL WHERE id=$1`, claim.Target)
			if err != nil {
				return err
			}
			return recordTaskOutcome(ctx, tx, claim, prior, "lease_expired")
		}
		err = tx.QueryRow(ctx, `UPDATE task_targets SET state='running',version=version+1,attempt_count=attempt_count+1,
			lease_token=gen_random_uuid(),lease_until=clock_timestamp()+($2::bigint*interval '1 millisecond')
			WHERE id=$1 RETURNING lease_token,version`, claim.Target, lease.Milliseconds()).Scan(&claim.Token, &claim.Version)
		if err != nil {
			return err
		}
		claim.Sequence = count + 1
		_, err = tx.Exec(ctx, `INSERT INTO task_attempts(id,target_id,sequence,requested_by) VALUES ($1,$2,$3,$4)`,
			claim.Token, claim.Target, claim.Sequence, claim.Requester)
		return err
	})
	return claim, worked, err
}

func recordTaskOutcome(ctx context.Context, tx pgx.Tx, claim taskClaim, attemptID, code string) error {
	result := "failure"
	if code == "pending" || code == "exists" || code == "delivery_succeeded" {
		result = "success"
	}
	_, err := addAuditEvent(ctx, tx, auditEvent{OrganizationID: claim.Organization, ActorID: claim.Requester,
		TaskID: claim.Task, ResourceID: claim.Target, Action: "task.target_finished", Result: result,
		Reason: code, Source: "task_worker", SourceKey: attemptID})
	return err
}

func (s Store) finishTaskAttempt(ctx context.Context, claim taskClaim) (string, error) {
	var code string
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		// Match the organization-first locking order used by scoped writes.
		if _, err := tx.Exec(ctx, `SELECT singleton FROM organizations WHERE id=$1 FOR NO KEY UPDATE`, claim.Organization); err != nil {
			return err
		}
		var priorCode string
		var finalVersion int64
		var valid bool
		var count int
		err := tx.QueryRow(ctx, `SELECT COALESCE(a.result_code,''),COALESCE(a.final_version,0),tt.attempt_count,
			COALESCE(tt.lease_token=a.id AND tt.requested_by=a.requested_by AND tt.lease_until>clock_timestamp() AND tt.version=$6,false)
			FROM task_targets tt JOIN tasks t ON t.id=tt.task_id JOIN task_attempts a ON a.target_id=tt.id
			WHERE t.id=$1 AND t.organization_id=$2 AND tt.id=$3 AND a.requested_by=$4 AND a.id=$5 AND a.sequence>0
			AND t.kind='user_import' AND tt.object_key=$7 FOR UPDATE OF tt`, claim.Task, claim.Organization,
			claim.Target, claim.Requester, claim.Token, claim.Version, claim.ObjectKey).Scan(&priorCode, &finalVersion, &count, &valid)
		if errors.Is(err, pgx.ErrNoRows) {
			return errTaskLease
		}
		if err != nil {
			return err
		}
		if finalVersion == claim.Version+1 {
			code = priorCode
			return nil
		}
		if !valid {
			return errTaskLease
		}
		var allowed bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users u WHERE u.id=$1 AND u.organization_id=$2
			AND u.status='active' AND `+adminBinding+`)`, claim.Requester, claim.Organization).Scan(&allowed)
		if err != nil {
			return err
		}
		code = "authorization_changed"
		if allowed {
			code, err = importAccount(ctx, tx, claim.Organization, claim.Requester, claim.Task, claim.ObjectKey)
			if err != nil {
				return err
			}
		}
		state := "failed"
		if code == "pending" || code == "exists" {
			state = "succeeded"
		}
		retryable := state == "failed" && count < maxTaskAttempts
		return finalizeTaskAttempt(ctx, tx, claim, state, code, retryable)
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

func finalizeTaskAttempt(ctx context.Context, tx pgx.Tx, claim taskClaim, state, code string, retryable bool) error {
	var version int64
	err := tx.QueryRow(ctx, `UPDATE task_targets SET state=$2,result_code=$3,retryable=$4,version=version+1,
		lease_token=NULL,lease_until=NULL WHERE id=$1 AND lease_token=$5 AND version=$6
		AND lease_until>clock_timestamp() RETURNING version`, claim.Target, state, code, retryable, claim.Token, claim.Version).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return errTaskLease
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE task_attempts SET result_code=$2,final_version=$3,finished_at=clock_timestamp()
		WHERE id=$1`, claim.Token, code, version); err != nil {
		return err
	}
	return recordTaskOutcome(ctx, tx, claim, claim.Token, code)
}

// RunTaskWorker resumes queued retries and expired leases until ctx is canceled.
func (s Store) RunTaskWorker(ctx context.Context) {
	for ctx.Err() == nil {
		step, cancel := context.WithTimeout(ctx, 10*time.Second)
		claim, worked, err := s.claimTaskTarget(step, 30*time.Second)
		if err == nil && claim.Token != "" {
			if claim.Kind == "delivery" {
				err = s.runDelivery(step, claim)
			} else {
				_, err = s.finishTaskAttempt(step, claim)
			}
		}
		cancel()
		if err != nil && !errors.Is(err, errTaskLease) && ctx.Err() == nil {
			slog.Error("DeskRealm task worker step failed", "task_kind", claim.Kind)
		}
		if worked && err == nil {
			continue
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
	}
}
