package m1

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"deskrealm/internal/managedproto"

	"github.com/jackc/pgx/v5"
)

type sessionRedeemInput struct {
	Grant               string   `json:"grant_id"`
	GrantSignature      string   `json:"grant_signature"`
	Connection          string   `json:"connection_id"`
	Path                string   `json:"path"`
	Relay               string   `json:"relay_id"`
	Channel             string   `json:"channel_sha256"`
	ControllerNonce     string   `json:"controller_nonce"`
	TargetNonce         string   `json:"target_nonce"`
	ControllerSignature string   `json:"controller_signature"`
	TargetSignature     string   `json:"target_signature"`
	ConfirmedActions    []string `json:"confirmed_actions"`
	ConsentSignature    string   `json:"consent_signature"`
}

type sessionProof struct {
	controllerSig, targetSig, grantSig, consentSig []byte
	channel, controllerNonce, targetNonce          []byte
	confirmed                                      byte
}

func (in sessionRedeemInput) parse() (sessionProof, bool) {
	var p sessionProof
	if !userIDPattern.MatchString(in.Grant) || !userIDPattern.MatchString(in.Connection) || !validSessionPath(in.Path, in.Relay) ||
		!validToken(in.ControllerNonce) || !validToken(in.TargetNonce) || in.ControllerNonce == in.TargetNonce {
		return p, false
	}
	var err error
	p.channel, err = hex.DecodeString(in.Channel)
	if err != nil || len(p.channel) != sha256.Size || hex.EncodeToString(p.channel) != in.Channel {
		return p, false
	}
	p.controllerNonce, _ = base64.RawURLEncoding.Strict().DecodeString(in.ControllerNonce)
	p.targetNonce, _ = base64.RawURLEncoding.Strict().DecodeString(in.TargetNonce)

	for _, field := range []struct {
		text string
		dest *[]byte
	}{{in.ControllerSignature, &p.controllerSig}, {in.TargetSignature, &p.targetSig}, {in.GrantSignature, &p.grantSig}} {
		sig, ok := canonicalSignature(field.text)
		if !ok {
			return p, false
		}
		*field.dest = sig
	}
	if in.ConsentSignature != "" || len(in.ConfirmedActions) > 0 {
		sig, ok := canonicalSignature(in.ConsentSignature)
		if !ok || !distinctControlActions(in.ConfirmedActions) {
			return p, false
		}
		p.consentSig, p.confirmed = sig, controlActionMask(in.ConfirmedActions)
	}
	return p, true
}

type sessionRedemption struct {
	Connection, Path, Relay      string
	Mask                         int16
	ProofHash, ReceiptDigest     []byte
	Started, Until, CurrentUntil time.Time
	Sequence                     int64
}

func readSessionRedemption(ctx context.Context, tx pgx.Tx, id string) (sessionRedemption, error) {
	var out sessionRedemption
	err := tx.QueryRow(ctx, `SELECT connection_id,path,COALESCE(relay_id,''),allowed_mask,proof_hash,receipt_digest,redeemed_at,lease_until,COALESCE(renewed_until,lease_until),lease_sequence
		FROM managed_session_redemptions WHERE session_id=$1`, id).
		Scan(&out.Connection, &out.Path, &out.Relay, &out.Mask, &out.ProofHash, &out.ReceiptDigest, &out.Started, &out.Until, &out.CurrentUntil, &out.Sequence)
	return out, err
}

func (p sessionProof) verify(g sessionGrant, in sessionRedeemInput) (hash [32]byte, allowed byte, reason string, err error) {
	grant, err := g.signingFrame()
	if err != nil {
		return hash, 0, "", err
	}
	grantHash := sha256.Sum256(append(grant, g.Signature...))
	path := byte(1)
	if in.Path == "relay" {
		path = 2
	}
	frame := func(role byte) ([]byte, error) {
		return managedproto.Encode(managedproto.ConnectionProof, []byte{role}, grantHash[:], managedFrameUUID(g.ID),
			managedFrameUUID(in.Connection), p.channel, p.controllerNonce, p.targetNonce, []byte{path})
	}
	controllerFrame, err := frame(1)
	if err != nil {
		return hash, 0, "", err
	}
	targetFrame, err := frame(2)
	if err != nil {
		return hash, 0, "", err
	}
	if !ed25519.Verify(g.ControllerKey, controllerFrame, p.controllerSig) ||
		!ed25519.Verify(g.TargetKey, targetFrame, p.targetSig) {
		return hash, 0, "invalid_connection_proof", nil
	}
	allowed = byte(g.Mask)
	if g.Mode == "attended" {
		if p.confirmed == 0 || p.confirmed&allowed != p.confirmed {
			return hash, 0, "local_confirmation_required", nil
		}
		confirm, err := managedproto.Encode(managedproto.SessionConsent, grantHash[:], managedFrameUUID(g.ID),
			managedFrameUUID(in.Connection), p.channel, []byte{p.confirmed})
		if err != nil {
			return hash, 0, "", err
		}
		if !ed25519.Verify(g.TargetKey, confirm, p.consentSig) {
			return hash, 0, "invalid_local_confirmation", nil
		}
		allowed = p.confirmed
	} else if p.confirmed != 0 {
		return hash, 0, "confirmation_mode_conflict", nil
	}
	canonical := append(controllerFrame, p.confirmed)
	canonical = append(canonical, in.Relay...)
	return sha256.Sum256(canonical), allowed, "", nil
}

func (a *API) redeemSessionGrant(w http.ResponseWriter, r *http.Request) {
	deviceHash := managedBearer(r, managedDevicePrefix)
	id := r.PathValue("id")
	if deviceHash == nil || !userIDPattern.MatchString(id) {
		failure(w, "redeem session grant", errManagedIdentity)
		return
	}
	if a.managedKey == nil {
		failure(w, "redeem session grant", errManagedUnavailable)
		return
	}
	var in sessionRedeemInput
	if !readJSON(w, r, &in) {
		return
	}
	proofs, ok := in.parse()
	if !ok {
		failure(w, "redeem session grant", errSessionInput)
		return
	}
	ctx := r.Context()
	var out sessionRedemption
	var receipt string
	var rejected error
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		if err := lockSessionAuthorization(ctx, tx); err != nil {
			return err
		}
		g, err := readSessionGrant(tx.QueryRow(ctx, sessionGrantSelect+"WHERE id=$1 FOR NO KEY UPDATE", id))
		if errors.Is(err, pgx.ErrNoRows) {
			return errManagedIdentity
		}
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
		deny := func(cause error, reason string) error {
			rejected = cause
			return auditSession(ctx, tx, g, device.Group, "session.redeem_denied", "denied", reason)
		}
		ok, err := g.matchesSignature(a.managedKey, in.Grant, proofs.grantSig)
		if err != nil {
			return err
		}
		if !ok || protocol != managedProtocolV1 || !bytes.Equal(device.Key, g.TargetKey) {
			return deny(errSessionDenied, "grant_binding_changed")
		}
		hash, mask, reason, err := proofs.verify(g, in)
		if err != nil {
			return err
		}
		if reason != "" {
			return deny(errSessionDenied, reason)
		}
		if in.Path == "relay" && !userIDPattern.MatchString(in.Relay) && in.Relay != a.transport.relayID {
			return deny(errSessionDenied, "relay_not_configured")
		}
		reason, _, err = g.revalidate(ctx, tx)
		if err == nil {
			reason, err = sessionTerminalReason(ctx, tx, g.ID, reason)
		}
		if err != nil {
			return err
		}
		if reason != "" {
			return deny(errSessionDenied, reason)
		}
		now, err := sessionClock(ctx, tx)
		if err != nil {
			return err
		}
		receipt = sessionReceiptCredential(a.managedKey, g, in.Connection)
		receiptDigest := digest(receipt)
		prior, err := readSessionRedemption(ctx, tx, g.ID)
		if err == nil {
			if prior.Connection != in.Connection || !bytes.Equal(prior.ProofHash, hash[:]) {
				return deny(errSessionConflict, "redemption_conflict")
			}
			activity, err := readSessionActivity(ctx, tx, g.ID)
			if err != nil {
				return err
			}
			if activity.State != "active" || !now.Before(prior.Until) {
				return deny(errSessionDenied, "initial_lease_unavailable")
			}
			if prior.ReceiptDigest == nil {
				if _, err := tx.Exec(ctx, "UPDATE managed_session_redemptions SET receipt_digest=$2 WHERE session_id=$1", g.ID, receiptDigest); err != nil {
					return err
				}
			}
			out = prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !now.Before(g.Expires) {
			return deny(errSessionDenied, "grant_expired")
		}
		route, err := readSessionRoute(ctx, tx, g.ID, in.Connection)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (route.Path != in.Path || route.Relay != in.Relay ||
			(in.Path == "relay" && !route.Paired))) {
			return deny(errSessionDenied, "route_evidence_missing")
		}
		if err != nil {
			return err
		}
		if in.Path == "relay" && userIDPattern.MatchString(in.Relay) {
			err := verifyRegionalRedemption(ctx, tx, g.ID, in.Connection, in.Relay)
			if errors.Is(err, pgx.ErrNoRows) {
				return deny(errSessionDenied, "route_evidence_missing")
			}
			if err != nil {
				return err
			}
		}
		out = sessionRedemption{Connection: in.Connection, Path: in.Path, Relay: in.Relay, Mask: int16(mask),
			ProofHash: hash[:], Started: now, Until: now.Add(sessionLeaseDuration)}
		_, err = tx.Exec(ctx, `INSERT INTO managed_session_redemptions(session_id,connection_id,path,relay_id,allowed_mask,proof_hash,redeemed_at,lease_until,receipt_digest)
			VALUES($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9)`, id, in.Connection, in.Path, in.Relay, out.Mask, out.ProofHash, out.Started, out.Until, receiptDigest)
		if err != nil {
			return err
		}
		return auditSession(ctx, tx, g, device.Group, "session.redeemed", "allowed", in.Path)
	})
	if err == nil {
		err = rejected
	}
	if err != nil {
		failure(w, "redeem session grant", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": id, "connection_id": out.Connection, "path": out.Path, "relay_id": out.Relay,
		"allowed_actions": controlActionsFromMask(byte(out.Mask)), "lease_sequence": 1,
		"redeemed_ms": out.Started.UnixMilli(), "lease_until_ms": out.Until.UnixMilli(), "session_credential": receipt,
	})
}
