package m1

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"time"

	"deskrealm/internal/managedproto"
	"github.com/jackc/pgx/v5"
)

type relaySessionTicket struct {
	Frame     string `json:"frame"`
	Signature string `json:"signature"`
	PublicKey string `json:"public_key"`
}

func regionalRelayEnabled(ctx context.Context, tx pgx.Tx, organization string) (bool, error) {
	policy, err := readRelayNetworkPolicy(ctx, tx, organization)
	if err != nil || policy.Mode == "restricted" {
		return policy.Mode == "restricted", err
	}
	var enrolled bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM relay_nodes WHERE organization_id=$1 AND registered_at IS NOT NULL)`, organization).Scan(&enrolled)
	return enrolled, err
}

func (a *API) signRelayTicket(ctx context.Context, tx pgx.Tx, g sessionGrant, round relayRound, role byte,
	holder ed25519.PublicKey, selection relaySelectionResult) (*relaySessionTicket, error) {
	chosen := selection.Selected
	var key []byte
	err := tx.QueryRow(ctx, `SELECT identity_key FROM session_relay_candidates WHERE round_id=$1 AND relay_id=$2`,
		round.ID, chosen.Relay).Scan(&key)
	if err != nil {
		return nil, err
	}
	endpoint := sha256.Sum256([]byte(chosen.Endpoint))
	frame, err := managedproto.Encode(managedproto.RelayTicket, []byte{role}, managedFrameUUID(g.ID),
		managedFrameUUID(g.Grant), managedFrameUUID(round.Connection), managedFrameUUID(round.ID), managedFrameUUID(chosen.Relay),
		binary.BigEndian.AppendUint64(nil, uint64(chosen.Version)), endpoint[:], holder,
		binary.BigEndian.AppendUint64(nil, uint64(round.Deadline.UnixMilli())),
		binary.BigEndian.AppendUint64(nil, uint64(selection.Deadline.UnixMilli())))
	if err != nil {
		return nil, err
	}
	return &relaySessionTicket{Frame: base64.RawURLEncoding.EncodeToString(frame), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(a.managedKey.key, frame)),
		PublicKey: base64.RawURLEncoding.EncodeToString(key)}, nil
}

type relayPairingInput struct {
	Connection string `json:"connection_id"`
	Round      string `json:"round_id"`
	Relay      string `json:"relay_id"`
	Signature  string `json:"signature"`
}

func (in relayPairingInput) frame(g sessionGrant, version int64) ([]byte, error) {
	return managedproto.Encode(managedproto.RelayPairReceipt, managedFrameUUID(g.ID), managedFrameUUID(g.Grant),
		managedFrameUUID(in.Connection), managedFrameUUID(in.Round), managedFrameUUID(in.Relay),
		binary.BigEndian.AppendUint64(nil, uint64(version)))
}

func (a *API) recordRegionalPairing(w http.ResponseWriter, r *http.Request) {
	var in relayPairingInput
	if !readJSON(w, r, &in) {
		return
	}
	signature, valid := canonicalSignature(in.Signature)
	if !valid || !userIDPattern.MatchString(in.Connection) || !userIDPattern.MatchString(in.Round) || !userIDPattern.MatchString(in.Relay) {
		failure(w, "record regional pairing", errSessionInput)
		return
	}
	address, valid := a.relayIngress.clientIP(r)
	if !valid {
		failure(w, "record regional pairing", errSessionDenied)
		return
	}
	ctx := r.Context()
	err := a.withRelaySession(r, func(tx pgx.Tx, g sessionGrant, role byte, _ ed25519.PublicKey, now time.Time) error {
		if role != relayTarget {
			return errManagedIdentity
		}
		round, err := readRelayRound(ctx, tx, g.ID, in.Connection)
		if errors.Is(err, pgx.ErrNoRows) {
			return errSessionConflict
		}
		if err != nil {
			return err
		}
		if round.ID != in.Round || !round.matchesAddress(role, address) || round.Deadline == nil || now.Before(*round.Deadline) {
			return errSessionConflict
		}
		deadline := minTime(g.Expires, round.Deadline.Add(relaySelectionWindow))
		key, version, err := regionalPairingNode(ctx, tx, round.ID, in.Relay)
		if errors.Is(err, pgx.ErrNoRows) {
			return errSessionConflict
		}
		if err != nil {
			return err
		}
		frame, err := in.frame(g, version)
		if err != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, frame, signature) {
			return errManagedIdentity
		}
		return recordRegionalRoute(ctx, tx, g, in, deadline)
	})
	if err != nil {
		failure(w, "record regional pairing", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func regionalPairingNode(ctx context.Context, tx pgx.Tx, round, relay string) ([]byte, int64, error) {
	var key []byte
	var version int64
	err := tx.QueryRow(ctx, `SELECT c.identity_key,c.config_version FROM session_relay_selections s
		JOIN session_relay_candidates c USING (round_id,relay_id) JOIN relay_nodes ON relay_nodes.id=c.relay_id
		WHERE s.round_id=$1 AND s.relay_id=$2 AND s.failed_at IS NULL AND relay_nodes.config_version=c.config_version
		AND relay_nodes.revision=c.node_revision AND relay_nodes.endpoint=c.endpoint
		AND (`+relayStateSQL+`) IN ('available','at_capacity') FOR SHARE OF relay_nodes`, round, relay).Scan(&key, &version)
	return key, version, err
}

func verifyRegionalRedemption(ctx context.Context, tx pgx.Tx, session, connection, relay string) error {
	round, err := readRelayRound(ctx, tx, session, connection)
	if err != nil {
		return err
	}
	_, _, err = regionalPairingNode(ctx, tx, round.ID, relay)
	return err
}

func recordRegionalRoute(ctx context.Context, tx pgx.Tx, g sessionGrant, in relayPairingInput, deadline time.Time) error {
	current, err := readSessionRoute(ctx, tx, g.ID, in.Connection)
	if err == nil {
		if current.Path != "relay" || current.Relay != in.Relay || !current.Paired {
			return errSessionConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	result, err := tx.Exec(ctx, `INSERT INTO managed_session_routes(session_id,connection_id,path,relay_id,paired_at)
		SELECT $1,$2,'relay',$3,clock_timestamp() WHERE clock_timestamp()<$4::timestamptz`, g.ID, in.Connection, in.Relay, deadline)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return errSessionConflict
	}
	return auditRelayDecision(ctx, tx, g, "session.relay_paired", "allowed", in.Relay)
}
