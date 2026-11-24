package m1

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

type windowsRolloutChange struct {
	Action   string `json:"action"`
	Revision int64  `json:"revision"`
	Device   string `json:"device_id"`
}

func (in windowsRolloutChange) valid() bool {
	if in.Revision < 1 {
		return false
	}
	switch in.Action {
	case "review", "advance", "retry", "cancel":
		return in.Device == ""
	case "fail", "confirm_rollback":
		return userIDPattern.MatchString(in.Device)
	default:
		return false
	}
}

func (a *API) changeWindowsRollout(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	task := r.PathValue("id")
	var in windowsRolloutChange
	if !readJSON(w, r, &in) {
		return
	}
	if !userIDPattern.MatchString(task) || !in.valid() {
		failure(w, "change Windows rollout", errTaskInvalid)
		return
	}
	ctx := r.Context()
	var out windowsRollout
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		rollout, err := readWindowsRollout(ctx, tx, who.Organization, task)
		if err != nil {
			return err
		}
		if rollout.Revision != in.Revision {
			return errTaskConflict
		}
		if rollout.State == "canceled" || rollout.State == "succeeded" {
			out = rollout
			return nil
		}
		reconciled, err := reconcileWindowsRollout(ctx, tx, who, rollout)
		if err != nil {
			return err
		}
		if reconciled {
			out, err = readWindowsRollout(ctx, tx, who.Organization, task)
			// Persist the newly observed outcomes before accepting any action
			// based on a stale view of the task.
			return err
		}
		changed, err := applyWindowsRolloutChange(ctx, tx, who, rollout, in)
		if err != nil {
			return err
		}
		if changed {
			if _, err := tx.Exec(ctx, "UPDATE windows_rollouts SET revision=revision+1 WHERE task_id=$1", task); err != nil {
				return err
			}
			resource := rollout.Release
			if in.Device != "" {
				resource = in.Device
			}
			if err := auditWindowsRollout(ctx, tx, who, task, resource, in.Action, ""); err != nil {
				return err
			}
		}
		out, err = readWindowsRollout(ctx, tx, who.Organization, task)
		return err
	})
	if err != nil {
		failure(w, "change Windows rollout", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func applyWindowsRolloutChange(ctx context.Context, tx pgx.Tx, who actor, rollout windowsRollout, in windowsRolloutChange) (bool, error) {
	const queued = `UPDATE task_targets tt SET state='queued',result_code='windows_batch_open',version=version+1,requested_by=$3
		FROM windows_rollout_devices rd WHERE rd.target_id=tt.id AND rd.task_id=$1`
	switch in.Action {
	case "review":
		return false, nil
	case "advance":
		if rollout.State != "ready" || rollout.Batch >= rollout.Batches {
			return false, errTaskConflict
		}
		if err := authorizeWindowsRolloutRelease(ctx, tx, rollout); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, queued+` AND rd.batch=$2 AND tt.state='waiting'`, rollout.ID, rollout.Batch+1, who.ID); err != nil {
			return false, err
		}
		_, err := tx.Exec(ctx, `UPDATE windows_rollouts SET batch=batch+1,opened_at=clock_timestamp() WHERE task_id=$1`, rollout.ID)
		return true, err
	case "retry":
		if rollout.State != "paused" {
			return false, errTaskConflict
		}
		if err := authorizeWindowsRolloutRelease(ctx, tx, rollout); err != nil {
			return false, err
		}
		var active, exhausted bool
		err := tx.QueryRow(ctx, `SELECT COALESCE(bool_or(tt.state='queued'),false),
			COALESCE(bool_or(tt.state='failed' AND tt.attempt_count>=$3),false)
			FROM windows_rollout_devices rd JOIN task_targets tt ON tt.id=rd.target_id WHERE rd.task_id=$1 AND rd.batch=$2`,
			rollout.ID, rollout.Batch, maxTaskAttempts).Scan(&active, &exhausted)
		if err != nil {
			return false, err
		}
		if active || exhausted {
			return false, errTaskConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE windows_rollout_devices rd SET started_at=NULL
			FROM task_targets tt WHERE rd.target_id=tt.id AND rd.task_id=$1 AND rd.batch=$2 AND tt.state='failed'`,
			rollout.ID, rollout.Batch); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, queued+` AND rd.batch=$2 AND tt.state='failed'`, rollout.ID, rollout.Batch, who.ID); err != nil {
			return false, err
		}
		_, err = tx.Exec(ctx, `UPDATE windows_rollouts SET opened_at=clock_timestamp() WHERE task_id=$1`, rollout.ID)
		return true, err
	case "fail":
		var target string
		err := tx.QueryRow(ctx, `SELECT tt.id FROM windows_rollout_devices rd JOIN task_targets tt ON tt.id=rd.target_id
			WHERE rd.task_id=$1 AND rd.batch=$2 AND rd.device_id=$3 AND tt.state='queued'`,
			rollout.ID, rollout.Batch, in.Device).Scan(&target)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, errTaskConflict
		}
		if err != nil {
			return false, err
		}
		return true, finishWindowsRolloutDevice(ctx, tx, who, rollout.ID, target, in.Device, "windows_batch_install_failed", false)
	case "confirm_rollback":
		return confirmWindowsRolloutRollback(ctx, tx, rollout, in.Device)
	case "cancel":
		var inFlight bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM windows_rollout_devices rd JOIN task_targets tt ON tt.id=rd.target_id
			WHERE rd.task_id=$1 AND rd.started_at IS NOT NULL AND tt.state='queued')`, rollout.ID).Scan(&inFlight)
		if err != nil {
			return false, err
		}
		if inFlight {
			return false, errTaskConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE task_targets SET state='canceled',result_code='windows_batch_canceled',version=version+1
			WHERE task_id=$1 AND state IN ('waiting','queued')`, rollout.ID); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE windows_rollout_devices SET held=false WHERE task_id=$1`, rollout.ID); err != nil {
			return false, err
		}
		_, err = tx.Exec(ctx, `UPDATE windows_rollouts SET canceled_at=clock_timestamp() WHERE task_id=$1`, rollout.ID)
		return true, err
	default:
		return false, errTaskInvalid
	}
}

func confirmWindowsRolloutRollback(ctx context.Context, tx pgx.Tx, rollout windowsRollout, device string) (bool, error) {
	if rollout.State != "paused" || rollout.Rollback == "" {
		return false, errTaskConflict
	}
	prior := windowsRollout{Release: rollout.Rollback, Group: rollout.Group}
	if err := authorizeWindowsRolloutRelease(ctx, tx, prior); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE task_targets tt SET result_code='windows_batch_rollback_verified',version=tt.version+1
		FROM windows_rollout_devices rd JOIN devices d ON d.id=rd.device_id
		WHERE rd.task_id=$1 AND rd.batch=$2 AND rd.device_id=$3 AND d.group_id=$4 AND rd.target_id=tt.id
		AND tt.state='failed' AND tt.result_code<>'windows_batch_rollback_verified' AND rd.key_epoch=d.device_key_epoch
		AND d.managed_bundle_sha=rd.baseline_sha AND (`+managedDeviceOnline+`)
		AND d.expected_policy_version IS NOT DISTINCT FROM d.applied_policy_version
		AND d.managed_heartbeat_at>(SELECT finished_at FROM task_attempts a WHERE a.target_id=tt.id AND a.sequence=tt.attempt_count)`,
		rollout.ID, rollout.Batch, device, rollout.Group)
	if err == nil && tag.RowsAffected() != 1 {
		return false, errTaskConflict
	}
	return err == nil, err
}

func authorizeWindowsRolloutRelease(ctx context.Context, tx pgx.Tx, rollout windowsRollout) error {
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM windows_releases r JOIN windows_release_groups g ON g.release_id=r.id
		WHERE r.id=$1 AND g.group_id=$2 AND r.revoked_at IS NULL AND r.installer_publication IS NOT NULL)`,
		rollout.Release, rollout.Group).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return errTaskConflict
	}
	return nil
}

func reconcileWindowsRollout(ctx context.Context, tx pgx.Tx, who actor, rollout windowsRollout) (bool, error) {
	row, err := tx.Query(ctx, `SELECT tt.id,rd.device_id,
		CASE WHEN d.group_id<>$3 OR d.device_key_epoch<>rd.key_epoch OR d.registration_state<>'approved'
			THEN 'windows_batch_device_changed'
		WHEN $5 THEN 'windows_batch_release_revoked'
		WHEN rd.started_at IS NOT NULL AND d.managed_bundle_sha=rd.bundle_sha AND d.managed_heartbeat_at>=rd.started_at
		   AND d.managed_heartbeat_epoch=rd.key_epoch AND d.managed_heartbeat_at<=$4
		   AND d.expected_policy_version IS NOT DISTINCT FROM d.applied_policy_version THEN 'windows_batch_healthy'
		WHEN clock_timestamp()>=$4 THEN 'windows_batch_health_timeout' ELSE '' END
		FROM windows_rollout_devices rd JOIN task_targets tt ON tt.id=rd.target_id JOIN devices d ON d.id=rd.device_id
		WHERE rd.task_id=$1 AND rd.batch=$2 AND tt.state='queued'`,
		rollout.ID, rollout.Batch, rollout.Group, rollout.Deadline, rollout.revoked)
	if err != nil {
		return false, err
	}
	type observation struct{ Target, Device, Result string }
	observed, err := pgx.CollectRows(row, pgx.RowToStructByPos[observation])
	if err != nil {
		return false, err
	}
	changed := false
	for _, o := range observed {
		if o.Result == "" {
			continue
		}
		if err := finishWindowsRolloutDevice(ctx, tx, who, rollout.ID, o.Target, o.Device, o.Result, o.Result == "windows_batch_healthy"); err != nil {
			return false, err
		}
		changed = true
	}
	if changed {
		_, err = tx.Exec(ctx, "UPDATE windows_rollouts SET revision=revision+1 WHERE task_id=$1", rollout.ID)
	}
	return changed, err
}

func finishWindowsRolloutDevice(ctx context.Context, tx pgx.Tx, who actor, task, target, device, code string, succeeded bool) error {
	state := "failed"
	if succeeded {
		state = "succeeded"
	}
	if _, err := tx.Exec(ctx, `WITH done AS (
		UPDATE task_targets SET state=$2,result_code=$3,version=version+1,attempt_count=attempt_count+1 WHERE id=$1
		RETURNING id,version,attempt_count,requested_by
	) INSERT INTO task_attempts(id,target_id,requested_by,sequence,result_code,final_version,started_at,finished_at)
	SELECT gen_random_uuid(),done.id,requested_by,attempt_count,$3,version,
		COALESCE(rd.started_at,r.opened_at),clock_timestamp() FROM done
		JOIN windows_rollout_devices rd ON rd.target_id=done.id JOIN windows_rollouts r ON r.task_id=rd.task_id`,
		target, state, code); err != nil {
		return err
	}
	if succeeded {
		if _, err := tx.Exec(ctx, "UPDATE windows_rollout_devices SET held=false WHERE target_id=$1", target); err != nil {
			return err
		}
	}
	_, err := addAuditEvent(ctx, tx, auditEvent{OrganizationID: who.Organization, ActorID: who.ID, TaskID: task,
		ResourceID: device, Action: "windows_rollout.device_result", Result: state, Reason: code})
	return err
}
