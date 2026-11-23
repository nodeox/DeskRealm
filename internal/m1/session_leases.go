package m1

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

const sessionLeaseDuration = 30 * time.Second
const sessionReceiptPrefix = "drc_"

type sessionActivity struct {
	Connection       string     `json:"connection_id,omitempty"`
	Sequence         int64      `json:"lease_sequence,omitempty"`
	LeaseUntil       *time.Time `json:"lease_until,omitempty"`
	RevocationAt     *time.Time `json:"revocation_requested_at,omitempty"`
	RevocationReason string     `json:"revocation_reason,omitempty"`
	StopReceivedAt   *time.Time `json:"stop_received_at,omitempty"`
	StopReason       string     `json:"stop_reason,omitempty"`
	LeaseExpiredAt   *time.Time `json:"lease_expired_at,omitempty"`
	State            string     `json:"state"`
}

const sessionActivityFrom = ` FROM managed_sessions g
	LEFT JOIN managed_session_redemptions r ON r.session_id=g.id
	LEFT JOIN managed_session_revocations v ON v.session_id=g.id
	LEFT JOIN managed_session_stops e ON e.session_id=g.id`
const currentSessionDeadline = `COALESCE(r.renewed_until,r.lease_until)`
const sessionActivitySelect = `COALESCE(r.connection_id::text,''),COALESCE(r.lease_sequence,0),` + currentSessionDeadline + `,
	v.requested_at,COALESCE(v.reason,''),e.received_at,COALESCE(e.reason,''),
	CASE WHEN ` + currentSessionDeadline + `<=statement_timestamp() AND
		(e.received_at IS NULL OR e.received_at>=` + currentSessionDeadline + `) THEN ` + currentSessionDeadline + ` END,
	CASE WHEN e.session_id IS NOT NULL THEN 'stopped'
		WHEN ` + currentSessionDeadline + `<=statement_timestamp() THEN 'lease_expired'
		WHEN v.session_id IS NOT NULL AND r.session_id IS NOT NULL THEN 'revocation_requested'
		WHEN v.session_id IS NOT NULL THEN 'revoked'
		WHEN r.session_id IS NOT NULL THEN 'active'
		WHEN g.expires_at<=statement_timestamp() THEN 'grant_expired'
		ELSE 'issued' END`

func (s *sessionActivity) destinations() []any {
	return []any{&s.Connection, &s.Sequence, &s.LeaseUntil, &s.RevocationAt, &s.RevocationReason,
		&s.StopReceivedAt, &s.StopReason, &s.LeaseExpiredAt, &s.State}
}

func readSessionActivity(ctx context.Context, tx pgx.Tx, id string) (sessionActivity, error) {
	var a sessionActivity
	err := tx.QueryRow(ctx, "SELECT "+sessionActivitySelect+sessionActivityFrom+" WHERE g.id=$1", id).Scan(a.destinations()...)
	return a, err
}

// The per-connection receipt credential is returned only to the authenticated
// redeemer. Its persisted digest stays valid after a device or signing-key
// change so a revoked device can reconcile the end of that connection.
func sessionReceiptCredential(key *ManagedSigningKey, g sessionGrant, connection string) string {
	mac := hmac.New(sha256.New, key.key.Seed())
	mac.Write([]byte("deskrealm/session-receipt/v1\x00"))
	mac.Write(managedFrameUUID(g.ID))
	mac.Write(managedFrameUUID(connection))
	return sessionReceiptPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func sessionTerminalReason(ctx context.Context, tx pgx.Tx, id, fallback string) (string, error) {
	var reason string
	err := tx.QueryRow(ctx, `SELECT COALESCE(
		(SELECT reason FROM managed_session_revocations WHERE session_id=$1),
		CASE WHEN EXISTS(SELECT 1 FROM managed_session_stops WHERE session_id=$1) THEN 'session_stopped' ELSE $2 END)`,
		id, fallback).Scan(&reason)
	return reason, err
}

func (g sessionGrant) revalidateLease(ctx context.Context, tx pgx.Tx) (reason, group string, err error) {
	var valid bool
	err = tx.QueryRow(ctx, `SELECT true FROM browser_sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_digest=$1 AND s.user_id=$2 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		AND u.status='active' FOR SHARE OF s`, g.BrowserDigest, g.User).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "controller_inactive", "", nil
	}
	if err != nil {
		return "", "", err
	}
	return g.revalidateAccess(ctx, tx)
}

func recordSessionRevocation(ctx context.Context, tx pgx.Tx, g sessionGrant, actorID, reason string) error {
	tag, err := tx.Exec(ctx, "INSERT INTO managed_session_revocations(session_id,reason) VALUES($1,$2) ON CONFLICT DO NOTHING", g.ID, reason)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	var group string
	var redeemed bool
	if err := tx.QueryRow(ctx, `SELECT group_id,EXISTS(SELECT 1 FROM managed_session_redemptions WHERE session_id=$2)
		FROM devices WHERE id=$1`, g.Target, g.ID).Scan(&group, &redeemed); err != nil {
		return err
	}
	if actorID == "" {
		actorID = g.User
	}
	event := auditEvent{OrganizationID: g.Organization, ActorID: actorID,
		ResourceID: g.Target, SessionID: g.ID, DeviceGroupID: group, Action: "session.revocation_requested", Result: "requested", Reason: reason}
	if redeemed {
		event.SourceKey = "session-revocation:" + g.ID
		event.ReceiptSource = "managed_endpoint"
	}
	_, err = addAuditEvent(ctx, tx, event)
	return err
}

// Callers hold the organization authorization lock used by issuance, renewal
// and the mutation being applied. Only outstanding grants and leases are evaluated.
func recheckSessions(ctx context.Context, tx pgx.Tx, scope string, args ...any) error {
	rows, err := tx.Query(ctx, sessionGrantSelect+`WHERE (`+scope+`)
		AND NOT EXISTS(SELECT 1 FROM managed_session_revocations v WHERE v.session_id=managed_sessions.id)
		AND NOT EXISTS(SELECT 1 FROM managed_session_stops e WHERE e.session_id=managed_sessions.id)
		AND (COALESCE((SELECT COALESCE(r.renewed_until,r.lease_until) FROM managed_session_redemptions r
			WHERE r.session_id=managed_sessions.id),expires_at)>statement_timestamp()) ORDER BY id`, args...)
	if err != nil {
		return err
	}
	grants, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (sessionGrant, error) { return readSessionGrant(row) })
	if err != nil {
		return err
	}
	for _, g := range grants {
		reason, _, err := g.revalidateLease(ctx, tx)
		if err != nil {
			return err
		}
		if reason != "" {
			if err := recordSessionRevocation(ctx, tx, g, "", reason); err != nil {
				return err
			}
		}
	}
	return nil
}

func authenticateSessionReceipt(ctx context.Context, tx pgx.Tx, id, connection string, credential []byte) (sessionGrant, sessionRedemption, error) {
	var empty sessionGrant
	var redemption sessionRedemption
	if credential == nil || !userIDPattern.MatchString(id) || !userIDPattern.MatchString(connection) {
		return empty, redemption, errManagedIdentity
	}
	r, err := readSessionRedemption(ctx, tx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil &&
		(r.Connection != connection || subtle.ConstantTimeCompare(r.ReceiptDigest, credential) != 1)) {
		return empty, redemption, errManagedIdentity
	}
	if err != nil {
		return empty, redemption, err
	}
	g, err := readSessionGrant(tx.QueryRow(ctx, sessionGrantSelect+"WHERE id=$1", id))
	return g, r, err
}

func (a *API) renewSession(w http.ResponseWriter, r *http.Request) {
	deviceHash := managedBearer(r, managedDevicePrefix)
	if deviceHash == nil {
		failure(w, "renew session", errManagedIdentity)
		return
	}
	var in struct {
		Connection string `json:"connection_id"`
		Receipt    string `json:"session_credential"`
		Previous   int64  `json:"lease_sequence"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Previous < 1 {
		failure(w, "renew session", errSessionInput)
		return
	}
	ctx := r.Context()
	var output sessionRedemption
	var rejected error
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		if err := lockSessionAuthorization(ctx, tx); err != nil {
			return err
		}
		g, lease, err := authenticateSessionReceipt(ctx, tx, r.PathValue("id"), in.Connection, managedDigest(in.Receipt, sessionReceiptPrefix))
		if err != nil {
			return err
		}
		device, err := lockRuntimeDevice(ctx, tx, g.Target)
		if err != nil {
			return err
		}
		protocol, _, err := device.credential(ctx, tx, deviceHash)
		if err != nil {
			return err
		}
		if protocol != managedProtocolV1 || !bytes.Equal(device.Key, g.TargetKey) {
			return errManagedIdentity
		}
		activity, err := readSessionActivity(ctx, tx, g.ID)
		if err != nil {
			return err
		}
		if activity.State != "active" {
			return errSessionDenied
		}
		reason, _, err := g.revalidateLease(ctx, tx)
		if err != nil {
			return err
		}
		if reason != "" {
			rejected = errSessionDenied
			return recordSessionRevocation(ctx, tx, g, "", reason)
		}
		now, err := sessionClock(ctx, tx)
		if err != nil {
			return err
		}
		if !now.Before(lease.CurrentUntil) {
			return errSessionDenied
		}
		switch in.Previous {
		case lease.Sequence - 1:
			output = lease
			return nil
		case lease.Sequence:
			lease.Sequence++
			lease.CurrentUntil = now.Add(sessionLeaseDuration)
		default:
			return errSessionConflict
		}
		_, err = tx.Exec(ctx, `UPDATE managed_session_redemptions SET lease_sequence=$2,renewed_at=$3,renewed_until=$4
			WHERE session_id=$1`, g.ID, lease.Sequence, now, lease.CurrentUntil)
		if err != nil {
			return err
		}
		output = lease
		return auditSession(ctx, tx, g, device.Group, "session.renewed", "allowed", "")
	})
	if err == nil {
		err = rejected
	}
	if err != nil {
		failure(w, "renew session", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": r.PathValue("id"), "connection_id": output.Connection, "lease_sequence": output.Sequence,
		"lease_until_ms": output.CurrentUntil.UnixMilli(), "allowed_actions": controlActionsFromMask(byte(output.Mask)),
	})
}
