package m1

import (
	"net/http"
	"time"

	"deskrealm/internal/windowsdelivery"
	"github.com/jackc/pgx/v5"
)

type windowsInstallerPublication struct {
	windowsdelivery.InstallerEvidence
	EvidenceSHA string    `json:"evidence_sha256"`
	VerifierSHA string    `json:"verifier_key_sha256"`
	PublishedAt time.Time `json:"published_at"`
}

func (a *API) publishWindowsInstallers(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "publish Windows installers", errWindowsReleaseMissing)
		return
	}
	var input struct {
		EvidenceSHA string `json:"evidence_sha256"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !windowsdelivery.ValidDigest(input.EvidenceSHA) {
		failure(w, "publish Windows installers", errTaskInvalid)
		return
	}
	ctx := r.Context()
	verifier := a.store.WindowsInstallers
	read := func(tx pgx.Tx, who actor) (windowsRelease, error) {
		if !who.PlatformAdmin {
			return windowsRelease{}, errForbidden
		}
		if verifier == nil || a.store.WindowsReleases == nil {
			return windowsRelease{}, errWindowsDeliveryUnavailable
		}
		current, err := findWindowsRelease(ctx, tx, who, id)
		if err != nil {
			return windowsRelease{}, err
		}
		if current.Revoked != nil || (current.Installer != nil && current.Installer.EvidenceSHA != input.EvidenceSHA) {
			return windowsRelease{}, errTaskConflict
		}
		return current, nil
	}
	var release windowsRelease
	err := a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		var err error
		release, err = read(tx, who)
		return err
	})
	if err != nil {
		failure(w, "publish Windows installers", err)
		return
	}
	if release.Installer != nil {
		writeJSON(w, http.StatusOK, release)
		return
	}
	evidence, err := a.store.WindowsReleases.PreserveInstallers(ctx, verifier, input.EvidenceSHA, release.manifestSHA)
	if err != nil {
		failure(w, "publish Windows installers", releaseVerificationError(err))
		return
	}
	status := http.StatusOK
	err = a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		current, err := read(tx, who)
		if err != nil || current.Installer != nil {
			release = current
			return err
		}
		if current.manifestSHA != evidence.ReleaseSHA {
			return errTaskConflict
		}
		current.Installer = &windowsInstallerPublication{
			InstallerEvidence: evidence, EvidenceSHA: input.EvidenceSHA, VerifierSHA: verifier.KeySHA(),
		}
		if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&current.Installer.PublishedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE windows_releases SET installer_publication=$2 WHERE id=$1", id, current.Installer); err != nil {
			return err
		}
		release = current
		status = http.StatusCreated
		return auditWindowsRelease(ctx, tx, who, id, current.task, "windows_release.installers_published", input.EvidenceSHA)
	})
	if err != nil {
		failure(w, "publish Windows installers", err)
		return
	}
	writeJSON(w, status, release)
}
