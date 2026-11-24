package m1

import (
	"context"
	"errors"
	"net/http"
	"time"

	"deskrealm/internal/windowsdelivery"
	"github.com/jackc/pgx/v5"
)

type windowsRolloutArtifact struct {
	arch, task, target, device, snapshot string
	pkg                                  windowsdelivery.Package
	published                            time.Time
}

func readRolloutArtifact(ctx context.Context, tx pgx.Tx, who actor, token windowsDeploymentToken, releaseID, task, device string) (windowsRolloutArtifact, error) {
	var out windowsRolloutArtifact
	if token.ID == "" {
		return out, errAPIToken
	}
	release, err := findDeploymentRelease(ctx, tx, who, token, releaseID)
	if err != nil {
		return out, err
	}
	if !release.RolloutOnly {
		return out, errTaskConflict
	}
	var platform, sha, bundle string
	err = tx.QueryRow(ctx, `SELECT rd.target_id,rd.platform,rd.installer_sha,rd.bundle_sha
		FROM windows_rollouts r JOIN windows_rollout_devices rd ON rd.task_id=r.task_id
		JOIN task_targets tt ON tt.id=rd.target_id JOIN devices d ON d.id=rd.device_id
		WHERE r.task_id=$1 AND r.release_id=$2 AND rd.device_id=$3 AND r.organization_id=$4 AND r.group_id=$5
		AND r.canceled_at IS NULL AND rd.batch=r.batch AND rd.held AND tt.state='queued'
		AND d.group_id=r.group_id AND d.registration_state='approved' AND d.device_key_epoch=rd.key_epoch
		AND d.platform=rd.platform AND clock_timestamp()<r.opened_at+$6::int*interval '1 second'`,
		task, releaseID, device, who.Organization, token.Group, int(windowsBatchWindow.Seconds())).
		Scan(&out.target, &platform, &sha, &bundle)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, errTaskMissing
	}
	if err != nil {
		return out, err
	}
	out.arch, out.task, out.device = windowsRolloutArch(platform), task, device
	out.snapshot, out.pkg, out.published = release.artifact(out.arch, true)
	if out.snapshot == "" || out.pkg.SHA != sha || release.Packages[out.arch].SHA != bundle {
		return windowsRolloutArtifact{}, errTaskConflict
	}
	return out, nil
}

func (a *API) downloadWindowsRolloutInstaller(w http.ResponseWriter, r *http.Request) {
	releaseID, task, device := r.PathValue("id"), r.PathValue("task"), r.PathValue("device")
	if !userIDPattern.MatchString(releaseID) || !userIDPattern.MatchString(task) || !userIDPattern.MatchString(device) {
		failure(w, "download Windows rollout installer", errTaskMissing)
		return
	}
	ctx := r.Context()
	var approved windowsRolloutArtifact
	err := a.withWindowsDeploymentActor(r, false, func(tx pgx.Tx, who actor, token windowsDeploymentToken) error {
		if a.store.WindowsReleases == nil {
			return errWindowsDeliveryUnavailable
		}
		var err error
		approved, err = readRolloutArtifact(ctx, tx, who, token, releaseID, task, device)
		return err
	})
	if err != nil {
		failure(w, "download Windows rollout installer", err)
		return
	}
	file, err := a.store.WindowsReleases.OpenInstaller(ctx, approved.snapshot, approved.arch, approved.pkg)
	if err != nil {
		failure(w, "download Windows rollout installer", releaseVerificationError(err))
		return
	}
	defer file.Close()
	err = a.withWindowsDeploymentActor(r, r.Method == http.MethodGet, func(tx pgx.Tx, who actor, token windowsDeploymentToken) error {
		current, err := readRolloutArtifact(ctx, tx, who, token, releaseID, task, device)
		if err != nil {
			return err
		}
		if current != approved {
			return errTaskConflict
		}
		if r.Method != http.MethodGet {
			return nil
		}
		cmd, err := tx.Exec(ctx, `UPDATE windows_rollout_devices SET started_at=clock_timestamp()
			WHERE target_id=$1 AND started_at IS NULL`, approved.target)
		if err != nil || cmd.RowsAffected() == 0 {
			return err
		}
		return auditWindowsRollout(ctx, tx, who, task, device, "installer_authorized", approved.pkg.SHA)
	})
	if err != nil {
		failure(w, "download Windows rollout installer", err)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Minute))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+approved.pkg.File+`"`)
	w.Header().Set("ETag", `"`+approved.pkg.SHA+`"`)
	http.ServeContent(w, r, approved.pkg.File, approved.published, file)
}
