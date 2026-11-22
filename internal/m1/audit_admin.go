package m1

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

const defaultAuditRetention = 90

type auditGrant struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Scope     string `json:"scope"`
	Group     string `json:"group,omitempty"`
	CanExport bool   `json:"can_export"`
}

func (a *API) listAuditGrants(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := readAsAdmin(ctx, a.store, a.cookieValue(r), func(tx pgx.Tx, org string) ([]auditGrant, error) {
		rows, err := tx.Query(ctx, `SELECT g.id,u.username,g.scope,COALESCE(ug.name,dg.name,''),g.can_export
			FROM audit_grants g JOIN users u ON u.id=g.user_id
			LEFT JOIN user_groups ug ON ug.id=g.user_group_id LEFT JOIN device_groups dg ON dg.id=g.device_group_id
			WHERE g.organization_id=$1 ORDER BY u.username,g.scope,g.id`, org)
		if err != nil {
			return nil, err
		}
		return pgx.AppendRows(make([]auditGrant, 0), rows, pgx.RowToStructByPos[auditGrant])
	})
	if err != nil {
		failure(w, "list audit grants", err)
		return
	}
	writeJSON(w, 200, list)
}

func (a *API) saveAuditGrant(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	var in auditGrant
	if !readJSON(w, r, &in) {
		return
	}
	in.Username = normalizeUsername(in.Username)
	in.Group = strings.TrimSpace(in.Group)
	groupScope := in.Scope == "user_group" || in.Scope == "device_group"
	if in.ID != "" || !usernamePattern.MatchString(in.Username) ||
		(groupScope && !validOrganization(in.Group)) ||
		(!groupScope && (in.Group != "" || (in.Scope != "organization" && in.Scope != "self"))) {
		failure(w, "save audit grant", errAuditInvalid)
		return
	}
	ctx := r.Context()
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		id, err := userByUsername(ctx, tx, who.Organization, in.Username)
		if err != nil {
			return err
		}
		var userGroup, deviceGroup string
		switch in.Scope {
		case "user_group":
			err = tx.QueryRow(ctx, "SELECT id FROM user_groups WHERE organization_id=$1 AND name=$2", who.Organization, in.Group).Scan(&userGroup)
		case "device_group":
			err = tx.QueryRow(ctx, "SELECT id FROM device_groups WHERE organization_id=$1 AND name=$2", who.Organization, in.Group).Scan(&deviceGroup)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return errGroupMissing
		}
		if err != nil {
			return err
		}
		var grant string
		err = tx.QueryRow(ctx, `INSERT INTO audit_grants AS g(organization_id,user_id,scope,user_group_id,device_group_id,can_export)
			VALUES($1,$2,$3,NULLIF($4,'')::uuid,NULLIF($5,'')::uuid,$6)
			ON CONFLICT(user_id,scope,user_group_id,device_group_id) DO UPDATE SET can_export=EXCLUDED.can_export
			WHERE g.can_export<>EXCLUDED.can_export RETURNING id`, who.Organization, id, in.Scope, userGroup, deviceGroup, in.CanExport).Scan(&grant)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return auditWithReason(ctx, tx, who.Organization, who.ID, "audit.granted", grant, id)
	})
	if err != nil {
		failure(w, "save audit grant", err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) revokeAuditGrant(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "revoke audit grant", errAuditInvalid)
		return
	}
	ctx := r.Context()
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		tag, err := tx.Exec(ctx, "DELETE FROM audit_grants WHERE organization_id=$1 AND id=$2", who.Organization, id)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return audit(ctx, tx, who.Organization, who.ID, "audit.grant_revoked", id)
	})
	if err != nil {
		failure(w, "revoke audit grant", err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) auditOptions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	result := struct {
		CanRead       bool `json:"can_read"`
		CanExport     bool `json:"can_export"`
		CanAdminister bool `json:"can_administer"`
		RetentionDays int  `json:"retention_days,omitempty"`
	}{}
	err := a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		var err error
		result.CanRead, result.CanExport, err = auditCapabilities(ctx, tx, who)
		if err != nil {
			return err
		}
		result.CanAdminister = who.PlatformAdmin
		if !who.PlatformAdmin {
			return nil
		}
		return tx.QueryRow(ctx, `SELECT COALESCE((SELECT days FROM audit_retention WHERE organization_id=$1),$2)`,
			who.Organization, defaultAuditRetention).Scan(&result.RetentionDays)
	})
	if err != nil {
		failure(w, "read audit options", err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *API) saveAuditRetention(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	var in struct {
		Days int `json:"days"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Days < 1 || in.Days > 3650 {
		failure(w, "save audit retention", errAuditInvalid)
		return
	}
	ctx := r.Context()
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		tag, err := tx.Exec(ctx, `INSERT INTO audit_retention AS p(organization_id,days) VALUES($1,$2)
			ON CONFLICT(organization_id) DO UPDATE SET days=EXCLUDED.days WHERE p.days<>EXCLUDED.days`, who.Organization, in.Days)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return auditWithReason(ctx, tx, who.Organization, who.ID, "audit.retention_changed", "", strconv.Itoa(in.Days))
	})
	if err != nil {
		failure(w, "save audit retention", err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) pruneAudit(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUserWrite(w, r) {
		return
	}
	ctx := r.Context()
	var removed int64
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		tag, err := tx.Exec(ctx, `WITH boundary AS (
			SELECT statement_timestamp()-make_interval(days=>COALESCE(
				(SELECT days FROM audit_retention WHERE organization_id=$1),$2)) AS cutoff
		), batch AS (
			SELECT e.id FROM audit_events e LEFT JOIN outbox o ON o.event_id=e.id CROSS JOIN boundary b
			WHERE e.organization_id=$1 AND e.occurred_at<b.cutoff AND e.received_at<b.cutoff
			AND (e.acknowledged_at IS NULL OR e.acknowledged_at<b.cutoff)
			AND e.receipt_state='confirmed' AND (o.event_id IS NULL OR o.delivered_at IS NOT NULL)
			ORDER BY e.occurred_at,e.id LIMIT 250 FOR UPDATE OF e
		), delivered AS (
			DELETE FROM outbox o USING batch WHERE o.event_id=batch.id RETURNING o.event_id
		)
		DELETE FROM audit_events e USING batch WHERE e.id=batch.id`, who.Organization, defaultAuditRetention)
		if err != nil {
			return err
		}
		removed = tag.RowsAffected()
		return auditWithReason(ctx, tx, who.Organization, who.ID, "audit.retention_pruned", "", strconv.FormatInt(removed, 10))
	})
	if err != nil {
		failure(w, "prune audit", err)
		return
	}
	writeJSON(w, 200, map[string]int64{"removed": removed})
}
