package m1

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	errEnrollment         = errors.New("invalid_enrollment")
	errEnrollmentConflict = errors.New("enrollment_conflict")
	rustdeskIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{2,31}$`)
)

func validDevicePlatform(s string) bool {
	return s == "windows-amd64" || s == "windows-arm64"
}

type enrollmentRequest struct {
	Credential string `json:"credential"`
	GroupID    string `json:"group_id"`
	Platform   string `json:"platform"`
	RustDeskID string `json:"rustdesk_id"`
	PublicKey  string `json:"public_key"`
	AttemptID  string `json:"attempt_id,omitempty"`
	Signature  string `json:"signature,omitempty"`
}

type enrollmentChallenge struct {
	ID        string    `json:"attempt_id"`
	Nonce     string    `json:"nonce"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (in enrollmentRequest) key() ([]byte, error) {
	key, valid := devicePublicKey(in.PublicKey)
	if !valid ||
		!validToken(in.Credential) || !userIDPattern.MatchString(in.GroupID) ||
		!validDevicePlatform(in.Platform) || !rustdeskIDPattern.MatchString(in.RustDeskID) {
		return nil, errEnrollment
	}
	return key, nil
}

// All fields have restricted alphabets. Newlines cannot occur in a field.
func enrollmentProof(id, nonce string, in enrollmentRequest) []byte {
	return []byte(strings.Join([]string{"deskrealm/device-enrollment/v1", id, nonce, in.GroupID,
		in.Platform, in.RustDeskID, in.PublicKey}, "\n"))
}

func (a *API) issueEnrollment(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	var in struct {
		GroupID  string `json:"group_id"`
		Purpose  string `json:"purpose"`
		Platform string `json:"platform"`
		Minutes  int    `json:"lifetime_minutes"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Purpose = strings.TrimSpace(in.Purpose)
	if !userIDPattern.MatchString(in.GroupID) || !validOrganization(in.Purpose) || !validDevicePlatform(in.Platform) ||
		in.Minutes < 1 || in.Minutes > 1440 {
		writeError(w, 400, "invalid_enrollment_request")
		return
	}
	value, err := token()
	if err != nil {
		failure(w, "issue enrollment", err)
		return
	}
	var id string
	var expires time.Time
	ctx := r.Context()
	err = pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		who, err := actorSession(ctx, tx, a.cookieValue(r), true)
		if err != nil {
			return err
		}
		var groupID string
		err = tx.QueryRow(ctx, `SELECT dg.id FROM device_groups dg WHERE dg.organization_id=$1 AND `+deviceGroupApprove+` AND dg.id=$4`,
			who.Organization, who.ID, who.PlatformAdmin, in.GroupID).Scan(&groupID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errDeviceGroupMissing
		}
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO enrollment_credentials(organization_id,group_id,platform,purpose,digest,issued_by,expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,clock_timestamp()+make_interval(mins=>$7))
			RETURNING id,expires_at`, who.Organization, groupID, in.Platform, in.Purpose, digest(value), who.ID, in.Minutes).Scan(&id, &expires)
		if err != nil {
			return err
		}
		_, err = addAuditEvent(ctx, tx, auditEvent{
			OrganizationID: who.Organization, ActorID: who.ID, ResourceID: id,
			DeviceGroupID: groupID, Action: "device.enrollment_issued", Result: "success",
		})
		return err
	})
	if err != nil {
		failure(w, "issue enrollment", err)
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "credential": value, "expires_at": expires})
}

type lockedEnrollment struct {
	ID, Organization, GroupID, Platform string
	Issuer                              string
	Expires                             time.Time
	Live, Consumed                      bool
}

func lockEnrollment(ctx context.Context, tx pgx.Tx, credential string) (lockedEnrollment, error) {
	var e lockedEnrollment
	err := tx.QueryRow(ctx, `SELECT id,organization_id,group_id,platform,expires_at,
		expires_at>clock_timestamp(),consumed_at IS NOT NULL,issued_by
		FROM enrollment_credentials WHERE digest=$1 FOR UPDATE`, digest(credential)).
		Scan(&e.ID, &e.Organization, &e.GroupID, &e.Platform, &e.Expires, &e.Live, &e.Consumed, &e.Issuer)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errEnrollment
	}
	return e, err
}

func enrollmentIssuerAllowed(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM enrollment_credentials e JOIN users u ON u.id=e.issued_by
		WHERE e.id=$1 AND u.status='active' AND (`+adminBinding+` OR EXISTS (
			SELECT 1 FROM device_group_grants g WHERE g.group_id=e.group_id AND g.user_id=u.id AND g.can_approve
		))
	)`, id).Scan(&allowed)
	return allowed, err
}

func (s Store) challengeEnrollment(ctx context.Context, in enrollmentRequest, key []byte) (enrollmentChallenge, error) {
	var out enrollmentChallenge
	nonce, err := token()
	if err != nil {
		return out, err
	}
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT singleton FROM organizations WHERE singleton FOR SHARE"); err != nil {
			return err
		}
		e, err := lockEnrollment(ctx, tx, in.Credential)
		if err != nil {
			return err
		}
		if !e.Live || e.Consumed || e.GroupID != in.GroupID || e.Platform != in.Platform {
			return errEnrollment
		}
		allowed, err := enrollmentIssuerAllowed(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		if !allowed {
			return errEnrollment
		}
		var knownKey bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM device_key_history WHERE public_key=$1)", key).Scan(&knownKey); err != nil {
			return err
		}
		if knownKey {
			return errDeviceConflict
		}
		// An expired uncompleted attempt may be replaced without consuming the credential.
		if _, err := tx.Exec(ctx, `DELETE FROM enrollment_attempts WHERE credential_id=$1
			AND expires_at<=clock_timestamp() AND device_id IS NULL`, e.ID); err != nil {
			return err
		}
		var registered []byte
		var platform, rustdeskID string
		err = tx.QueryRow(ctx, `SELECT id,nonce,expires_at,public_key,platform,rustdesk_id FROM enrollment_attempts
			WHERE credential_id=$1`, e.ID).Scan(&out.ID, &out.Nonce, &out.ExpiresAt, &registered, &platform, &rustdeskID)
		if err == nil {
			if !bytes.Equal(registered, key) || platform != in.Platform || rustdeskID != in.RustDeskID {
				return errEnrollmentConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO enrollment_attempts(credential_id,nonce,group_id,platform,rustdesk_id,public_key,expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,LEAST($7,clock_timestamp()+interval '2 minutes')) RETURNING id,nonce,expires_at`,
			e.ID, nonce, e.GroupID, in.Platform, in.RustDeskID, key, e.Expires).Scan(&out.ID, &out.Nonce, &out.ExpiresAt)
	})
	return out, err
}

type enrollmentResult struct {
	DeviceID string `json:"device_id"`
	State    string `json:"registration_state"`
}

func (s Store) completeEnrollment(ctx context.Context, in enrollmentRequest, key, signature []byte) (enrollmentResult, error) {
	var out enrollmentResult
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT singleton FROM organizations WHERE singleton FOR SHARE"); err != nil {
			return err
		}
		e, err := lockEnrollment(ctx, tx, in.Credential)
		if err != nil {
			return err
		}
		if e.GroupID != in.GroupID || e.Platform != in.Platform {
			return errEnrollment
		}
		var storedKey []byte
		var nonce, group, platform, rustdeskID string
		var live bool
		err = tx.QueryRow(ctx, `SELECT nonce,group_id,platform,rustdesk_id,public_key,expires_at>clock_timestamp(),
			COALESCE(device_id::text,'') FROM enrollment_attempts WHERE id=$1 AND credential_id=$2`, in.AttemptID, e.ID).
			Scan(&nonce, &group, &platform, &rustdeskID, &storedKey, &live, &out.DeviceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errEnrollment
		}
		if err != nil {
			return err
		}
		if group != in.GroupID || platform != in.Platform || rustdeskID != in.RustDeskID || !bytes.Equal(key, storedKey) {
			return errEnrollmentConflict
		}
		if !ed25519.Verify(key, enrollmentProof(in.AttemptID, nonce, in), signature) {
			return errEnrollment
		}
		if out.DeviceID != "" && e.Consumed {
			var active bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM devices WHERE id=$1 AND public_key=$2
				AND registration_state IN ('pending','approved'))`, out.DeviceID, key).Scan(&active); err != nil {
				return err
			}
			if !active {
				return errEnrollment
			}
			// A completed request always returns its original receipt. The status API tracks subsequent decisions.
			out.State = "pending"
			return nil
		}
		if !live || !e.Live || e.Consumed {
			return errEnrollment
		}
		allowed, err := enrollmentIssuerAllowed(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		if !allowed {
			return errEnrollment
		}
		err = tx.QueryRow(ctx, `INSERT INTO devices(organization_id,group_id,platform,rustdesk_id,public_key)
			VALUES($1,$2,$3,$4,$5) RETURNING id,registration_state`, e.Organization, e.GroupID, platform, rustdeskID, key).
			Scan(&out.DeviceID, &out.State)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return errDeviceConflict
		}
		if err != nil {
			return err
		}
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{"INSERT INTO device_key_history(device_id,public_key) VALUES($1,$2)", []any{out.DeviceID, key}},
			{"INSERT INTO rustdesk_id_history(device_id,rustdesk_id) VALUES($1,$2)", []any{out.DeviceID, rustdeskID}},
			{"INSERT INTO registration_approvals(device_id) VALUES($1)", []any{out.DeviceID}},
			{"UPDATE enrollment_credentials SET consumed_at=now() WHERE id=$1", []any{e.ID}},
			{"UPDATE enrollment_attempts SET device_id=$2 WHERE id=$1", []any{in.AttemptID, out.DeviceID}},
		} {
			if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
				if uniqueViolation(err) {
					return errDeviceConflict
				}
				return err
			}
		}
		taskID, err := createEnrollmentTask(ctx, tx, e.Organization, e.Issuer, out.DeviceID, rustdeskID)
		if err != nil {
			return err
		}
		if err := refreshManagedPolicyTargets(ctx, tx, e.Organization, out.DeviceID, false); err != nil {
			return err
		}
		_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: e.Organization, ResourceID: out.DeviceID,
			TaskID: taskID, DeviceGroupID: e.GroupID, Action: "device.enrollment_pending", Result: "success"})
		return err
	})
	return out, err
}

func (a *API) enrollDevice(w http.ResponseWriter, r *http.Request) {
	var in enrollmentRequest
	if !readJSON(w, r, &in) {
		return
	}
	key, err := in.key()
	if err != nil {
		failure(w, "enroll device", err)
		return
	}
	if !a.admit(r) {
		writeError(w, 429, "try_again_later")
		return
	}
	defer func() { <-a.hashSlots }()
	if r.URL.Path == "/v1/enrollment/challenge" {
		if in.AttemptID != "" || in.Signature != "" {
			failure(w, "enrollment challenge", errEnrollment)
			return
		}
		out, err := a.store.challengeEnrollment(r.Context(), in, key)
		if err != nil {
			failure(w, "enrollment challenge", err)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(in.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize || base64.StdEncoding.EncodeToString(sig) != in.Signature ||
		!userIDPattern.MatchString(in.AttemptID) {
		failure(w, "complete enrollment", errEnrollment)
		return
	}
	out, err := a.store.completeEnrollment(r.Context(), in, key, sig)
	if err != nil {
		failure(w, "complete enrollment", err)
		return
	}
	writeJSON(w, 200, out)
}
