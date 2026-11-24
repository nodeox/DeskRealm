package m1

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"time"

	"deskrealm/internal/managedproto"
	"github.com/jackc/pgx/v5"
)

const (
	relayController byte = 1
	relayTarget     byte = 2
)

type relayRound struct {
	ID                               string                `json:"round_id"`
	Connection                       string                `json:"connection_id"`
	Version                          int64                 `json:"version"`
	NetworkRevision                  int64                 `json:"network_revision"`
	Opened                           *time.Time            `json:"opened_at,omitempty"`
	Deadline                         *time.Time            `json:"deadline,omitempty"`
	State                            string                `json:"state"`
	Candidates                       []relayRoundCandidate `json:"candidates"`
	controllerAddress, targetAddress string
}

type relayRoundCandidate struct {
	ID         string            `json:"relay_id"`
	Site       string            `json:"site"`
	Region     string            `json:"region"`
	Endpoint   string            `json:"endpoint"`
	Build      string            `json:"build"`
	PublicKey  string            `json:"public_key"`
	Version    int64             `json:"config_version"`
	Eligible   bool              `json:"eligible"`
	Controller relayProbeSummary `json:"controller"`
	Target     relayProbeSummary `json:"target"`
}

type relayProbeSample struct {
	Result string  `json:"result"`
	RTT    *uint32 `json:"rtt_us,omitempty"`
}

type relayProbeSummary struct {
	State   string             `json:"state"`
	Median  *uint32            `json:"median_us,omitempty"`
	Samples []relayProbeSample `json:"samples,omitempty"`
}

func summarizeRelaySamples(samples []relayProbeSample, closed bool) relayProbeSummary {
	out := relayProbeSummary{Samples: samples}
	if len(samples) == 0 {
		out.State = "pending"
		if closed {
			out.State = "missing"
		}
		return out
	}
	var rtts []uint32
	for _, sample := range samples {
		if sample.Result == "ok" && sample.RTT != nil {
			rtts = append(rtts, *sample.RTT)
		}
	}
	if len(rtts) < 2 {
		out.State = "insufficient"
		return out
	}
	slices.Sort(rtts)
	median := rtts[1]
	if len(rtts) == 2 {
		median = (rtts[0] + rtts[1]) / 2
	}
	out.State, out.Median = "measured", &median
	return out
}

func relaySampleBytes(samples []relayProbeSample) ([]byte, bool) {
	if len(samples) != 3 {
		return nil, false
	}
	wire := make([]byte, 0, 15)
	for _, sample := range samples {
		switch sample.Result {
		case "ok":
			if sample.RTT == nil || *sample.RTT > 2_000_000 {
				return nil, false
			}
			wire = binary.BigEndian.AppendUint32(append(wire, 1), *sample.RTT)
		default:
			code := map[string]byte{"unreachable": 2, "timeout": 3, "protocol_mismatch": 4, "missing": 5}[sample.Result]
			if code == 0 || sample.RTT != nil {
				return nil, false
			}
			wire = binary.BigEndian.AppendUint32(append(wire, code), 0)
		}
	}
	return wire, true
}

func relayJoinFrame(g sessionGrant, role byte, connection string) ([]byte, error) {
	return managedproto.Encode(managedproto.RelayRoundJoin, []byte{role}, managedFrameUUID(g.ID), managedFrameUUID(g.Grant),
		managedFrameUUID(connection))
}

func relayReportFrame(g sessionGrant, role byte, round relayRound, node, endpoint string, version int64, wire []byte) ([]byte, error) {
	endpointHash := sha256.Sum256([]byte(endpoint))
	return managedproto.Encode(managedproto.RelayProbe, []byte{role}, managedFrameUUID(g.ID), managedFrameUUID(g.Grant),
		managedFrameUUID(round.Connection), managedFrameUUID(round.ID), managedFrameUUID(node),
		binary.BigEndian.AppendUint64(nil, uint64(version)), endpointHash[:], wire)
}

// Order round operations with the same authorization mutations as session
// routing, and prove the caller is one of the two endpoints of this grant.
func (a *API) withRelaySession(r *http.Request, fn func(pgx.Tx, sessionGrant, byte, ed25519.PublicKey, time.Time) error) error {
	if a.managedKey == nil {
		return errManagedUnavailable
	}
	ctx, id := r.Context(), r.PathValue("id")
	if !userIDPattern.MatchString(id) {
		return errSessionInput
	}
	role, hash := relayController, managedBearer(r, managedUserPrefix)
	if hash == nil {
		role, hash = relayTarget, managedBearer(r, managedDevicePrefix)
	}
	if hash == nil || len(r.Header.Values("Cookie")) != 0 {
		return errManagedIdentity
	}
	return pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
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
		key := g.ControllerKey
		if role == relayController {
			if subtle.ConstantTimeCompare(hash, g.ControllerDigest) != 1 {
				return errManagedIdentity
			}
		} else {
			device, err := lockRuntimeDevice(ctx, tx, g.Target)
			if err != nil {
				return err
			}
			protocol, _, err := device.credential(ctx, tx, hash)
			if err != nil {
				return err
			}
			if protocol != managedProtocolV1 {
				return errManagedIdentity
			}
			key = g.TargetKey
		}
		reason, _, err := g.revalidate(ctx, tx)
		if err != nil {
			return err
		}
		reason, err = sessionTerminalReason(ctx, tx, g.ID, reason)
		if err != nil {
			return err
		}
		now, err := sessionClock(ctx, tx)
		if err != nil {
			return err
		}
		if reason != "" || !now.Before(g.Expires) || g.Kid != a.managedKey.id {
			return errSessionDenied
		}
		var taken bool
		err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM managed_session_redemptions WHERE session_id=$1)", g.ID).Scan(&taken)
		if err != nil {
			return err
		}
		if taken {
			return errSessionConflict
		}
		return fn(tx, g, role, key, now)
	})
}

func readRelayRound(ctx context.Context, tx pgx.Tx, session, connection string) (relayRound, error) {
	out := relayRound{Connection: connection, Candidates: []relayRoundCandidate{}}
	err := tx.QueryRow(ctx, `SELECT id,version,network_revision,opened_at,deadline,host(controller_address),
		COALESCE(host(target_address),'') FROM session_relay_rounds
		WHERE session_id=$1 AND connection_id=$2 AND superseded_at IS NULL`, session, connection).
		Scan(&out.ID, &out.Version, &out.NetworkRevision, &out.Opened, &out.Deadline, &out.controllerAddress, &out.targetAddress)
	return out, err
}

func (out relayRound) matchesAddress(role byte, caller netip.Addr) bool {
	address := out.controllerAddress
	if role == relayTarget {
		address = out.targetAddress
	}
	return address == caller.String()
}

const relaySnapshotEligible = `relay_nodes.config_version=c.config_version AND relay_nodes.revision=c.node_revision
	AND relay_nodes.endpoint=c.endpoint AND (` + relayStateSQL + `)='available'`

func loadRelayRoundCandidates(ctx context.Context, tx pgx.Tx, round *relayRound, now time.Time) error {
	round.State = "waiting_for_target"
	if round.Deadline == nil {
		return nil
	}
	round.State = "collecting"
	closed := !now.Before(*round.Deadline)
	if closed {
		round.State = "complete"
	}
	rows, err := tx.Query(ctx, `SELECT c.relay_id,c.site,c.region,c.endpoint,c.build,c.identity_key,c.config_version,
		(`+relaySnapshotEligible+`),left_report.samples,right_report.samples
		FROM session_relay_candidates c JOIN relay_nodes ON relay_nodes.id=c.relay_id
		LEFT JOIN session_relay_reports left_report ON left_report.round_id=c.round_id AND left_report.relay_id=c.relay_id AND left_report.role=1
		LEFT JOIN session_relay_reports right_report ON right_report.round_id=c.round_id AND right_report.relay_id=c.relay_id AND right_report.role=2
		WHERE c.round_id=$1 ORDER BY c.relay_id`, round.ID)
	if err != nil {
		return err
	}
	round.Candidates, err = pgx.AppendRows(round.Candidates, rows, func(row pgx.CollectableRow) (relayRoundCandidate, error) {
		var candidate relayRoundCandidate
		var key []byte
		var left, right []relayProbeSample
		err := row.Scan(&candidate.ID, &candidate.Site, &candidate.Region, &candidate.Endpoint, &candidate.Build,
			&key, &candidate.Version, &candidate.Eligible, &left, &right)
		candidate.PublicKey = base64.RawURLEncoding.EncodeToString(key)
		candidate.Controller, candidate.Target = summarizeRelaySamples(left, closed), summarizeRelaySamples(right, closed)
		return candidate, err
	})
	return err
}

func (a *API) getSessionRelayRound(w http.ResponseWriter, r *http.Request) {
	connection := r.URL.Query().Get("connection_id")
	if !userIDPattern.MatchString(connection) {
		failure(w, "read relay round", errSessionInput)
		return
	}
	caller, valid := a.relayIngress.clientIP(r)
	if !valid {
		failure(w, "read relay round", errSessionDenied)
		return
	}
	var out relayRound
	err := a.withRelaySession(r, func(tx pgx.Tx, g sessionGrant, role byte, _ ed25519.PublicKey, now time.Time) error {
		var err error
		out, err = readRelayRound(r.Context(), tx, g.ID, connection)
		if errors.Is(err, pgx.ErrNoRows) {
			return errSessionConflict
		}
		if err != nil {
			return err
		}
		if !out.matchesAddress(role, caller) {
			return errSessionConflict
		}
		return loadRelayRoundCandidates(r.Context(), tx, &out, now)
	})
	if err != nil {
		failure(w, "read relay round", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) joinSessionRelayRound(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Connection string `json:"connection_id"`
		Signature  string `json:"signature"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	signature, valid := canonicalSignature(in.Signature)
	if !valid || !userIDPattern.MatchString(in.Connection) {
		failure(w, "join relay round", errSessionInput)
		return
	}
	caller, valid := a.relayIngress.clientIP(r)
	if !valid {
		failure(w, "join relay round", errSessionDenied)
		return
	}
	ctx := r.Context()
	var out relayRound
	err := a.withRelaySession(r, func(tx pgx.Tx, g sessionGrant, role byte, key ed25519.PublicKey, now time.Time) error {
		frame, err := relayJoinFrame(g, role, in.Connection)
		if err != nil || !ed25519.Verify(key, frame, signature) {
			return errManagedIdentity
		}
		out, err = readRelayRound(ctx, tx, g.ID, in.Connection)
		if errors.Is(err, pgx.ErrNoRows) {
			if role != relayController {
				return errSessionConflict
			}
			policy, err := readRelayNetworkPolicy(ctx, tx, g.Organization)
			if err != nil {
				return err
			}
			out, err = createRelayRound(ctx, tx, g.ID, in.Connection, caller, policy.Revision)
			if err != nil {
				return err
			}
			return loadRelayRoundCandidates(ctx, tx, &out, now)
		}
		if err != nil {
			return err
		}
		if role == relayTarget && out.Opened == nil {
			if err := openRelayRound(ctx, tx, g, &out, caller, now); err != nil {
				return err
			}
		} else if !out.matchesAddress(role, caller) {
			return errSessionConflict
		}
		return loadRelayRoundCandidates(ctx, tx, &out, now)
	})
	if err != nil {
		failure(w, "join relay round", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func openRelayRound(ctx context.Context, tx pgx.Tx, g sessionGrant, out *relayRound, caller netip.Addr, now time.Time) error {
	policy, err := readRelayNetworkPolicy(ctx, tx, g.Organization)
	if err != nil {
		return err
	}
	if out.NetworkRevision != policy.Revision {
		return errSessionConflict
	}
	controller, err := netip.ParseAddr(out.controllerAddress)
	if err != nil {
		return err
	}
	deadline := now.Add(2 * time.Second)
	if g.Expires.Before(deadline) {
		deadline = g.Expires
	}
	if _, err := tx.Exec(ctx, `UPDATE session_relay_rounds SET opened_at=$2,deadline=$3,target_address=$4
		WHERE id=$1`, out.ID, now, deadline, caller); err != nil {
		return err
	}
	scope := policy.scope(controller, caller)
	_, err = tx.Exec(ctx, `INSERT INTO session_relay_candidates
		(round_id,relay_id,site,region,endpoint,build,identity_key,config_version,node_revision)
		SELECT $1,id,site,region,endpoint,build,identity_key,config_version,revision FROM relay_nodes
		WHERE organization_id=$2 AND (`+relayStateSQL+`)='available'
			AND ($3 OR (region=ANY($4::text[]) AND id=ANY($5::uuid[])))`,
		out.ID, g.Organization, policy.Mode == "organization", scope.regions, scope.nodes)
	if err == nil {
		out.Opened, out.Deadline, out.targetAddress = &now, &deadline, caller.String()
	}
	return err
}

func createRelayRound(ctx context.Context, tx pgx.Tx, session, connection string, caller netip.Addr, networkRevision int64) (relayRound, error) {
	out := relayRound{Connection: connection, Candidates: []relayRoundCandidate{},
		NetworkRevision: networkRevision, controllerAddress: caller.String()}
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM session_relay_rounds
		WHERE session_id=$1 AND connection_id=$2)`, session, connection).Scan(&exists)
	if err != nil {
		return out, err
	}
	if exists {
		return out, errSessionConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE session_relay_rounds SET superseded_at=clock_timestamp()
		WHERE session_id=$1 AND superseded_at IS NULL`, session); err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO session_relay_rounds(session_id,connection_id,version,network_revision,controller_address)
		SELECT $1,$2,COALESCE(max(version),0)+1,$3,$4 FROM session_relay_rounds WHERE session_id=$1 RETURNING id,version`,
		session, connection, networkRevision, caller).Scan(&out.ID, &out.Version)
	return out, err
}

func (a *API) reportSessionRelayProbe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Connection string             `json:"connection_id"`
		Round      string             `json:"round_id"`
		Relay      string             `json:"relay_id"`
		Samples    []relayProbeSample `json:"samples"`
		Signature  string             `json:"signature"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	signature, valid := canonicalSignature(in.Signature)
	wire, samplesValid := relaySampleBytes(in.Samples)
	if !valid || !samplesValid || !userIDPattern.MatchString(in.Connection) ||
		!userIDPattern.MatchString(in.Round) || !userIDPattern.MatchString(in.Relay) {
		failure(w, "report relay probe", errSessionInput)
		return
	}
	caller, valid := a.relayIngress.clientIP(r)
	if !valid {
		failure(w, "report relay probe", errSessionDenied)
		return
	}
	ctx := r.Context()
	err := a.withRelaySession(r, func(tx pgx.Tx, g sessionGrant, role byte, key ed25519.PublicKey, now time.Time) error {
		round, err := readRelayRound(ctx, tx, g.ID, in.Connection)
		if errors.Is(err, pgx.ErrNoRows) {
			return errSessionConflict
		}
		if err != nil {
			return err
		}
		if round.ID != in.Round || round.Deadline == nil || !now.Before(*round.Deadline) || !round.matchesAddress(role, caller) {
			return errSessionConflict
		}
		var endpoint string
		var version int64
		err = tx.QueryRow(ctx, `SELECT c.endpoint,c.config_version FROM session_relay_candidates c
			JOIN relay_nodes ON relay_nodes.id=c.relay_id WHERE c.round_id=$1 AND c.relay_id=$2 AND (`+relaySnapshotEligible+`)
			FOR SHARE OF relay_nodes`, round.ID, in.Relay).Scan(&endpoint, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return errSessionConflict
		}
		if err != nil {
			return err
		}
		frame, err := relayReportFrame(g, role, round, in.Relay, endpoint, version, wire)
		if err != nil || !ed25519.Verify(key, frame, signature) {
			return errManagedIdentity
		}
		result, err := tx.Exec(ctx, `INSERT INTO session_relay_reports(round_id,relay_id,role,samples,received_at)
			SELECT $1,$2,$3,$4,clock_timestamp() WHERE clock_timestamp()<$5::timestamptz
			ON CONFLICT DO NOTHING`, round.ID, in.Relay, int16(role), in.Samples, round.Deadline)
		if err == nil && result.RowsAffected() != 1 {
			err = errSessionConflict
		}
		return err
	})
	if err != nil {
		failure(w, "report relay probe", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
