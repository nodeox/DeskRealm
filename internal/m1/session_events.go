package m1

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
)

func (a *API) sessionRuntimeStatus(w http.ResponseWriter, r *http.Request) {
	var activity sessionActivity
	ctx := r.Context()
	err := pgx.BeginTxFunc(ctx, a.store.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		g, _, err := authenticateSessionReceipt(ctx, tx, r.PathValue("id"), r.URL.Query().Get("connection_id"), managedBearer(r, sessionReceiptPrefix))
		if err != nil {
			return err
		}
		activity, err = readSessionActivity(ctx, tx, g.ID)
		return err
	})
	if err != nil {
		failure(w, "read session status", err)
		return
	}
	activity.RevocationReason = ""
	writeJSON(w, 200, activity)
}

func (a *API) sessionStopped(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Connection string `json:"connection_id"`
		Sequence   int64  `json:"lease_sequence"`
		Reason     string `json:"reason"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	switch in.Reason {
	case "local_end", "revoked", "lease_expired", "connection_lost":
	default:
		failure(w, "report session stop", errSessionInput)
		return
	}
	if in.Sequence < 1 {
		failure(w, "report session stop", errSessionInput)
		return
	}
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		if err := lockSessionAuthorization(ctx, tx); err != nil {
			return err
		}
		g, lease, err := authenticateSessionReceipt(ctx, tx, r.PathValue("id"), in.Connection, managedBearer(r, sessionReceiptPrefix))
		if err != nil {
			return err
		}
		if in.Sequence > lease.Sequence {
			return errSessionConflict
		}
		var sequence int64
		var reason string
		err = tx.QueryRow(ctx, `SELECT lease_sequence,reason FROM managed_session_stops WHERE session_id=$1`, g.ID).Scan(&sequence, &reason)
		switch {
		case err == nil && sequence == in.Sequence && reason == in.Reason:
			return nil
		case err == nil:
			return errSessionConflict
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if in.Reason == "revoked" {
			reason, err := sessionTerminalReason(ctx, tx, g.ID, "")
			if err != nil {
				return err
			}
			if reason == "" {
				return errSessionConflict
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO managed_session_stops(session_id,lease_sequence,reason) VALUES($1,$2,$3)`,
			g.ID, in.Sequence, in.Reason); err != nil {
			return err
		}
		if err := auditSessionEndpoint(ctx, tx, g, "session.stopped", "confirmed", in.Reason); err != nil {
			return err
		}
		var pending string
		err = tx.QueryRow(ctx, `SELECT id FROM audit_events WHERE organization_id=$1 AND source='control_plane'
			AND source_key=$2`, g.Organization, "session-revocation:"+g.ID).Scan(&pending)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return receiveAuditReceipt(ctx, tx, g.Organization, pending, "managed_endpoint", g.ID, "success")
	})
	if err != nil {
		failure(w, "report session stop", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type sessionFileInput struct {
	Connection string `json:"connection_id"`
	Event      string `json:"event_id"`
	Sequence   int64  `json:"lease_sequence"`
	Direction  string `json:"direction"`
	Result     string `json:"result"`
	Size       *int64 `json:"size_bytes"`
}

func (a *API) sessionFileEvent(w http.ResponseWriter, r *http.Request) {
	var in sessionFileInput
	if !readJSON(w, r, &in) {
		return
	}
	if !userIDPattern.MatchString(in.Event) || in.Sequence < 1 || in.Size == nil || *in.Size < 0 ||
		(in.Direction != "upload" && in.Direction != "download") ||
		(in.Result != "completed" && in.Result != "failed" && in.Result != "cancelled") {
		failure(w, "report file event", errSessionInput)
		return
	}
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		if err := lockSessionAuthorization(ctx, tx); err != nil {
			return err
		}
		g, lease, err := authenticateSessionReceipt(ctx, tx, r.PathValue("id"), in.Connection, managedBearer(r, sessionReceiptPrefix))
		if err != nil {
			return err
		}
		if byte(lease.Mask)&controlActionMask([]string{"file_transfer"}) == 0 || in.Sequence > lease.Sequence {
			return errSessionDenied
		}
		var sequence, size int64
		var direction, result string
		err = tx.QueryRow(ctx, `SELECT lease_sequence,direction,result,size_bytes FROM managed_session_file_events
			WHERE session_id=$1 AND event_id=$2`, g.ID, in.Event).Scan(&sequence, &direction, &result, &size)
		switch {
		case err == nil && sequence == in.Sequence && direction == in.Direction && result == in.Result && size == *in.Size:
			return nil
		case err == nil:
			return errSessionConflict
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO managed_session_file_events(session_id,event_id,lease_sequence,direction,result,size_bytes)
			VALUES($1,$2,$3,$4,$5,$6)`, g.ID, in.Event, in.Sequence, in.Direction, in.Result, *in.Size); err != nil {
			return err
		}
		return auditSessionEndpoint(ctx, tx, g, "session.file_reported", in.Result,
			fmt.Sprintf("%s %s %d", in.Event, in.Direction, *in.Size))
	})
	if err != nil {
		failure(w, "report file event", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func auditSessionEndpoint(ctx context.Context, tx pgx.Tx, g sessionGrant, action, result, reason string) error {
	var group string
	if err := tx.QueryRow(ctx, "SELECT group_id FROM devices WHERE id=$1", g.Target).Scan(&group); err != nil {
		return err
	}
	_, err := addAuditEvent(ctx, tx, auditEvent{OrganizationID: g.Organization, ActorID: g.User,
		ResourceID: g.Target, SessionID: g.ID, DeviceGroupID: group, Source: "managed_endpoint",
		Action: action, Result: result, Reason: reason})
	return err
}
