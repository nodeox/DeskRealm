package m1

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type windowsEnrollmentInput struct {
	Deployment  string `json:"deployment_id"`
	Group       string `json:"group_id"`
	Arch        string `json:"arch"`
	RustDeskID  string `json:"rustdesk_id"`
	PublicKey   string `json:"public_key"`
	Purpose     string `json:"purpose"`
	Minutes     int    `json:"lifetime_minutes"`
	AutoApprove bool   `json:"auto_approve"`
}

type windowsEnrollmentReceipt struct {
	Deployment string    `json:"deployment_id"`
	ID         string    `json:"id"`
	Release    string    `json:"release_id"`
	Group      string    `json:"group_id"`
	Platform   string    `json:"platform"`
	Credential string    `json:"credential,omitempty"`
	Expires    time.Time `json:"expires_at"`
	State      string    `json:"state"`
}

type windowsEnrollmentStatus struct {
	windowsEnrollmentReceipt
	Device          string `json:"device_id,omitempty"`
	Registration    string `json:"registration_state,omitempty"`
	ExpectedVersion *int64 `json:"expected_policy_version"`
	AppliedVersion  *int64 `json:"applied_policy_version"`
}

const windowsDeploymentState = `CASE WHEN e.consumed_at IS NOT NULL THEN 'consumed'
	WHEN b.revoked_at IS NOT NULL OR r.revoked_at IS NOT NULL OR t.revoked_at IS NOT NULL THEN 'revoked'
	WHEN e.expires_at<=clock_timestamp() OR t.expires_at<=clock_timestamp() THEN 'expired' ELSE 'issued' END`

// Reconstruct the original credential for authorized retries while storing only its digest.
func windowsEnrollmentSecret(key *ManagedSigningKey, organization, release, deployment string) string {
	mac := hmac.New(sha256.New, key.key.Seed())
	mac.Write([]byte("DeskRealm/windows-enrollment/v1\x00"))
	for _, id := range []string{organization, release, deployment} {
		mac.Write(managedFrameUUID(id))
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func windowsEnrollmentAllowed(ctx context.Context, tx pgx.Tx, who actor, release, group string) (bool, error) {
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM windows_release_groups rg JOIN device_groups dg ON dg.id=rg.group_id AND dg.organization_id=rg.organization_id
		WHERE rg.organization_id=$1 AND rg.release_id=$4 AND rg.group_id=$5 AND `+deviceGroupApprove+`
	)`, who.Organization, who.ID, who.PlatformAdmin, release, group).Scan(&allowed)
	return allowed, err
}

func (a *API) issueWindowsEnrollment(w http.ResponseWriter, r *http.Request) {
	if !automationDeployment(r) {
		if _, ok := a.sessionWrite(w, r); !ok {
			return
		}
	}
	ctx, release := r.Context(), r.PathValue("id")
	var in windowsEnrollmentInput
	if !readJSON(w, r, &in) {
		return
	}
	platform := map[string]string{"x64": "windows-amd64", "arm64": "windows-arm64"}[in.Arch]
	key, valid := devicePublicKey(in.PublicKey)
	in.Purpose = strings.TrimSpace(in.Purpose)
	if !valid || !userIDPattern.MatchString(release) || !userIDPattern.MatchString(in.Deployment) ||
		!userIDPattern.MatchString(in.Group) || platform == "" || !rustdeskIDPattern.MatchString(in.RustDeskID) ||
		!validOrganization(in.Purpose) || in.Minutes < 1 || in.Minutes > 1440 {
		failure(w, "issue Windows enrollment", errTaskInvalid)
		return
	}
	result := windowsEnrollmentReceipt{Deployment: in.Deployment, Release: release, Group: in.Group, Platform: platform}
	status := http.StatusOK
	err := a.withWindowsDeploymentActor(r, true, func(tx pgx.Tx, who actor, token windowsDeploymentToken) error {
		if a.managedKey == nil || a.store.WindowsReleases == nil {
			return errWindowsDeliveryUnavailable
		}
		if in.AutoApprove && (!who.PlatformAdmin || token.ID != "") {
			return errForbidden
		}
		if token.ID != "" && token.Group != in.Group {
			return errWindowsReleaseMissing
		}
		record, err := findDeploymentRelease(ctx, tx, who, token, release)
		if err != nil {
			return err
		}
		allowed, err := windowsEnrollmentAllowed(ctx, tx, who, record.ID, in.Group)
		if err != nil {
			return err
		}
		if record.Packages[in.Arch].File == "" || !allowed ||
			(token.ID != "" && (record.Installer == nil || record.Installer.Installers[in.Arch].File == "")) {
			return errWindowsReleaseMissing
		}
		secret := windowsEnrollmentSecret(a.managedKey, who.Organization, release, in.Deployment)
		hash := digest(secret)
		var stored, priorKey []byte
		var priorRelease, priorGroup, priorPlatform, priorRustDesk, priorPurpose, priorIssuer, priorToken string
		var priorMinutes int
		var priorAuto bool
		err = tx.QueryRow(ctx, `SELECT e.id,e.expires_at,e.issued_by,e.digest,b.release_id,e.group_id,e.platform,
			b.public_key,b.rustdesk_id,e.purpose,b.lifetime_minutes,b.auto_approve,COALESCE(b.api_token_id::text,''),`+windowsDeploymentState+`
			FROM windows_enrollment_bindings b JOIN enrollment_credentials e ON e.id=b.credential_id
			JOIN windows_releases r ON r.id=b.release_id LEFT JOIN api_tokens t ON t.id=b.api_token_id
			WHERE b.organization_id=$1 AND b.deployment_id=$2`, who.Organization, in.Deployment).
			Scan(&result.ID, &result.Expires, &priorIssuer, &stored, &priorRelease, &priorGroup, &priorPlatform,
				&priorKey, &priorRustDesk, &priorPurpose, &priorMinutes, &priorAuto, &priorToken, &result.State)
		if err == nil {
			if priorIssuer != who.ID || priorRelease != release || priorGroup != in.Group || priorPlatform != platform ||
				!bytes.Equal(priorKey, key) || priorRustDesk != in.RustDeskID || priorPurpose != in.Purpose ||
				priorMinutes != in.Minutes || priorAuto != in.AutoApprove || priorToken != token.ID {
				return errEnrollmentConflict
			}
			if result.State == "issued" {
				if !bytes.Equal(stored, hash) {
					return errEnrollmentConflict
				}
				result.Credential = secret
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO enrollment_credentials(organization_id,group_id,platform,purpose,digest,issued_by,expires_at)
			VALUES($1,$2,$3,$4,$5,$6,LEAST(clock_timestamp()+make_interval(mins=>$7),$8::timestamptz)) RETURNING id,expires_at`,
			who.Organization, in.Group, platform, in.Purpose, hash, who.ID, in.Minutes, token.Expires).
			Scan(&result.ID, &result.Expires); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO windows_enrollment_bindings(credential_id,organization_id,deployment_id,
			release_id,group_id,public_key,rustdesk_id,lifetime_minutes,auto_approve,api_token_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,'')::uuid)`, result.ID, who.Organization, in.Deployment, release,
			in.Group, key, in.RustDeskID, in.Minutes, in.AutoApprove, token.ID); err != nil {
			return err
		}
		result.State, result.Credential, status = "issued", secret, http.StatusCreated
		return auditWindowsEnrollment(ctx, tx, who, record.task, in.Group, in.Deployment, "windows_enrollment.issued", token.ID)
	})
	if err != nil {
		failure(w, "issue Windows enrollment", err)
		return
	}
	writeJSON(w, status, result)
}

func (a *API) getWindowsEnrollment(w http.ResponseWriter, r *http.Request) {
	ctx, release, deployment := r.Context(), r.PathValue("id"), r.PathValue("deployment")
	if !userIDPattern.MatchString(release) || !userIDPattern.MatchString(deployment) {
		failure(w, "read Windows enrollment", errWindowsReleaseMissing)
		return
	}
	var result windowsEnrollmentStatus
	result.Release, result.Deployment = release, deployment
	err := a.withWindowsDeploymentActor(r, false, func(tx pgx.Tx, who actor, token windowsDeploymentToken) error {
		err := tx.QueryRow(ctx, `SELECT e.id,e.group_id,e.platform,e.expires_at,`+windowsDeploymentState+`,
			COALESCE(d.id::text,''),COALESCE(d.registration_state,''),d.expected_policy_version,d.applied_policy_version
			FROM windows_enrollment_bindings b JOIN enrollment_credentials e ON e.id=b.credential_id
			JOIN windows_releases r ON r.id=b.release_id JOIN device_groups dg ON dg.id=b.group_id
			LEFT JOIN api_tokens t ON t.id=b.api_token_id
			LEFT JOIN enrollment_attempts ea ON ea.credential_id=e.id LEFT JOIN devices d ON d.id=ea.device_id
			WHERE b.organization_id=$1 AND b.release_id=$4 AND b.deployment_id=$5 AND `+deviceGroupApprove+`
			AND ($6='' OR b.api_token_id=NULLIF($6,'')::uuid)`,
			who.Organization, who.ID, who.PlatformAdmin, release, deployment, token.ID).
			Scan(&result.ID, &result.Group, &result.Platform, &result.Expires, &result.State, &result.Device,
				&result.Registration, &result.ExpectedVersion, &result.AppliedVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return errWindowsReleaseMissing
		}
		return err
	})
	if err != nil {
		failure(w, "read Windows enrollment", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) revokeWindowsEnrollment(w http.ResponseWriter, r *http.Request) {
	if !automationDeployment(r) {
		if _, ok := a.sessionWrite(w, r); !ok {
			return
		}
	}
	ctx, release, deployment := r.Context(), r.PathValue("id"), r.PathValue("deployment")
	if !userIDPattern.MatchString(release) || !userIDPattern.MatchString(deployment) {
		failure(w, "revoke Windows enrollment", errWindowsReleaseMissing)
		return
	}
	if !readJSON(w, r, &struct{}{}) {
		return
	}
	err := a.withWindowsDeploymentActor(r, true, func(tx pgx.Tx, who actor, token windowsDeploymentToken) error {
		var task, group string
		var consumed, revoked bool
		err := tx.QueryRow(ctx, `SELECT b.group_id,r.task_id,e.consumed_at IS NOT NULL,b.revoked_at IS NOT NULL
			FROM windows_enrollment_bindings b JOIN enrollment_credentials e ON e.id=b.credential_id
			JOIN windows_releases r ON r.id=b.release_id WHERE b.organization_id=$1 AND b.release_id=$2 AND b.deployment_id=$3
			AND ($4='' OR b.api_token_id=NULLIF($4,'')::uuid) FOR UPDATE OF b,e`,
			who.Organization, release, deployment, token.ID).Scan(&group, &task, &consumed, &revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return errWindowsReleaseMissing
		}
		if err != nil {
			return err
		}
		allowed, err := windowsEnrollmentAllowed(ctx, tx, who, release, group)
		if err != nil {
			return err
		}
		if !allowed {
			return errWindowsReleaseMissing
		}
		if consumed {
			return errEnrollmentConflict
		}
		if revoked {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE windows_enrollment_bindings SET revoked_at=clock_timestamp()
			WHERE organization_id=$1 AND deployment_id=$2`, who.Organization, deployment); err != nil {
			return err
		}
		return auditWindowsEnrollment(ctx, tx, who, task, group, deployment, "windows_enrollment.revoked", token.ID)
	})
	if err != nil {
		failure(w, "revoke Windows enrollment", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func auditWindowsEnrollment(ctx context.Context, tx pgx.Tx, who actor, task, group, deployment, action, tokenID string) error {
	_, err := addAuditEvent(ctx, tx, auditEvent{OrganizationID: who.Organization, ActorID: who.ID,
		TaskID: task, DeviceGroupID: group, ResourceID: deployment, Action: action, Result: "success", Reason: tokenID})
	return err
}

func finishWindowsEnrollment(ctx context.Context, tx pgx.Tx, e lockedEnrollment, attempt string, out *enrollmentResult) error {
	if err := auditWindowsEnrollment(ctx, tx, actor{Organization: e.Organization}, e.ReleaseTask, e.GroupID,
		e.Deployment, "windows_enrollment.completed", ""); err != nil {
		return err
	}
	if !e.AutoApprove {
		return nil
	}
	var stillAdmin bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users u WHERE u.id=$1 AND u.organization_id=$2
		AND u.status='active' AND `+adminBinding+`)`, e.Issuer, e.Organization).Scan(&stillAdmin)
	if err != nil || !stillAdmin {
		return err
	}
	issuer := actor{ID: e.Issuer, Organization: e.Organization}
	registered := device{ID: out.DeviceID, GroupID: e.GroupID, RegistrationState: out.State}
	if err := recordDeviceDecision(ctx, tx, issuer, &registered, "approved"); err != nil {
		return err
	}
	out.State = registered.RegistrationState
	_, err = tx.Exec(ctx, "UPDATE enrollment_attempts SET receipt_state=$2 WHERE id=$1", attempt, out.State)
	return err
}
