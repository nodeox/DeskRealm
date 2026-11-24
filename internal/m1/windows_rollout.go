package m1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"deskrealm/internal/windowsdelivery"
	"github.com/jackc/pgx/v5"
)

const windowsBatchWindow = 30 * time.Minute

type windowsRollout struct {
	ID       string                 `json:"task_id"`
	Release  string                 `json:"release_id"`
	Rollback string                 `json:"rollback_release_id,omitempty"`
	Group    string                 `json:"group_id"`
	Batch    int                    `json:"batch"`
	Batches  int                    `json:"batches"`
	Revision int64                  `json:"revision"`
	State    string                 `json:"state"`
	Deadline time.Time              `json:"deadline"`
	Devices  []windowsRolloutDevice `json:"devices"`
	revoked  bool
}

type windowsRolloutDevice struct {
	ID     string `json:"device_id"`
	Batch  int    `json:"batch"`
	Target string `json:"target_id"`
}

type windowsRolloutRequest struct {
	RequestID string     `json:"request_id"`
	Group     string     `json:"group_id"`
	Rollback  string     `json:"rollback_release_id,omitempty"`
	Batches   [][]string `json:"batches"`
}

func (r windowsRolloutRequest) valid() bool {
	if !userIDPattern.MatchString(r.RequestID) || !userIDPattern.MatchString(r.Group) || len(r.Batches) == 0 || len(r.Batches) > 20 ||
		(r.Rollback != "" && !userIDPattern.MatchString(r.Rollback)) {
		return false
	}
	seen := make(map[string]bool)
	for _, batch := range r.Batches {
		if len(batch) == 0 || len(batch) > 50 {
			return false
		}
		for _, id := range batch {
			if !userIDPattern.MatchString(id) || seen[id] {
				return false
			}
			seen[id] = true
		}
		slices.Sort(batch)
	}
	return len(seen) <= 150
}

func readWindowsRollout(ctx context.Context, tx pgx.Tx, org, task string) (windowsRollout, error) {
	out := windowsRollout{ID: task}
	err := tx.QueryRow(ctx, `SELECT release_id,COALESCE(rollback_release_id::text,''),group_id,batch,batches,revision,opened_at,rel.revoked_at IS NOT NULL,
		CASE WHEN canceled_at IS NOT NULL THEN 'canceled'
		WHEN NOT EXISTS(SELECT 1 FROM task_targets WHERE task_id=$2 AND state<>'succeeded') THEN 'succeeded'
		WHEN rel.revoked_at IS NOT NULL OR EXISTS(SELECT 1 FROM task_targets WHERE task_id=$2 AND state='failed') THEN 'paused'
		WHEN EXISTS(SELECT 1 FROM windows_rollout_devices rd JOIN task_targets tt ON tt.id=rd.target_id
		WHERE rd.task_id=$2 AND rd.batch=r.batch AND tt.state='queued') THEN 'active'
		ELSE 'ready' END
		FROM windows_rollouts r JOIN windows_releases rel ON rel.id=r.release_id WHERE r.organization_id=$1 AND r.task_id=$2`, org, task).
		Scan(&out.Release, &out.Rollback, &out.Group, &out.Batch, &out.Batches, &out.Revision, &out.Deadline, &out.revoked, &out.State)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errTaskMissing
	}
	if err != nil {
		return out, err
	}
	out.Deadline = out.Deadline.Add(windowsBatchWindow)
	rows, err := tx.Query(ctx, `SELECT device_id,batch,target_id FROM windows_rollout_devices WHERE task_id=$1 ORDER BY batch,device_id`, task)
	if err != nil {
		return out, err
	}
	out.Devices, err = pgx.CollectRows(rows, pgx.RowToStructByPos[windowsRolloutDevice])
	return out, err
}

func (a *API) createWindowsRollout(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	releaseID := r.PathValue("id")
	var in windowsRolloutRequest
	if !readJSON(w, r, &in) {
		return
	}
	if !userIDPattern.MatchString(releaseID) || !in.valid() || in.Rollback == releaseID {
		failure(w, "create Windows rollout", errTaskInvalid)
		return
	}
	raw, err := json.Marshal(in)
	if err != nil {
		failure(w, "create Windows rollout", err)
		return
	}
	requestDigest := digest(releaseID + string(raw))
	ctx := r.Context()
	var out windowsRollout
	status := http.StatusOK
	err = a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		var task string
		var existing []byte
		err := tx.QueryRow(ctx, `SELECT task_id,request_digest FROM windows_rollouts WHERE organization_id=$1 AND request_id=$2`,
			who.Organization, in.RequestID).Scan(&task, &existing)
		if err == nil {
			if !bytes.Equal(existing, requestDigest) {
				return errTaskConflict
			}
			out, err = readWindowsRollout(ctx, tx, who.Organization, task)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if a.store.WindowsReleases == nil {
			return errWindowsDeliveryUnavailable
		}
		release, err := findDeploymentRelease(ctx, tx, who, windowsDeploymentToken{}, releaseID)
		if err != nil {
			return err
		}
		var published bool
		err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM windows_release_groups WHERE release_id=$1 AND group_id=$2)",
			releaseID, in.Group).Scan(&published)
		if err != nil {
			return err
		}
		if !published || release.Installer == nil || !release.RolloutOnly {
			return errTaskConflict
		}
		var prior windowsRelease
		if in.Rollback != "" {
			prior, err = findDeploymentRelease(ctx, tx, who, windowsDeploymentToken{}, in.Rollback)
			if err != nil {
				return err
			}
			if prior.Installer == nil {
				return errTaskConflict
			}
			if err := authorizeWindowsRolloutRelease(ctx, tx, windowsRollout{Release: prior.ID, Group: in.Group}); err != nil {
				return err
			}
		}
		err = tx.QueryRow(ctx, `INSERT INTO tasks(organization_id,kind,owner_id) VALUES($1,'windows_rollout',$2) RETURNING id`,
			who.Organization, who.ID).Scan(&task)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO windows_rollouts(task_id,organization_id,release_id,group_id,request_id,request_digest,batches,rollback_release_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,'')::uuid)`, task, who.Organization, release.ID, in.Group, in.RequestID, requestDigest, len(in.Batches), in.Rollback); err != nil {
			return err
		}
		for batch, ids := range in.Batches {
			for _, id := range ids {
				if err := addWindowsRolloutDevice(ctx, tx, who, task, in.Group, batch+1, id, release, prior); err != nil {
					return err
				}
			}
		}
		out, err = readWindowsRollout(ctx, tx, who.Organization, task)
		if err != nil {
			return err
		}
		status = http.StatusCreated
		return auditWindowsRollout(ctx, tx, who, task, release.ID, "created", "")
	})
	if err != nil {
		failure(w, "create Windows rollout", err)
		return
	}
	writeJSON(w, status, out)
}

func windowsRolloutArch(platform string) string {
	if platform == "windows-arm64" {
		return "arm64"
	}
	return "x64"
}

func addWindowsRolloutDevice(ctx context.Context, tx pgx.Tx, who actor, task, group string, batch int, id string, release, prior windowsRelease) error {
	var platform, bundle string
	var epoch int64
	var online bool
	err := tx.QueryRow(ctx, `SELECT platform,device_key_epoch,COALESCE(managed_bundle_sha,''),COALESCE(`+managedDeviceOnline+`,false) FROM devices d
		WHERE id=$1 AND group_id=$2 AND organization_id=$3 AND registration_state='approved' FOR SHARE`,
		id, group, who.Organization).Scan(&platform, &epoch, &bundle, &online)
	if errors.Is(err, pgx.ErrNoRows) {
		return errForbidden
	}
	if err != nil {
		return err
	}
	arch := windowsRolloutArch(platform)
	installer, desired := release.Installer.Installers[arch].SHA, release.Packages[arch].SHA
	if !windowsdelivery.ValidDigest(installer) || !windowsdelivery.ValidDigest(desired) || (online && bundle == desired) {
		return errTaskConflict
	}
	baseline := ""
	if prior.ID != "" {
		baseline = prior.Packages[arch].SHA
		if !online || !windowsdelivery.ValidDigest(baseline) || bundle != baseline {
			return errTaskConflict
		}
	}
	state, result := "waiting", "windows_batch_waiting"
	if batch == 1 {
		state, result = "queued", "windows_batch_open"
	}
	var target string
	if err := tx.QueryRow(ctx, `INSERT INTO task_targets(task_id,object_key,state,result_code,requested_by)
		VALUES($1,$2,$3,$4,$5) RETURNING id`, task, id, state, result, who.ID).Scan(&target); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO windows_rollout_devices(task_id,target_id,device_id,batch,platform,key_epoch,installer_sha,bundle_sha,baseline_sha)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''))`, task, target, id, batch, platform, epoch, installer, desired, baseline)
	if uniqueViolation(err) {
		return errTaskConflict
	}
	return err
}

func auditWindowsRollout(ctx context.Context, tx pgx.Tx, who actor, task, resource, action, reason string) error {
	_, err := addAuditEvent(ctx, tx, auditEvent{OrganizationID: who.Organization, ActorID: who.ID, TaskID: task,
		ResourceID: resource, Action: "windows_rollout." + action, Result: "accepted", Reason: reason})
	return err
}
