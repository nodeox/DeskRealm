package m1

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var errSessionMissing = errors.New("managed_session_not_found")

const scopedSessionFrom = sessionActivityFrom + ` JOIN devices d ON d.organization_id=g.organization_id AND d.id=g.target_device_id
	JOIN device_groups dg ON dg.id=d.group_id`
const scopedSessionWhere = ` WHERE g.organization_id=$1 AND (g.user_id=$2 OR ` + deviceGroupRead + `)`
const sessionCanRevoke = deviceGroupApprove
const sessionAdminSelect = `SELECT g.id,g.user_id,g.controller_device_id,g.target_device_id,g.purpose,g.confirmation_mode,
	g.policy_version,g.issued_at,g.expires_at,r.redeemed_at,COALESCE(r.path,''),COALESCE(r.relay_id,''),
	COALESCE(r.allowed_mask,g.allowed_mask),` + sessionCanRevoke + `,` + sessionActivitySelect + scopedSessionFrom + scopedSessionWhere

type sessionAdminRow struct {
	ID           string               `json:"id"`
	User         string               `json:"user_id"`
	Controller   string               `json:"controller_device_id"`
	Target       string               `json:"target_device_id"`
	Purpose      string               `json:"purpose"`
	Mode         string               `json:"confirmation_mode"`
	Policy       int64                `json:"policy_version"`
	Issued       time.Time            `json:"issued_at"`
	GrantExpires time.Time            `json:"grant_expires_at"`
	Redeemed     *time.Time           `json:"redeemed_at,omitempty"`
	Path         string               `json:"path,omitempty"`
	Relay        string               `json:"relay_id,omitempty"`
	Actions      []string             `json:"allowed_actions"`
	CanRevoke    bool                 `json:"can_revoke"`
	RelayHistory *sessionRelayHistory `json:"relay_history,omitempty"`
	sessionActivity
}

type sessionMetrics struct {
	Total   int64 `json:"total"`
	Active  int64 `json:"active"`
	Direct  int64 `json:"direct"`
	Relayed int64 `json:"relayed"`
}

const scopedSessionMetrics = `SELECT count(*),count(*) FILTER (WHERE r.session_id IS NOT NULL AND e.session_id IS NULL
	AND v.session_id IS NULL AND ` + currentSessionDeadline + `>statement_timestamp()),
	count(*) FILTER (WHERE r.path='direct'),count(*) FILTER (WHERE r.path='relay')` + scopedSessionFrom + scopedSessionWhere

func scanSessionAdmin(row pgx.Row) (sessionAdminRow, error) {
	var out sessionAdminRow
	var mask int16
	fields := []any{&out.ID, &out.User, &out.Controller, &out.Target, &out.Purpose, &out.Mode, &out.Policy,
		&out.Issued, &out.GrantExpires, &out.Redeemed, &out.Path, &out.Relay, &mask, &out.CanRevoke}
	err := row.Scan(append(fields, out.sessionActivity.destinations()...)...)
	out.Actions = controlActionsFromMask(byte(mask))
	return out, err
}

func (a *API) getSessions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, before := r.PathValue("id"), r.URL.Query().Get("before")
	if id != "" && !userIDPattern.MatchString(id) {
		failure(w, "read session", errSessionMissing)
		return
	}
	if before != "" && !userIDPattern.MatchString(before) {
		failure(w, "list sessions", errSessionInput)
		return
	}
	const limit = 100
	page := struct {
		Sessions []sessionAdminRow `json:"sessions"`
		Next     string            `json:"next,omitempty"`
		Metrics  sessionMetrics    `json:"metrics"`
	}{Sessions: []sessionAdminRow{}}
	var detail sessionAdminRow
	err := a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		if id != "" {
			var err error
			detail, err = scanSessionAdmin(tx.QueryRow(ctx, sessionAdminSelect+" AND g.id=$4", who.Organization, who.ID, who.PlatformAdmin, id))
			if errors.Is(err, pgx.ErrNoRows) {
				return errSessionMissing
			}
			if err != nil {
				return err
			}
			detail.RelayHistory, err = readSessionRelayHistory(ctx, tx, detail.ID)
			return err
		}
		m := &page.Metrics
		if err := tx.QueryRow(ctx, scopedSessionMetrics, who.Organization, who.ID, who.PlatformAdmin).
			Scan(&m.Total, &m.Active, &m.Direct, &m.Relayed); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, sessionAdminSelect+` AND ($4='' OR g.id<NULLIF($4,'')::uuid)
			ORDER BY g.id DESC LIMIT $5`, who.Organization, who.ID, who.PlatformAdmin, before, limit+1)
		if err != nil {
			return err
		}
		page.Sessions, err = pgx.AppendRows(page.Sessions, rows, func(row pgx.CollectableRow) (sessionAdminRow, error) {
			return scanSessionAdmin(row)
		})
		if len(page.Sessions) > limit {
			page.Sessions = page.Sessions[:limit]
			page.Next = page.Sessions[limit-1].ID
		}
		return err
	})
	if err != nil {
		failure(w, "read sessions", err)
		return
	}
	if id != "" {
		writeJSON(w, 200, detail)
		return
	}
	writeJSON(w, 200, page)
}

func (a *API) revokeSession(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		failure(w, "revoke session", errSessionMissing)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" || utf8.RuneCountInString(in.Reason) > 120 || strings.ContainsAny(in.Reason, "\r\n\x00") {
		failure(w, "revoke session", errSessionInput)
		return
	}
	ctx := r.Context()
	var result sessionActivity
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		view, err := scanSessionAdmin(tx.QueryRow(ctx, sessionAdminSelect+" AND g.id=$4", who.Organization, who.ID, who.PlatformAdmin, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return errSessionMissing
		}
		if err != nil {
			return err
		}
		if !view.CanRevoke {
			return errForbidden
		}
		result = view.sessionActivity
		if result.RevocationAt != nil {
			return nil
		}
		if result.State != "issued" && result.State != "active" {
			return errSessionConflict
		}
		g, err := readSessionGrant(tx.QueryRow(ctx, sessionGrantSelect+"WHERE id=$1", id))
		if err != nil {
			return err
		}
		if err := recordSessionRevocation(ctx, tx, g, who.ID, in.Reason); err != nil {
			return err
		}
		result, err = readSessionActivity(ctx, tx, id)
		return err
	})
	if err != nil {
		failure(w, "revoke session", err)
		return
	}
	writeJSON(w, 200, result)
}
