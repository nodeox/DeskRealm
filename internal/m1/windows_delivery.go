package m1

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"deskrealm/internal/relayprotocol"
	"deskrealm/internal/windowsdelivery"
	"github.com/jackc/pgx/v5"
)

const windowsDeliveryKind = "windows_delivery"

var (
	errWindowsDeliveryUnavailable = errors.New("windows_delivery_unavailable")
	errWindowsCandidateMissing    = errors.New("windows_candidate_missing")
	errWindowsCandidateInvalid    = errors.New("windows_candidate_invalid")
)

type windowsTaskDetail struct {
	ManifestSHA string                   `json:"manifest_sha256"`
	Manifest    windowsdelivery.Manifest `json:"manifest"`
	VerifiedAt  *time.Time               `json:"verified_at,omitempty"`
	VerifierKey string                   `json:"verifier_key_sha256,omitempty"`
}

func ConfigureWindowsDelivery(directory, keyFile, origin, idServer, rendezvousKey, relayID, relayAddress string,
	signing *ManagedSigningKey) (*windowsdelivery.Verifier, error) {
	if directory == "" && keyFile == "" {
		return nil, nil
	}
	rendezvous, err := base64.StdEncoding.Strict().DecodeString(rendezvousKey)
	if normalizedRelayEndpoint(idServer) != idServer || idServer == "" ||
		err != nil || base64.StdEncoding.EncodeToString(rendezvous) != rendezvousKey ||
		relayprotocol.PublicKey(base64.RawURLEncoding.EncodeToString(rendezvous)) == nil {
		return nil, errors.New("Windows delivery requires the deployed ID server and its Ed25519 public key")
	}
	center := windowsdelivery.Center{Origin: origin, IDServer: idServer, RendezvousKey: rendezvousKey}
	if signing != nil {
		center.SigningKeyID = signing.id
		center.SigningKey = base64.RawURLEncoding.EncodeToString(signing.key.Public().(ed25519.PublicKey))
	}
	if relayID != "" || relayAddress != "" {
		if !signingKeyID.MatchString(relayID) || normalizedRelayEndpoint(relayAddress) != relayAddress || relayAddress == "" {
			return nil, errWindowsCandidateInvalid
		}
		center.DefaultRelay = &windowsdelivery.Relay{ID: relayID, Address: relayAddress}
	}
	return windowsdelivery.Configure(directory, keyFile, center)
}

func validWindowsGroups(ctx context.Context, tx pgx.Tx, org string, ids []string) (bool, error) {
	var count int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM device_groups WHERE organization_id=$1 AND id=ANY($2::uuid[])`,
		org, ids).Scan(&count)
	return count == len(ids), err
}

func readWindowsTask(ctx context.Context, tx pgx.Tx, task string) (*windowsTaskDetail, error) {
	var detail windowsTaskDetail
	err := tx.QueryRow(ctx, `SELECT manifest_sha256, manifest, verified_at, COALESCE(verifier_key_sha256,'')
		FROM windows_delivery_requests WHERE task_id=$1`, task).
		Scan(&detail.ManifestSHA, &detail.Manifest, &detail.VerifiedAt, &detail.VerifierKey)
	return &detail, err
}

func (a *API) createWindowsDelivery(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	var in struct {
		RequestKey  string `json:"request_key"`
		ManifestSHA string `json:"manifest_sha256"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !auditSourceKey.MatchString(in.RequestKey) || !windowsdelivery.ValidDigest(in.ManifestSHA) {
		failure(w, "queue Windows verification", errTaskInvalid)
		return
	}
	ctx := r.Context()
	var task string
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		if a.store.WindowsDelivery == nil {
			return errWindowsDeliveryUnavailable
		}
		var prior string
		err := tx.QueryRow(ctx, `SELECT task_id,manifest_sha256 FROM windows_delivery_requests
			WHERE organization_id=$1 AND request_key=$2`, who.Organization, in.RequestKey).
			Scan(&task, &prior)
		if err == nil {
			if prior != in.ManifestSHA {
				return errTaskConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		manifest, err := a.store.WindowsDelivery.Read(in.ManifestSHA)
		switch {
		case errors.Is(err, windowsdelivery.ErrUnavailable):
			return errWindowsCandidateMissing
		case err != nil:
			return errWindowsCandidateInvalid
		}
		allowed, err := validWindowsGroups(ctx, tx, who.Organization, manifest.Approval.Groups)
		if err != nil {
			return err
		}
		if !allowed {
			return errForbidden
		}
		if err := tx.QueryRow(ctx, `INSERT INTO tasks(organization_id,kind,owner_id)
			VALUES($1,$2,$3) RETURNING id`, who.Organization, windowsDeliveryKind, who.ID).Scan(&task); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO windows_delivery_requests
			(task_id,organization_id,request_key,manifest_sha256,manifest) VALUES($1,$2,$3,$4,$5)`,
			task, who.Organization, in.RequestKey, in.ManifestSHA, manifest); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_targets(task_id,operation_id,object_key,state,requested_by)
			VALUES($1,gen_random_uuid(),'windows-bundle','queued',$2)`, task, who.ID); err != nil {
			return err
		}
		_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: who.Organization, ActorID: who.ID, TaskID: task,
			ResourceID: task, Action: "task.windows_delivery_created", Result: "accepted"})
		return err
	})
	if err != nil {
		failure(w, "queue Windows verification", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": task})
}

func (s Store) runWindowsDelivery(ctx context.Context, claim taskClaim) error {
	var id string
	if err := s.DB.QueryRow(ctx, `SELECT manifest_sha256 FROM windows_delivery_requests
		WHERE task_id=$1 AND organization_id=$2`, claim.Task, claim.Organization).Scan(&id); err != nil {
		return err
	}
	_, err := s.WindowsDelivery.Verify(ctx, id)
	var code string
	switch {
	case err == nil:
		code = "windows_verified"
	case errors.Is(err, windowsdelivery.ErrUnavailable):
		code = "windows_files_missing"
	case errors.Is(err, windowsdelivery.ErrManifest):
		code = "windows_manifest_invalid"
	case errors.Is(err, windowsdelivery.ErrSignature):
		code = "windows_signature_invalid"
	case errors.Is(err, windowsdelivery.ErrPackage):
		code = "windows_package_invalid"
	default:
		return err
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM organizations WHERE id=$1 FOR NO KEY UPDATE`, claim.Organization); err != nil {
			return err
		}
		detail, err := readWindowsTask(ctx, tx, claim.Task)
		if err != nil {
			return err
		}
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users u WHERE u.id=$1 AND u.organization_id=$2
			AND u.status='active' AND `+adminBinding+`)`, claim.Requester, claim.Organization).Scan(&allowed); err != nil {
			return err
		}
		groups, err := validWindowsGroups(ctx, tx, claim.Organization, detail.Manifest.Approval.Groups)
		if err != nil {
			return err
		}
		if !allowed || !groups {
			code = "authorization_changed"
		}
		if code == "windows_verified" {
			brand := detail.Manifest.Approval.Brand
			tag, err := tx.Exec(ctx, `INSERT INTO windows_delivery_brand_revisions(organization_id,template_id,revision,brand)
				VALUES($1,$2,$3,$4) ON CONFLICT (organization_id,template_id,revision)
				DO UPDATE SET brand=EXCLUDED.brand WHERE windows_delivery_brand_revisions.brand=EXCLUDED.brand`,
				claim.Organization, brand.ID, brand.Revision, brand)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				code = "windows_brand_conflict"
			}
		}
		state := "failed"
		if code == "windows_verified" {
			state = "succeeded"
			if _, err := tx.Exec(ctx, `UPDATE windows_delivery_requests SET verified_at=clock_timestamp(),verifier_key_sha256=$2
				WHERE task_id=$1`, claim.Task, s.WindowsDelivery.KeySHA()); err != nil {
				return err
			}
		}
		return finalizeTaskAttempt(ctx, tx, claim, state, code,
			state == "failed" && code != "authorization_changed" && code != "windows_brand_conflict" && claim.Sequence < maxTaskAttempts)
	})
}
