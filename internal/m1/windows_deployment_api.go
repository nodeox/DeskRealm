package m1

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const deploymentReleasePrefix = "/v1/deployment/windows-releases/"

type windowsDeploymentToken struct {
	ID, Group string
	Expires   *time.Time
}

func automationDeployment(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, deploymentReleasePrefix)
}

func isWindowsDeploymentPost(path string) bool {
	tail, found := strings.CutPrefix(path, deploymentReleasePrefix)
	if !found {
		return false
	}
	parts := strings.Split(tail, "/")
	if (len(parts) != 2 && len(parts) != 4) || !userIDPattern.MatchString(parts[0]) || parts[1] != "deployments" {
		return false
	}
	return len(parts) == 2 || (userIDPattern.MatchString(parts[2]) && parts[3] == "revoke")
}

func canUseWindowsDeploymentGroup(ctx context.Context, tx pgx.Tx, who actor, group string) (bool, error) {
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM device_groups dg WHERE dg.organization_id=$1 AND `+deviceGroupApprove+` AND dg.id=$4
	)`, who.Organization, who.ID, who.PlatformAdmin, group).Scan(&allowed)
	return allowed, err
}

func (a *API) withWindowsDeploymentActor(r *http.Request, write bool,
	run func(pgx.Tx, actor, windowsDeploymentToken) error) error {
	if !automationDeployment(r) {
		return a.withActor(r, write, func(tx pgx.Tx, who actor) error {
			return run(tx, who, windowsDeploymentToken{})
		})
	}
	credentials := a.readCredentials(r)
	if !credentials.fromAPI || !validToken(credentials.bearer) || len(r.Header.Values("Cookie")) != 0 {
		return errAPIToken
	}
	ctx := r.Context()
	options := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	if write {
		options = pgx.TxOptions{}
	}
	return pgx.BeginTxFunc(ctx, a.store.DB, options, func(tx pgx.Tx) error {
		if write {
			if _, err := tx.Exec(ctx, "SELECT singleton FROM organizations WHERE singleton FOR NO KEY UPDATE"); err != nil {
				return err
			}
		}
		var who actor
		var token windowsDeploymentToken
		err := tx.QueryRow(ctx, `SELECT u.id,u.organization_id,`+adminBinding+`,t.id,t.device_group_id,t.expires_at
			FROM api_tokens t JOIN users u ON u.id=t.user_id AND u.organization_id=t.organization_id
			WHERE t.digest=$1 AND t.action='windows.deploy' AND t.scope='device_group' AND t.revoked_at IS NULL
			AND t.expires_at>clock_timestamp() AND u.status='active'`, digest(credentials.bearer)).
			Scan(&who.ID, &who.Organization, &who.PlatformAdmin, &token.ID, &token.Group, &token.Expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return errAPIToken
		}
		if err != nil {
			return err
		}
		allowed, err := canUseWindowsDeploymentGroup(ctx, tx, who, token.Group)
		if err != nil {
			return err
		}
		if !allowed {
			return errAPIToken
		}
		return run(tx, who, token)
	})
}

func (a *API) getWindowsDeploymentRelease(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var release windowsRelease
	err := a.withWindowsDeploymentActor(r, false, func(tx pgx.Tx, who actor, token windowsDeploymentToken) error {
		if a.store.WindowsReleases == nil {
			return errWindowsDeliveryUnavailable
		}
		var err error
		release, err = findDeploymentRelease(ctx, tx, who, token, r.PathValue("id"))
		return err
	})
	if err != nil {
		failure(w, "read Windows deployment release", err)
		return
	}
	writeJSON(w, http.StatusOK, release)
}

func findDeploymentRelease(ctx context.Context, tx pgx.Tx, who actor, token windowsDeploymentToken, id string) (windowsRelease, error) {
	if !userIDPattern.MatchString(id) {
		return windowsRelease{}, errWindowsReleaseMissing
	}
	release, err := findWindowsRelease(ctx, tx, who, id)
	if err != nil {
		return windowsRelease{}, err
	}
	if release.Revoked != nil {
		return windowsRelease{}, errWindowsReleaseMissing
	}
	if token.ID != "" {
		allowed, err := windowsEnrollmentAllowed(ctx, tx, who, id, token.Group)
		if err != nil {
			return windowsRelease{}, err
		}
		if !allowed {
			return windowsRelease{}, errWindowsReleaseMissing
		}
	}
	return release, nil
}
