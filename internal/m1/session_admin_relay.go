package m1

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type sessionRelayHistory struct {
	Rounds []sessionRelayAdminRound `json:"rounds"`
	Older  bool                     `json:"older_rounds,omitempty"`
}

type sessionRelayAdminRound struct {
	Connection string                       `json:"connection_id"`
	Version    int64                        `json:"version"`
	Opened     *time.Time                   `json:"opened_at,omitempty"`
	Deadline   *time.Time                   `json:"deadline,omitempty"`
	Superseded *time.Time                   `json:"superseded_at,omitempty"`
	Candidates []sessionRelayAdminCandidate `json:"candidates"`
}

type sessionRelayAdminCandidate struct {
	Relay       string            `json:"relay_id"`
	Name        string            `json:"name"`
	Site        string            `json:"site"`
	Region      string            `json:"region"`
	Controller  relayProbeSummary `json:"controller"`
	Target      relayProbeSummary `json:"target"`
	Attempt     *int              `json:"attempt,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	CombinedRTT *uint32           `json:"combined_rtt_us,omitempty"`
	SelectedAt  *time.Time        `json:"selected_at,omitempty"`
	FailedAt    *time.Time        `json:"failed_at,omitempty"`
	Redeemed    bool              `json:"redeemed,omitempty"`
}

func readSessionRelayHistory(ctx context.Context, tx pgx.Tx, session string) (*sessionRelayHistory, error) {
	const limit = 10
	// Include the redeemed connection even if later version history exists.
	rows, err := tx.Query(ctx, `WITH recent AS (
		SELECT r.id,r.connection_id,r.version,r.opened_at,r.deadline,r.superseded_at,d.relay_id AS actual
		FROM session_relay_rounds r
		LEFT JOIN managed_session_redemptions d ON d.session_id=r.session_id AND d.connection_id=r.connection_id AND d.path='relay'
		WHERE r.session_id=$1 ORDER BY (d.relay_id IS NOT NULL) DESC,r.version DESC LIMIT $2
	) SELECT r.connection_id,r.version,r.opened_at,r.deadline,r.superseded_at,
		(r.superseded_at IS NOT NULL OR COALESCE(r.deadline<=statement_timestamp(),false)),
		COALESCE(c.relay_id::text,''),COALESCE(n.name,''),COALESCE(c.site,''),COALESCE(c.region,''),
		l.samples,t.samples,s.attempt,COALESCE(s.reason,''),s.combined_rtt_us,s.selected_at,s.failed_at,
		COALESCE(c.relay_id::text=r.actual,false)
		FROM recent r LEFT JOIN session_relay_candidates c ON c.round_id=r.id
		LEFT JOIN relay_nodes n ON n.id=c.relay_id
		LEFT JOIN session_relay_reports l ON l.round_id=r.id AND l.relay_id=c.relay_id AND l.role=1
		LEFT JOIN session_relay_reports t ON t.round_id=r.id AND t.relay_id=c.relay_id AND t.role=2
		LEFT JOIN session_relay_selections s ON s.round_id=r.id AND s.relay_id=c.relay_id
		ORDER BY (r.actual IS NOT NULL) DESC,r.version DESC,s.attempt NULLS LAST,c.relay_id`, session, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	history := &sessionRelayHistory{}
	for rows.Next() {
		var round sessionRelayAdminRound
		var node sessionRelayAdminCandidate
		var controller, target []relayProbeSample
		var closed bool
		err := rows.Scan(&round.Connection, &round.Version, &round.Opened, &round.Deadline, &round.Superseded, &closed,
			&node.Relay, &node.Name, &node.Site, &node.Region, &controller, &target, &node.Attempt, &node.Reason,
			&node.CombinedRTT, &node.SelectedAt, &node.FailedAt, &node.Redeemed)
		if err != nil {
			return nil, err
		}
		count := len(history.Rounds)
		if count == 0 || history.Rounds[count-1].Version != round.Version {
			if count == limit {
				history.Older = true
				break
			}
			round.Candidates = []sessionRelayAdminCandidate{}
			history.Rounds = append(history.Rounds, round)
		}
		if node.Relay != "" {
			node.Controller, node.Target = summarizeRelaySamples(controller, closed), summarizeRelaySamples(target, closed)
			current := &history.Rounds[len(history.Rounds)-1]
			current.Candidates = append(current.Candidates, node)
		}
	}
	if len(history.Rounds) == 0 {
		return nil, rows.Err()
	}
	return history, rows.Err()
}
