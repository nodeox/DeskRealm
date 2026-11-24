package m1

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"deskrealm/internal/windowsdelivery"
	"github.com/jackc/pgx/v5"
)

var errWindowsReleaseMissing = errors.New("windows_release_not_found")

type windowsRelease struct {
	ID          string                             `json:"id"`
	Version     string                             `json:"version"`
	Scope       string                             `json:"signature_scope"`
	Packages    map[string]windowsdelivery.Package `json:"packages"`
	Published   time.Time                          `json:"published_at"`
	Revoked     *time.Time                         `json:"revoked_at,omitempty"`
	Installer   *windowsInstallerPublication       `json:"installer_publication,omitempty"`
	RolloutOnly bool                               `json:"rollout_only,omitempty"`
	manifestSHA string
	task        string
}

const windowsReleaseColumns = `SELECT r.id,r.version,r.signature_scope,r.packages,r.published_at,r.revoked_at,r.installer_publication,r.manifest_sha256,r.task_id,r.rollout_only`

const windowsReleaseAccess = ` FROM windows_releases r WHERE r.organization_id=$1 AND ($3 OR (
	r.revoked_at IS NULL AND EXISTS (
		SELECT 1 FROM windows_release_groups rg
		JOIN device_group_grants dg USING (organization_id,group_id)
		WHERE rg.release_id=r.id AND dg.user_id=$2)))`

func scanWindowsRelease(row pgx.Row) (windowsRelease, error) {
	var result windowsRelease
	err := row.Scan(&result.ID, &result.Version, &result.Scope, &result.Packages, &result.Published, &result.Revoked,
		&result.Installer, &result.manifestSHA, &result.task, &result.RolloutOnly)
	return result, err
}

func findWindowsRelease(ctx context.Context, tx pgx.Tx, who actor, id string) (windowsRelease, error) {
	result, err := scanWindowsRelease(tx.QueryRow(ctx, windowsReleaseColumns+windowsReleaseAccess+` AND r.id=$4`,
		who.Organization, who.ID, who.PlatformAdmin, id))
	if errors.Is(err, pgx.ErrNoRows) {
		err = errWindowsReleaseMissing
	}
	return result, err
}

func (a *API) listWindowsReleases(w http.ResponseWriter, r *http.Request) {
	const limit = 50
	var before *time.Time
	var beforeID string
	if cursor := r.URL.Query().Get("before"); cursor != "" {
		stamp, id, found := strings.Cut(cursor, ",")
		value, err := time.Parse(time.RFC3339Nano, stamp)
		if !found || err != nil || !userIDPattern.MatchString(id) {
			failure(w, "list Windows releases", errTaskInvalid)
			return
		}
		before, beforeID = &value, id
	}
	ctx := r.Context()
	result := struct {
		Releases             []windowsRelease `json:"releases"`
		Next                 string           `json:"next_cursor,omitempty"`
		CanPublishInstallers bool             `json:"can_publish_installers,omitempty"`
	}{Releases: []windowsRelease{}}
	err := a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		if a.store.WindowsReleases == nil {
			return errWindowsDeliveryUnavailable
		}
		result.CanPublishInstallers = who.PlatformAdmin && a.store.WindowsInstallers != nil
		rows, err := tx.Query(ctx, windowsReleaseColumns+windowsReleaseAccess+
			` AND ($4::timestamptz IS NULL OR (r.published_at,r.id)<($4,NULLIF($5,'')::uuid))
			ORDER BY r.published_at DESC,r.id DESC LIMIT $6`,
			who.Organization, who.ID, who.PlatformAdmin, before, beforeID, limit+1)
		if err != nil {
			return err
		}
		result.Releases, err = pgx.AppendRows(result.Releases, rows, func(row pgx.CollectableRow) (windowsRelease, error) {
			return scanWindowsRelease(row)
		})
		return err
	})
	if err != nil {
		failure(w, "list Windows releases", err)
		return
	}
	if len(result.Releases) > limit {
		last := result.Releases[limit-1]
		result.Next = last.Published.UTC().Format(time.RFC3339Nano) + "," + last.ID
		result.Releases = result.Releases[:limit]
	}
	writeJSON(w, http.StatusOK, result)
}

func readPublishableWindowsTask(ctx context.Context, tx pgx.Tx, who actor, task string) (*windowsTaskDetail, error) {
	if !who.PlatformAdmin {
		return nil, errForbidden
	}
	var state string
	err := tx.QueryRow(ctx, `SELECT tt.state FROM tasks t JOIN task_targets tt ON tt.task_id=t.id
		WHERE t.id=$1 AND t.organization_id=$2 AND t.kind='windows_delivery' AND tt.object_key='windows-bundle'`,
		task, who.Organization).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errTaskMissing
	}
	if err != nil {
		return nil, err
	}
	detail, err := readWindowsTask(ctx, tx, task)
	if err != nil {
		return nil, err
	}
	if state != "succeeded" || detail.VerifiedAt == nil {
		return nil, errTaskConflict
	}
	allowed, err := validWindowsGroups(ctx, tx, who.Organization, detail.Manifest.Approval.Groups)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, errForbidden
	}
	return detail, nil
}

func (a *API) publishWindowsRelease(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	task := r.PathValue("id")
	if !userIDPattern.MatchString(task) {
		failure(w, "publish Windows release", errTaskMissing)
		return
	}
	var input struct {
		RolloutOnly bool `json:"rollout_only"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	ctx := r.Context()
	var detail *windowsTaskDetail
	var release windowsRelease
	err := a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		if a.store.WindowsReleases == nil || a.store.WindowsDelivery == nil {
			return errWindowsDeliveryUnavailable
		}
		var err error
		detail, err = readPublishableWindowsTask(ctx, tx, who, task)
		if err != nil {
			return err
		}
		release, err = readPublishedManifest(ctx, tx, who.Organization, detail.ManifestSHA)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		failure(w, "publish Windows release", err)
		return
	}
	if release.ID != "" {
		if release.RolloutOnly != input.RolloutOnly {
			failure(w, "publish Windows release", errTaskConflict)
			return
		}
		writeJSON(w, http.StatusOK, release)
		return
	}
	verifier := a.store.WindowsDelivery
	if detail.VerifierKey != verifier.KeySHA() {
		failure(w, "publish Windows release", errWindowsCandidateInvalid)
		return
	}
	manifest, err := a.store.WindowsReleases.Preserve(ctx, verifier, detail.ManifestSHA)
	if err != nil {
		failure(w, "publish Windows release", releaseVerificationError(err))
		return
	}
	status := http.StatusOK
	err = a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		current, err := readPublishableWindowsTask(ctx, tx, who, task)
		if err != nil {
			return err
		}
		if current.ManifestSHA != detail.ManifestSHA || current.VerifierKey != detail.VerifierKey {
			return errTaskConflict
		}
		release, err = readPublishedManifest(ctx, tx, who.Organization, detail.ManifestSHA)
		if err == nil {
			if release.RolloutOnly != input.RolloutOnly {
				return errTaskConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		release = windowsRelease{Version: manifest.Approval.Version, Scope: manifest.Scope, Packages: manifest.Packages, RolloutOnly: input.RolloutOnly}
		if err := tx.QueryRow(ctx, `INSERT INTO windows_releases(organization_id,task_id,manifest_sha256,version,signature_scope,
			packages,verifier_key_sha256,rollout_only) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id,published_at`,
			who.Organization, task, detail.ManifestSHA, release.Version, release.Scope, release.Packages, detail.VerifierKey, input.RolloutOnly).
			Scan(&release.ID, &release.Published); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO windows_release_groups(organization_id,release_id,group_id)
			SELECT $1,$2,id FROM unnest($3::uuid[]) AS groups(id)`, who.Organization, release.ID, manifest.Approval.Groups); err != nil {
			return err
		}
		status = http.StatusCreated
		return auditWindowsRelease(ctx, tx, who, release.ID, task, "windows_release.published", "")
	})
	if err != nil {
		failure(w, "publish Windows release", err)
		return
	}
	writeJSON(w, status, release)
}

func readPublishedManifest(ctx context.Context, tx pgx.Tx, organization, digest string) (windowsRelease, error) {
	release, err := scanWindowsRelease(tx.QueryRow(ctx, windowsReleaseColumns+
		` FROM windows_releases r WHERE organization_id=$1 AND manifest_sha256=$2`, organization, digest))
	if err == nil && release.Revoked != nil {
		return windowsRelease{}, errTaskConflict
	}
	return release, err
}

func (a *API) revokeWindowsRelease(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "revoke Windows release", errWindowsReleaseMissing)
		return
	}
	if !readJSON(w, r, &struct{}{}) {
		return
	}
	ctx := r.Context()
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		release, err := findWindowsRelease(ctx, tx, who, id)
		if err != nil || release.Revoked != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE windows_releases SET revoked_at=clock_timestamp() WHERE id=$1`, id); err != nil {
			return err
		}
		return auditWindowsRelease(ctx, tx, who, id, release.task, "windows_release.revoked", "")
	})
	if err != nil {
		failure(w, "revoke Windows release", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) downloadWindowsRelease(w http.ResponseWriter, r *http.Request) {
	a.downloadWindowsArtifact(w, r, false)
}

func (a *API) downloadWindowsInstaller(w http.ResponseWriter, r *http.Request) {
	a.downloadWindowsArtifact(w, r, true)
}

func (r windowsRelease) artifact(arch string, installer bool) (string, windowsdelivery.Package, time.Time) {
	if !installer {
		return r.manifestSHA, r.Packages[arch], r.Published
	}
	if r.Installer == nil {
		return "", windowsdelivery.Package{}, time.Time{}
	}
	return r.Installer.EvidenceSHA, r.Installer.Installers[arch], r.Installer.PublishedAt
}

func (a *API) downloadWindowsArtifact(w http.ResponseWriter, r *http.Request, installer bool) {
	id, arch := r.PathValue("id"), r.PathValue("arch")
	if !userIDPattern.MatchString(id) || (arch != "x64" && arch != "arm64") {
		failure(w, "download Windows release", errWindowsReleaseMissing)
		return
	}
	ctx := r.Context()
	var release windowsRelease
	err := a.withWindowsDeploymentActor(r, false, func(tx pgx.Tx, who actor, token windowsDeploymentToken) error {
		if a.store.WindowsReleases == nil {
			return errWindowsDeliveryUnavailable
		}
		var err error
		release, err = findDeploymentRelease(ctx, tx, who, token, id)
		return err
	})
	if err != nil {
		failure(w, "download Windows release", err)
		return
	}
	snapshot, pkg, published := release.artifact(arch, installer)
	if release.RolloutOnly || pkg.File == "" {
		failure(w, "download Windows release", errWindowsReleaseMissing)
		return
	}
	var file *os.File
	contentType, reason := "application/zip", arch
	if installer {
		contentType, reason = "application/octet-stream", "installer:"+arch
		file, err = a.store.WindowsReleases.OpenInstaller(ctx, snapshot, arch, pkg)
	} else {
		file, err = a.store.WindowsReleases.Open(ctx, snapshot, arch, pkg)
	}
	if err != nil {
		failure(w, "download Windows release", releaseVerificationError(err))
		return
	}
	defer file.Close()

	// Renew the access check after hashing. Group membership and revocation
	// can change while the complete archive is being read.
	err = a.withWindowsDeploymentActor(r, r.Method == http.MethodGet, func(tx pgx.Tx, who actor, token windowsDeploymentToken) error {
		current, err := findDeploymentRelease(ctx, tx, who, token, id)
		if err != nil {
			return err
		}
		currentSnapshot, currentPackage, _ := current.artifact(arch, installer)
		if current.Revoked != nil || current.RolloutOnly || currentSnapshot != snapshot || currentPackage != pkg {
			return errWindowsReleaseMissing
		}
		if r.Method == http.MethodGet {
			return auditWindowsRelease(ctx, tx, who, id, current.task, "windows_release.download_authorized", reason)
		}
		return nil
	})
	if err != nil {
		failure(w, "download Windows release", err)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Minute))
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+pkg.File+`"`)
	w.Header().Set("ETag", `"`+pkg.SHA+`"`)
	http.ServeContent(w, r, pkg.File, published, file)
}

func releaseVerificationError(err error) error {
	switch {
	case errors.Is(err, windowsdelivery.ErrUnavailable):
		return errWindowsDeliveryUnavailable
	case errors.Is(err, windowsdelivery.ErrManifest), errors.Is(err, windowsdelivery.ErrSignature), errors.Is(err, windowsdelivery.ErrPackage):
		return errWindowsCandidateInvalid
	default:
		return err
	}
}

func auditWindowsRelease(ctx context.Context, tx pgx.Tx, who actor, release, task, action, reason string) error {
	_, err := addAuditEvent(ctx, tx, auditEvent{OrganizationID: who.Organization, ActorID: who.ID,
		TaskID: task, ResourceID: release, Action: action, Result: "success", Reason: reason})
	return err
}
