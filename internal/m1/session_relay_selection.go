package m1

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"time"

	"deskrealm/internal/managedproto"
	"github.com/jackc/pgx/v5"
)

const relaySelectionWindow = 10 * time.Second

type relaySelectionRequest struct {
	Connection string `json:"connection_id"`
	Round      string `json:"round_id"`
	Relay      string `json:"relay_id"`
	Action     string `json:"action"`
	Signature  string `json:"signature"`
}

type relaySelectionOption struct {
	Relay       string  `json:"relay_id"`
	Endpoint    string  `json:"endpoint"`
	Version     int64   `json:"config_version"`
	Reason      string  `json:"reason"`
	CombinedRTT *uint32 `json:"combined_rtt_us,omitempty"`
}

type relaySelectionResult struct {
	Round      string                 `json:"round_id"`
	Connection string                 `json:"connection_id"`
	Deadline   time.Time              `json:"deadline"`
	State      string                 `json:"state"`
	Selected   *relaySelectionOption  `json:"selected,omitempty"`
	Options    []relaySelectionOption `json:"options,omitempty"`
	Ticket     *relaySessionTicket    `json:"ticket,omitempty"`
}

type relayRank struct {
	relaySelectionOption
	site, geo, defaultRegion int
	active, capacity         int64
}

func relayDefinitelyUnusable(samples []relayProbeSample) bool {
	if len(samples) != 3 {
		return false
	}
	for _, sample := range samples {
		if sample.Result != "unreachable" && sample.Result != "protocol_mismatch" {
			return false
		}
	}
	return true
}

func orderRelayOptions(round relayRound, policy relayNetworkPolicy, failed map[string]bool) []relaySelectionOption {
	left, _ := netip.ParseAddr(round.controllerAddress)
	right, _ := netip.ParseAddr(round.targetAddress)
	sites := []string{policy.site(left), policy.site(right)}
	regions := []string{policy.geoRegion(left), policy.geoRegion(right)}
	var ranked []relayRank
	for _, node := range round.Candidates {
		if !node.Eligible || failed[node.ID] || relayDefinitelyUnusable(node.Controller.Samples) ||
			relayDefinitelyUnusable(node.Target.Samples) {
			continue
		}
		option := relayRank{
			relaySelectionOption: relaySelectionOption{Relay: node.ID, Endpoint: node.Endpoint,
				Version: node.Version, Reason: "available"},
			active: node.active, capacity: node.capacity,
		}
		for _, site := range sites {
			if site == node.Site {
				option.site++
			}
		}
		for _, region := range regions {
			if region == node.Region {
				option.geo++
			}
		}
		if node.Region == policy.DefaultRegion {
			option.defaultRegion = 1
		}
		switch {
		case node.Controller.Median != nil && node.Target.Median != nil:
			sum := *node.Controller.Median + *node.Target.Median
			option.CombinedRTT, option.Reason = &sum, "dual_rtt"
		case option.site > 0:
			option.Reason = "site"
		case option.geo > 0:
			option.Reason = "geoip"
		case option.defaultRegion > 0:
			option.Reason = "default_region"
		}
		ranked = append(ranked, option)
	}
	slices.SortFunc(ranked, compareRelayOptions)
	out := make([]relaySelectionOption, 0, len(ranked))
	for _, item := range ranked {
		out = append(out, item.relaySelectionOption)
	}
	return out
}

func compareRelayOptions(a, b relayRank) int {
	var order int
	switch {
	case a.CombinedRTT != nil && b.CombinedRTT != nil:
		order = cmp.Compare(*a.CombinedRTT, *b.CombinedRTT)
	case a.CombinedRTT != nil:
		return -1
	case b.CombinedRTT != nil:
		return 1
	default:
		for _, preference := range []int{cmp.Compare(b.site, a.site), cmp.Compare(b.geo, a.geo),
			cmp.Compare(b.defaultRegion, a.defaultRegion)} {
			if preference != 0 {
				return preference
			}
		}
	}
	if order != 0 {
		return order
	}
	if load := cmp.Compare(a.active*b.capacity, b.active*a.capacity); load != 0 {
		return load
	}
	return cmp.Compare(a.Relay, b.Relay)
}

func (in relaySelectionRequest) frame(g sessionGrant) ([]byte, error) {
	action := byte(1)
	if in.Action == "fail" {
		action = 2
	}
	return managedproto.Encode(managedproto.RelaySelection, []byte{action}, managedFrameUUID(g.ID), managedFrameUUID(g.Grant),
		managedFrameUUID(in.Connection), managedFrameUUID(in.Round), managedFrameUUID(in.Relay))
}

func (a *API) relaySelection(w http.ResponseWriter, r *http.Request) {
	change := r.Method == http.MethodPost
	in := relaySelectionRequest{Connection: r.URL.Query().Get("connection_id")}
	if change && !readJSON(w, r, &in) {
		return
	}
	sig, signed := canonicalSignature(in.Signature)
	if !userIDPattern.MatchString(in.Connection) ||
		(change && (!signed || !userIDPattern.MatchString(in.Round) || !userIDPattern.MatchString(in.Relay) ||
			(in.Action != "select" && in.Action != "fail"))) {
		failure(w, "relay selection", errSessionInput)
		return
	}
	address, valid := a.relayIngress.clientIP(r)
	if !valid {
		failure(w, "relay selection", errSessionDenied)
		return
	}
	ctx := r.Context()
	var out relaySelectionResult
	err := a.withRelaySession(r, func(tx pgx.Tx, g sessionGrant, role byte, key ed25519.PublicKey, now time.Time) error {
		if change {
			frame, err := in.frame(g)
			if role != relayController || err != nil || !ed25519.Verify(key, frame, sig) {
				return errManagedIdentity
			}
		}
		round, err := readRelayRound(ctx, tx, g.ID, in.Connection)
		if errors.Is(err, pgx.ErrNoRows) {
			return errSessionConflict
		}
		if err != nil {
			return err
		}
		if !round.matchesAddress(role, address) || round.Deadline == nil || now.Before(*round.Deadline) ||
			(change && in.Round != round.ID) {
			return errSessionConflict
		}
		out = relaySelectionResult{Round: round.ID, Connection: round.Connection,
			Deadline: minTime(g.Expires, round.Deadline.Add(relaySelectionWindow))}
		if !now.Before(out.Deadline) {
			return errSessionConflict
		}
		if err := a.selectRelay(ctx, tx, g, role, round, in, &out); err != nil || out.State != "confirmed" {
			return err
		}
		out.Ticket, err = a.signRelayTicket(ctx, tx, g, round, role, key, out)
		return err
	})
	if err != nil {
		failure(w, "relay selection", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func readRelaySelectionAttempts(ctx context.Context, tx pgx.Tx, round string) (int, *relaySelectionOption, map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT s.attempt,s.relay_id,c.endpoint,c.config_version,s.reason,s.combined_rtt_us,s.failed_at IS NOT NULL
		FROM session_relay_selections s JOIN session_relay_candidates c USING (round_id,relay_id)
		WHERE s.round_id=$1 ORDER BY s.attempt`, round)
	if err != nil {
		return 0, nil, nil, err
	}
	defer rows.Close()
	var count int
	var current *relaySelectionOption
	failed := make(map[string]bool)
	for rows.Next() {
		var option relaySelectionOption
		var unsuccessful bool
		if err := rows.Scan(&count, &option.Relay, &option.Endpoint, &option.Version, &option.Reason,
			&option.CombinedRTT, &unsuccessful); err != nil {
			return 0, nil, nil, err
		}
		if unsuccessful {
			failed[option.Relay] = true
		} else {
			current = &option
		}
	}
	return count, current, failed, rows.Err()
}

func (a *API) selectRelay(ctx context.Context, tx pgx.Tx, g sessionGrant, role byte, round relayRound,
	in relaySelectionRequest, out *relaySelectionResult) error {
	attempt, current, failed, err := readRelaySelectionAttempts(ctx, tx, round.ID)
	if err != nil {
		return err
	}
	if in.Action == "fail" {
		if current == nil || current.Relay != in.Relay {
			if !failed[in.Relay] {
				return errSessionConflict
			}
		} else {
			tag, err := tx.Exec(ctx, `UPDATE session_relay_selections SET failed_at=clock_timestamp()
				WHERE round_id=$1 AND relay_id=$2 AND failed_at IS NULL AND clock_timestamp()<$3::timestamptz`,
				round.ID, in.Relay, out.Deadline)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errSessionConflict
			}
			if _, err := tx.Exec(ctx, `DELETE FROM managed_session_routes
				WHERE session_id=$1 AND connection_id=$2 AND path='relay' AND relay_id=$3`, g.ID, round.Connection, in.Relay); err != nil {
				return err
			}
			if err := auditRelayDecision(ctx, tx, g, "session.relay_selection_failed", "failed", "connection_failed"); err != nil {
				return err
			}
			failed[in.Relay], current = true, nil
		}
	}
	if in.Action == "select" {
		// Serialize confirmation with node health, availability and capacity updates.
		if _, err := tx.Exec(ctx, `SELECT relay_nodes.id FROM relay_nodes
			JOIN session_relay_candidates c ON c.relay_id=relay_nodes.id
			WHERE c.round_id=$1 ORDER BY relay_nodes.id FOR SHARE OF relay_nodes`, round.ID); err != nil {
			return err
		}
	}
	now, err := sessionClock(ctx, tx)
	if err != nil {
		return err
	}
	if !now.Before(out.Deadline) {
		return errSessionConflict
	}
	if err := a.loadRelayRoundCandidates(ctx, tx, &round, now, g, role, nil); err != nil {
		return err
	}
	if current != nil {
		if in.Action == "select" && in.Relay != current.Relay {
			return errSessionConflict
		}
		out.State, out.Selected = "confirmed", current
		if !slices.ContainsFunc(round.Candidates, func(node relayRoundCandidate) bool { return node.ID == current.Relay && node.Eligible }) {
			if in.Action == "select" {
				return errSessionConflict
			}
			out.State = "unavailable"
		}
		return nil
	}
	if role == relayTarget {
		out.State = "waiting_for_selection"
		return nil
	}
	policy, err := readRelayNetworkPolicy(ctx, tx, g.Organization)
	if err != nil {
		return err
	}
	if policy.Revision != round.NetworkRevision {
		return errSessionConflict
	}
	options := orderRelayOptions(round, policy, failed)
	if in.Action != "select" {
		out.State, out.Options = "ready", options
		if len(options) == 0 {
			out.State = "exhausted"
		}
		return nil
	}
	if len(options) == 0 || options[0].Relay != in.Relay {
		return errSessionConflict
	}
	winner := options[0]
	tag, err := tx.Exec(ctx, `INSERT INTO session_relay_selections
		(round_id,relay_id,attempt,reason,combined_rtt_us,selected_at)
		SELECT $1,$2,$3,$4,$5,clock_timestamp() WHERE clock_timestamp()<$6::timestamptz`, round.ID, winner.Relay,
		attempt+1, winner.Reason, winner.CombinedRTT, out.Deadline)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errSessionConflict
	}
	out.State, out.Selected = "confirmed", &winner
	return auditRelayDecision(ctx, tx, g, "session.relay_selected", "allowed", winner.Reason)
}

func auditRelayDecision(ctx context.Context, tx pgx.Tx, g sessionGrant, action, result, reason string) error {
	var group string
	if err := tx.QueryRow(ctx, "SELECT group_id FROM devices WHERE id=$1 AND organization_id=$2", g.Target, g.Organization).Scan(&group); err != nil {
		return err
	}
	return auditSession(ctx, tx, g, group, action, result, reason)
}
