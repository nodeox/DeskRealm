package m1

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	errDeviceGroupMissing  = errors.New("device_group_not_found")
	errDeviceGroupConflict = errors.New("device_group_name_in_use")
	errDeviceMissing       = errors.New("device_not_found")
	errDeviceConflict      = errors.New("device_conflict")
	errDeviceDecision      = errors.New("device_decision_conflict")
)

type deviceGroup struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CanApprove bool   `json:"can_approve"`
}

const deviceGroupRead = `( $3 OR EXISTS (
	SELECT 1 FROM device_group_grants g WHERE g.group_id=dg.id AND g.user_id=$2
))`

const deviceGroupApprove = `( $3 OR EXISTS (
	SELECT 1 FROM device_group_grants g WHERE g.group_id=dg.id AND g.user_id=$2 AND g.can_approve
))`

func canManageDevices(ctx context.Context, store Store, user principal) (bool, error) {
	if user.Role == adminRole {
		return true, nil
	}
	var available bool
	err := store.DB.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM device_group_grants WHERE user_id=$1)", user.UserID).Scan(&available)
	return available, err
}

func (a *API) listDeviceGroups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	result := make([]deviceGroup, 0)
	err := pgx.BeginTxFunc(ctx, a.store.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		who, err := actorSession(ctx, tx, a.cookieValue(r), false)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT dg.id,dg.name,`+deviceGroupApprove+` FROM device_groups dg
			WHERE dg.organization_id=$1 AND `+deviceGroupRead+` ORDER BY dg.name,dg.id`, who.Organization, who.ID, who.PlatformAdmin)
		if err != nil {
			return err
		}
		result, err = pgx.AppendRows(result, rows, pgx.RowToStructByPos[deviceGroup])
		return err
	})
	if err != nil {
		failure(w, "list device groups", err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *API) createDeviceGroup(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !validOrganization(in.Name) {
		writeError(w, 400, "invalid_group")
		return
	}
	ctx := r.Context()
	result := deviceGroup{Name: in.Name, CanApprove: true}
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		who, org, err := managementActor(ctx, tx, a.cookieValue(r), true)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, "INSERT INTO device_groups(organization_id,name) VALUES ($1,$2) RETURNING id", org, in.Name).Scan(&result.ID)
		var dbErr *pgconn.PgError
		if errors.As(err, &dbErr) && dbErr.Code == "23505" {
			return errDeviceGroupConflict
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, org, who, "device_group.created", result.ID)
	})
	if err != nil {
		failure(w, "create device group", err)
		return
	}
	writeJSON(w, 201, result)
}

type deviceGroupGrant struct {
	Username string `json:"username"`
	Approve  bool   `json:"can_approve"`
	Revoke   bool   `json:"revoke,omitempty"`
}

func (a *API) deviceGroupGrants(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "list device group grants", errDeviceGroupMissing)
		return
	}
	ctx := r.Context()
	result, err := readAsAdmin(ctx, a.store, a.cookieValue(r), func(tx pgx.Tx, org string) ([]deviceGroupGrant, error) {
		if err := requireDeviceGroup(ctx, tx, org, id); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, `SELECT u.username,g.can_approve FROM device_group_grants g
			JOIN users u ON u.id=g.user_id WHERE g.organization_id=$1 AND g.group_id=$2 ORDER BY u.username`, org, id)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, func(row pgx.CollectableRow) (deviceGroupGrant, error) {
			var grant deviceGroupGrant
			err := row.Scan(&grant.Username, &grant.Approve)
			return grant, err
		})
	})
	if err != nil {
		failure(w, "list device group grants", err)
		return
	}
	if result == nil {
		result = []deviceGroupGrant{}
	}
	writeJSON(w, 200, result)
}

func requireDeviceGroup(ctx context.Context, tx pgx.Tx, org, id string) error {
	var exists bool
	err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM device_groups WHERE organization_id=$1 AND id=$2)", org, id).Scan(&exists)
	if err == nil && !exists {
		return errDeviceGroupMissing
	}
	return err
}

func (a *API) setDeviceGroupGrant(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "change device group grant", errDeviceGroupMissing)
		return
	}
	var in deviceGroupGrant
	if !readJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		who, org, err := managementActor(ctx, tx, a.cookieValue(r), true)
		if err != nil {
			return err
		}
		if err := requireDeviceGroup(ctx, tx, org, id); err != nil {
			return err
		}
		target, err := userByUsername(ctx, tx, org, normalizeUsername(in.Username))
		if err != nil {
			return err
		}
		action := "device_group.granted"
		var tag pgconn.CommandTag
		if in.Revoke {
			action = "device_group.revoked"
			tag, err = tx.Exec(ctx, `DELETE FROM device_group_grants WHERE organization_id=$1 AND group_id=$2 AND user_id=$3`, org, id, target)
		} else {
			tag, err = tx.Exec(ctx, `INSERT INTO device_group_grants(organization_id,group_id,user_id,can_approve)
				VALUES($1,$2,$3,$4) ON CONFLICT(group_id,user_id) DO UPDATE SET can_approve=EXCLUDED.can_approve
				WHERE device_group_grants.can_approve<>EXCLUDED.can_approve`, org, id, target, in.Approve)
		}
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return auditWithReason(ctx, tx, org, who, action, id, target)
	})
	if err != nil {
		failure(w, "change device group grant", err)
		return
	}
	w.WriteHeader(204)
}
