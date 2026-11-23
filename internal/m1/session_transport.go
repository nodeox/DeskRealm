package m1

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

const rendezvousPrefix, relayPrefix = "drr_", "drl_"

type ManagedTransport struct {
	rendezvous, relay []byte
	relayID           string
}

// ConfigureManagedTransport uses separate credentials for coordination and relay
// pairing receipts. Relay support may be left unset for direct-only operation.
func ConfigureManagedTransport(rendezvousToken, relayID, relayToken string) (ManagedTransport, error) {
	var out ManagedTransport
	if rendezvousToken == "" && relayID == "" && relayToken == "" {
		return out, nil
	}
	if !validToken(rendezvousToken) {
		return out, errors.New("managed rendezvous requires a 32-byte base64url token")
	}
	out.rendezvous = digest(rendezvousPrefix + rendezvousToken)
	if relayID == "" && relayToken == "" {
		return out, nil
	}
	if !validToken(relayToken) || !signingKeyID.MatchString(relayID) || rendezvousToken == relayToken {
		return ManagedTransport{}, errors.New("managed relay requires a valid ID and its own 32-byte base64url token")
	}
	out.relayID, out.relay = relayID, digest(relayPrefix+relayToken)
	return out, nil
}

type sessionRouteInput struct {
	Session    string `json:"session_id"`
	Grant      string `json:"grant_id"`
	Signature  string `json:"grant_signature"`
	Controller string `json:"controller_device_id"`
	Target     string `json:"target_device_id"`
	Connection string `json:"connection_id"`
	Path       string `json:"path"`
	Relay      string `json:"relay_id"`
}

type sessionRoute struct {
	Path, Relay string
	Paired      bool
}

func readSessionRoute(ctx context.Context, tx pgx.Tx, session, connection string) (sessionRoute, error) {
	var result sessionRoute
	err := tx.QueryRow(ctx, `SELECT path,COALESCE(relay_id,''),paired_at IS NOT NULL
		FROM managed_session_routes WHERE session_id=$1 AND connection_id=$2`, session, connection).
		Scan(&result.Path, &result.Relay, &result.Paired)
	return result, err
}

func canonicalSignature(value string) ([]byte, bool) {
	sig, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return sig, err == nil && len(sig) == ed25519.SignatureSize && base64.RawURLEncoding.EncodeToString(sig) == value
}

func validSessionPath(path, relayID string) bool {
	return (path == "direct" && relayID == "") || (path == "relay" && signingKeyID.MatchString(relayID))
}

func (a *API) authorizeSessionRoute(w http.ResponseWriter, r *http.Request) {
	pairing := r.URL.Path == "/v1/internal/managed/relay/paired"
	prefix, expected := rendezvousPrefix, a.transport.rendezvous
	if pairing {
		prefix, expected = relayPrefix, a.transport.relay
	}
	if a.managedKey == nil || expected == nil {
		failure(w, "authorize session route", errManagedUnavailable)
		return
	}
	if subtle.ConstantTimeCompare(managedBearer(r, prefix), expected) != 1 {
		failure(w, "authorize session route", errManagedIdentity)
		return
	}
	var req sessionRouteInput
	if !readJSON(w, r, &req) {
		return
	}
	sig, valid := canonicalSignature(req.Signature)
	if !valid || !userIDPattern.MatchString(req.Session) || !userIDPattern.MatchString(req.Grant) ||
		!userIDPattern.MatchString(req.Connection) || !userIDPattern.MatchString(req.Controller) ||
		!userIDPattern.MatchString(req.Target) || !validSessionPath(req.Path, req.Relay) ||
		(pairing && req.Path != "relay") {
		failure(w, "authorize session route", errSessionInput)
		return
	}
	ctx := r.Context()
	var rejected error
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		if err := lockSessionAuthorization(ctx, tx); err != nil {
			return err
		}
		g, err := readSessionGrant(tx.QueryRow(ctx, sessionGrantSelect+"WHERE id=$1 FOR NO KEY UPDATE", req.Session))
		if errors.Is(err, pgx.ErrNoRows) {
			return errSessionDenied
		}
		if err != nil {
			return err
		}
		ok, err := g.matchesSignature(a.managedKey, req.Grant, sig)
		if err != nil {
			return err
		}
		if !ok || g.Controller != req.Controller || g.Target != req.Target {
			return errSessionDenied
		}
		reason, group, err := g.revalidate(ctx, tx)
		if err != nil {
			return err
		}
		now, err := sessionClock(ctx, tx)
		if err != nil {
			return err
		}
		if !now.Before(g.Expires) {
			reason = "grant_expired"
		}
		if req.Path == "relay" && req.Relay != a.transport.relayID {
			reason = "relay_not_configured"
		}
		if reason != "" {
			rejected = errSessionDenied
			return auditSession(ctx, tx, g, group, "session.route_denied", "denied", reason)
		}
		redeemed, err := readSessionRedemption(ctx, tx, g.ID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && redeemed.Connection != req.Connection {
			rejected = errSessionConflict
			return auditSession(ctx, tx, g, group, "session.route_denied", "denied", "already_redeemed")
		}
		route, err := readSessionRoute(ctx, tx, g.ID, req.Connection)
		if err == nil {
			if route.Path != req.Path || route.Relay != req.Relay {
				rejected = errSessionConflict
				return auditSession(ctx, tx, g, group, "session.route_denied", "denied", "route_conflict")
			}
			if !pairing || route.Paired {
				return nil
			}
			if _, err := tx.Exec(ctx, `UPDATE managed_session_routes SET paired_at=clock_timestamp()
				WHERE session_id=$1 AND connection_id=$2`, g.ID, req.Connection); err != nil {
				return err
			}
			return auditSession(ctx, tx, g, group, "session.relay_paired", "allowed", req.Relay)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if pairing {
			rejected = errSessionConflict
			return auditSession(ctx, tx, g, group, "session.route_denied", "denied", "route_unavailable")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO managed_session_routes(session_id,connection_id,path,relay_id)
			VALUES($1,$2,$3,NULLIF($4,''))`, g.ID, req.Connection, req.Path, req.Relay); err != nil {
			return err
		}
		return auditSession(ctx, tx, g, group, "session.route_authorized", "allowed", req.Path)
	})
	if err == nil {
		err = rejected
	}
	if err != nil {
		failure(w, "authorize session route", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
