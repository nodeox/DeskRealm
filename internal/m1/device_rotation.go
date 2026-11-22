package m1

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var errDeviceRotation = errors.New("invalid_device_rotation")

type rotationGrant struct {
	DeviceID   string    `json:"device_id"`
	Version    int64     `json:"version"`
	Credential string    `json:"credential"`
	Nonce      string    `json:"nonce"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (a *API) issueDeviceRotation(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "issue device rotation", errDeviceMissing)
		return
	}
	var in struct {
		Version int64 `json:"version"`
		Restore bool  `json:"restore"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Version < 1 {
		writeError(w, 400, "invalid_device_change")
		return
	}

	credential, err := token()
	if err != nil {
		failure(w, "issue device rotation", err)
		return
	}
	nonce, err := token()
	if err != nil {
		failure(w, "issue device rotation", err)
		return
	}
	out := rotationGrant{DeviceID: id, Version: in.Version, Credential: credential, Nonce: nonce}
	ctx := r.Context()
	err = pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		who, err := actorSession(ctx, tx, a.cookieValue(r), true)
		if err != nil {
			return err
		}
		d, err := lockManagedDevice(ctx, tx, who, id)
		if err != nil {
			return err
		}
		if d.Version != in.Version {
			return errDeviceVersion
		}
		if (!in.Restore && d.RegistrationState != "approved") || (in.Restore && d.RegistrationState != "disabled") {
			return errDeviceLifecycle
		}
		if err := revokePendingRotations(ctx, tx, id); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO device_key_rotations(device_id,issued_by,device_version,digest,nonce,restore,expires_at)
			VALUES($1,$2,$3,$4,$5,$6,clock_timestamp()+interval '15 minutes') RETURNING expires_at`,
			id, who.ID, d.Version, digest(credential), nonce, in.Restore).Scan(&out.ExpiresAt); err != nil {
			return err
		}
		return audit(ctx, tx, who.Organization, who.ID, "device.rotation_issued", id)
	})
	if err != nil {
		failure(w, "issue device rotation", err)
		return
	}
	writeJSON(w, 201, out)
}

type rotationRequest struct {
	DeviceID   string `json:"device_id"`
	Version    int64  `json:"version"`
	Credential string `json:"credential"`
	PublicKey  string `json:"public_key"`
	Signature  string `json:"signature"`
}

type rotationReceipt struct {
	DeviceID string `json:"device_id"`
	Version  int64  `json:"version"`
	State    string `json:"registration_state"`
}

func rotationProof(in rotationRequest, nonce string) []byte {
	return []byte(strings.Join([]string{"deskrealm/device-key-rotation/v1", in.DeviceID,
		strconv.FormatInt(in.Version, 10), nonce, in.PublicKey}, "\n"))
}

func (s Store) completeDeviceRotation(ctx context.Context, in rotationRequest, key, sig []byte) (rotationReceipt, error) {
	var out rotationReceipt
	hash := digest(in.Credential)
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT singleton FROM organizations WHERE singleton FOR SHARE"); err != nil {
			return err
		}
		var org, group, state string
		var version int64
		var oldKey []byte
		err := tx.QueryRow(ctx, `SELECT d.organization_id,d.group_id,d.registration_state,d.version,d.public_key
			FROM devices d JOIN device_key_rotations k ON k.device_id=d.id WHERE k.digest=$1 AND d.id=$2
			FOR NO KEY UPDATE OF d`, hash, in.DeviceID).Scan(&org, &group, &state, &version, &oldKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return errDeviceRotation
		}
		if err != nil {
			return err
		}

		var nonce, issuer string
		var issuedVersion int64
		var restore, live bool
		var completedKey []byte
		var completedVersion *int64
		err = tx.QueryRow(ctx, `SELECT nonce,issued_by,device_version,restore,
			revoked_at IS NULL AND expires_at>clock_timestamp(),completed_key,completed_version
			FROM device_key_rotations WHERE digest=$1 AND device_id=$2 FOR UPDATE`, hash, in.DeviceID).
			Scan(&nonce, &issuer, &issuedVersion, &restore, &live, &completedKey, &completedVersion)
		if err != nil {
			return err
		}
		if in.Version != issuedVersion || !ed25519.Verify(key, rotationProof(in, nonce), sig) {
			return errDeviceRotation
		}
		if completedVersion != nil {
			if state != "approved" || !bytes.Equal(key, completedKey) || !bytes.Equal(oldKey, key) {
				return errDeviceRotation
			}
			out = rotationReceipt{in.DeviceID, *completedVersion, "approved"}
			return nil
		}
		if !live || version != issuedVersion || bytes.Equal(oldKey, key) ||
			(!restore && state != "approved") || (restore && state != "disabled") {
			return errDeviceRotation
		}
		var issuerAllowed bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM users u WHERE u.id=$1 AND u.organization_id=$2 AND u.status='active' AND (`+adminBinding+` OR EXISTS(
				SELECT 1 FROM device_group_grants g WHERE g.group_id=$3 AND g.user_id=u.id AND g.can_approve
			))
		)`, issuer, org, group).Scan(&issuerAllowed)
		if err != nil {
			return err
		}
		if !issuerAllowed {
			return errDeviceRotation
		}

		if _, err := tx.Exec(ctx, "INSERT INTO device_key_history(device_id,public_key) VALUES($1,$2)", in.DeviceID, key); err != nil {
			if uniqueViolation(err) {
				return errDeviceConflict
			}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE device_key_history SET revoked_at=now() WHERE device_id=$1 AND public_key=$2 AND revoked_at IS NULL`,
			in.DeviceID, oldKey); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `UPDATE devices SET public_key=$2,registration_state='approved',version=version+1
			WHERE id=$1 RETURNING version`, in.DeviceID, key).Scan(&version)
		if uniqueViolation(err) {
			return errDeviceConflict
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE device_key_rotations SET completed_key=$2,completed_version=$3 WHERE digest=$1`,
			hash, key, version); err != nil {
			return err
		}

		event := "device.key_rotated"
		if restore {
			event = "device.restored"
		}
		out = rotationReceipt{in.DeviceID, version, "approved"}
		return auditWithReason(ctx, tx, org, "", event, in.DeviceID, issuer)
	})
	return out, err
}

func (a *API) rotateDevice(w http.ResponseWriter, r *http.Request) {
	var in rotationRequest
	if !readJSON(w, r, &in) {
		return
	}
	key, valid := devicePublicKey(in.PublicKey)
	sig, sigErr := base64.StdEncoding.Strict().DecodeString(in.Signature)
	if !userIDPattern.MatchString(in.DeviceID) || in.Version < 1 || !validToken(in.Credential) ||
		!valid ||
		sigErr != nil || len(sig) != ed25519.SignatureSize || base64.StdEncoding.EncodeToString(sig) != in.Signature {
		failure(w, "rotate device", errDeviceRotation)
		return
	}
	if !a.admit(r) {
		writeError(w, 429, "try_again_later")
		return
	}
	defer func() { <-a.hashSlots }()
	out, err := a.store.completeDeviceRotation(r.Context(), in, key, sig)
	if err != nil {
		failure(w, "rotate device", err)
		return
	}
	writeJSON(w, 200, out)
}
