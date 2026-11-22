package m1

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var errDeviceVersion = errors.New("device_version_conflict")
var errDeviceLifecycle = errors.New("device_lifecycle_conflict")

type deviceChange struct {
	Version    int64  `json:"version"`
	GroupID    string `json:"group_id,omitempty"`
	RustDeskID string `json:"rustdesk_id,omitempty"`
}

func (a *API) changeDevice(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "change device", errDeviceMissing)
		return
	}
	var in deviceChange
	if !readJSON(w, r, &in) {
		return
	}
	action := r.PathValue("change")
	valid := in.Version > 0
	switch action {
	case "move":
		valid = valid && userIDPattern.MatchString(in.GroupID) && in.RustDeskID == ""
	case "rename-id":
		valid = valid && in.GroupID == "" && rustdeskIDPattern.MatchString(in.RustDeskID)
	case "disable", "retire":
		valid = valid && in.GroupID == "" && in.RustDeskID == ""
	default:
		writeError(w, 404, "not_found")
		return
	}
	if !valid {
		writeError(w, 400, "invalid_device_change")
		return
	}

	ctx := r.Context()
	var result device
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		who, err := actorSession(ctx, tx, a.cookieValue(r), true)
		if err != nil {
			return err
		}
		result, err = lockManagedDevice(ctx, tx, who, id)
		if err != nil {
			return err
		}
		return applyDeviceChange(ctx, tx, who, &result, action, in)
	})
	if err != nil {
		failure(w, "change device", err)
		return
	}
	writeJSON(w, 200, result)
}

func applyDeviceChange(ctx context.Context, tx pgx.Tx, who actor, d *device, action string, in deviceChange) error {
	already := (action == "move" && d.GroupID == in.GroupID) || (action == "rename-id" && d.RustDeskID == in.RustDeskID) ||
		(action == "disable" && d.RegistrationState == "disabled") || (action == "retire" && d.RegistrationState == "retired")
	if already && (d.Version == in.Version || d.Version-1 == in.Version) {
		return nil
	}
	if d.Version != in.Version {
		return errDeviceVersion
	}
	if d.RegistrationState == "retired" {
		return errDeviceLifecycle
	}

	var reason, event string
	switch action {
	case "move":
		var name string
		err := tx.QueryRow(ctx, `SELECT dg.name FROM device_groups dg
			WHERE dg.organization_id=$1 AND `+deviceGroupApprove+` AND dg.id=$4`,
			who.Organization, who.ID, who.PlatformAdmin, in.GroupID).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return errDeviceGroupMissing
		}
		if err != nil {
			return err
		}
		reason = d.GroupID + " -> " + in.GroupID
		d.GroupID, d.GroupName = in.GroupID, name
		event = "device.moved"
	case "rename-id":
		reason = d.RustDeskID + " -> " + in.RustDeskID
		d.RustDeskID = in.RustDeskID
		event = "device.rustdesk_id_changed"
	case "disable":
		if d.RegistrationState != "approved" {
			return errDeviceLifecycle
		}
		d.RegistrationState = "disabled"
		event = "device.disabled"
	case "retire":
		d.RegistrationState = "retired"
		event = "device.retired"
	}

	err := tx.QueryRow(ctx, `UPDATE devices SET group_id=$2,rustdesk_id=$3,registration_state=$4,version=version+1
		WHERE id=$1 RETURNING version`, d.ID, d.GroupID, d.RustDeskID, d.RegistrationState).Scan(&d.Version)
	if uniqueViolation(err) {
		return errDeviceConflict
	}
	if err != nil {
		return err
	}
	if action == "rename-id" {
		if _, err := tx.Exec(ctx, "INSERT INTO rustdesk_id_history(device_id,rustdesk_id) VALUES($1,$2)", d.ID, d.RustDeskID); err != nil {
			return err
		}
	}
	if action == "disable" || action == "retire" {
		if _, err := tx.Exec(ctx, `UPDATE device_key_history SET revoked_at=now()
			WHERE device_id=$1 AND revoked_at IS NULL`, d.ID); err != nil {
			return err
		}
	}
	if err := revokePendingRotations(ctx, tx, d.ID); err != nil {
		return err
	}
	return auditWithReason(ctx, tx, who.Organization, who.ID, event, d.ID, reason)
}

func uniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

func revokePendingRotations(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE device_key_rotations SET revoked_at=now()
		WHERE device_id=$1 AND completed_key IS NULL AND revoked_at IS NULL`, id)
	return err
}

func revokeUserRotations(ctx context.Context, tx pgx.Tx, userID string) error {
	_, err := tx.Exec(ctx, `UPDATE device_key_rotations SET revoked_at=now()
		WHERE issued_by=$1 AND completed_key IS NULL AND revoked_at IS NULL`, userID)
	return err
}

type deviceHistory struct {
	IDs  []deviceIDHistory  `json:"rustdesk_ids"`
	Keys []deviceKeyHistory `json:"keys"`
}

type deviceIDHistory struct {
	ID         string    `json:"rustdesk_id"`
	RecordedAt time.Time `json:"recorded_at"`
}

type deviceKeyHistory struct {
	Fingerprint string     `json:"fingerprint"`
	RecordedAt  time.Time  `json:"recorded_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
}

func (a *API) deviceHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "read device history", errDeviceMissing)
		return
	}
	ctx := r.Context()
	out := deviceHistory{IDs: make([]deviceIDHistory, 0), Keys: make([]deviceKeyHistory, 0)}
	err := pgx.BeginTxFunc(ctx, a.store.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		who, err := actorSession(ctx, tx, a.cookieValue(r), false)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT d.id FROM devices d JOIN device_groups dg ON dg.id=d.group_id
			WHERE d.organization_id=$1 AND `+deviceGroupRead+` AND d.id=$4`, who.Organization, who.ID, who.PlatformAdmin, id).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errDeviceMissing
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT rustdesk_id,recorded_at FROM rustdesk_id_history
			WHERE device_id=$1 ORDER BY id DESC LIMIT 100`, id)
		if err != nil {
			return err
		}
		out.IDs, err = pgx.AppendRows(out.IDs, rows, pgx.RowToStructByPos[deviceIDHistory])
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT encode(sha256(public_key),'hex'),recorded_at,revoked_at
			FROM device_key_history WHERE device_id=$1 ORDER BY recorded_at DESC,public_key LIMIT 100`, id)
		if err != nil {
			return err
		}
		out.Keys, err = pgx.AppendRows(out.Keys, rows, pgx.RowToStructByPos[deviceKeyHistory])
		return err
	})
	if err != nil {
		failure(w, "read device history", err)
		return
	}
	writeJSON(w, 200, out)
}
