package m1

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

type device struct {
	ID                string `json:"id"`
	GroupID           string `json:"group_id"`
	GroupName         string `json:"group_name"`
	Platform          string `json:"platform"`
	RustDeskID        string `json:"rustdesk_id"`
	RegistrationState string `json:"registration_state"`
	ConnectionState   string `json:"connection_state"`
	ExpectedPolicy    *int64 `json:"expected_policy_version"`
	AppliedPolicy     *int64 `json:"applied_policy_version"`
	CanApprove        bool   `json:"can_approve"`
}

const deviceProjection = `d.id,d.group_id,dg.name,d.platform,d.rustdesk_id,d.registration_state,
	d.connection_state,d.expected_policy_version,d.applied_policy_version,` + deviceGroupApprove

func (a *API) listOrGetDevices(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	after := r.URL.Query().Get("after")
	if id != "" && !userIDPattern.MatchString(id) {
		failure(w, "read device", errDeviceMissing)
		return
	}
	if after != "" && !userIDPattern.MatchString(after) {
		writeError(w, 400, "invalid_cursor")
		return
	}
	ctx := r.Context()
	result := struct {
		Devices []device `json:"devices"`
		Next    string   `json:"next,omitempty"`
	}{Devices: make([]device, 0)}
	err := pgx.BeginTxFunc(ctx, a.store.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		who, err := actorSession(ctx, tx, a.cookieValue(r), false)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+deviceProjection+` FROM devices d JOIN device_groups dg ON dg.id=d.group_id
			WHERE d.organization_id=$1 AND `+deviceGroupRead+`
			AND ($4='' OR d.id=NULLIF($4,'')::uuid) AND ($5='' OR d.id>NULLIF($5,'')::uuid)
			ORDER BY d.id LIMIT 101`, who.Organization, who.ID, who.PlatformAdmin, id, after)
		if err != nil {
			return err
		}
		result.Devices, err = pgx.AppendRows(result.Devices, rows, pgx.RowToStructByPos[device])
		return err
	})
	if err != nil {
		failure(w, "read device", err)
		return
	}
	if id != "" {
		if len(result.Devices) == 0 {
			failure(w, "read device", errDeviceMissing)
			return
		}
		writeJSON(w, 200, result.Devices[0])
		return
	}
	if len(result.Devices) > 100 {
		result.Devices = result.Devices[:100]
		result.Next = result.Devices[99].ID
	}
	writeJSON(w, 200, result)
}

func (a *API) decideDevice(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "decide device", errDeviceMissing)
		return
	}
	var target string
	switch r.PathValue("action") {
	case "approve":
		target = "approved"
	case "reject":
		target = "rejected"
	default:
		writeError(w, 404, "not_found")
		return
	}
	var input struct{}
	if !readJSON(w, r, &input) {
		return
	}
	ctx := r.Context()
	var result device
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		who, err := actorSession(ctx, tx, a.cookieValue(r), true)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+deviceProjection+` FROM devices d JOIN device_groups dg ON dg.id=d.group_id
			WHERE d.organization_id=$1 AND `+deviceGroupApprove+` AND d.id=$4 FOR NO KEY UPDATE OF d`,
			who.Organization, who.ID, who.PlatformAdmin, id)
		if err != nil {
			return err
		}
		result, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[device])
		if errors.Is(err, pgx.ErrNoRows) {
			return errDeviceMissing
		}
		if err != nil || result.RegistrationState == target {
			return err
		}
		if result.RegistrationState != "pending" {
			return errDeviceDecision
		}
		tag, err := tx.Exec(ctx, `UPDATE registration_approvals SET decision=$2,decided_by=$3,decided_at=now()
			WHERE device_id=$1 AND decision IS NULL`, id, target, who.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errDeviceDecision
		}
		if _, err := tx.Exec(ctx, "UPDATE devices SET registration_state=$2 WHERE id=$1", id, target); err != nil {
			return err
		}
		result.RegistrationState = target
		return audit(ctx, tx, who.Organization, who.ID, "device."+target, id)
	})
	if err != nil {
		failure(w, "decide device", err)
		return
	}
	writeJSON(w, 200, result)
}

func isDeviceEnrollmentRequest(r *http.Request) bool {
	return r.URL.Path == "/v1/enrollment/challenge" || r.URL.Path == "/v1/enrollment/complete"
}
