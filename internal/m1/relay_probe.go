package m1

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"deskrealm/internal/relayprotocol"
	"github.com/jackc/pgx/v5"
)

const relayProbeInterval = 5 * time.Second
const relayHealthySQL = `relay_nodes.health_config_version=relay_nodes.config_version
	AND relay_nodes.health_at>statement_timestamp()-interval '15 seconds' AND relay_nodes.protocol_ready`
const relayStateSQL = `CASE
	WHEN relay_nodes.registered_at IS NULL THEN 'pending_enrollment'
	WHEN relay_nodes.mode='disabled' THEN 'disabled'
	WHEN relay_nodes.mode='draining' THEN 'draining'
	WHEN NOT COALESCE(` + relayHealthySQL + `,false) THEN 'awaiting_health'
	WHEN relay_nodes.probe_failures>=3 THEN 'probe_unreachable'
	WHEN relay_nodes.probe_config_version IS DISTINCT FROM relay_nodes.config_version
		OR relay_nodes.probe_succeeded_at IS NULL OR relay_nodes.probe_succeeded_at<=statement_timestamp()-interval '15 seconds' THEN 'awaiting_probe'
	WHEN greatest(relay_nodes.active_sessions,relay_nodes.probe_active_sessions)>=relay_nodes.capacity THEN 'at_capacity'
	ELSE 'available' END`

type relayProbeTarget struct {
	nodeID, endpoint, build string
	key                     []byte
	version                 int64
	capacity                int
	checked                 time.Time
}

func observeRelay(ctx context.Context, target relayProbeTarget) (int, string) {
	active, err := relayprotocol.Probe(ctx, target.endpoint, target.key, target.build)
	switch {
	case errors.Is(err, relayprotocol.ErrProbeProtocol):
		return 0, "protocol_mismatch"
	case err != nil:
		return 0, "unreachable"
	case active > target.capacity:
		return 0, "invalid_capacity"
	default:
		return active, "verified"
	}
}

// RunRelayProbes checks independently from the center's own network. Postgres
// coordinates claims across center processes, and network I/O holds no locks.
func (s Store) RunRelayProbes(ctx context.Context) {
	tick := time.NewTicker(relayProbeInterval)
	defer tick.Stop()
	for ctx.Err() == nil {
		step, cancel := context.WithTimeout(ctx, relayProbeInterval)
		if err := s.probeDueRelays(step); err != nil && ctx.Err() == nil {
			slog.Error("DeskRealm relay probe sweep failed")
		}
		cancel()
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
}

func (s Store) probeDueRelays(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `WITH due AS (
		SELECT id FROM relay_nodes WHERE registered_at IS NOT NULL AND mode<>'disabled' AND `+relayHealthySQL+`
			AND (probe_checked_at IS NULL OR probe_checked_at<=statement_timestamp()-interval '4 seconds')
		ORDER BY probe_checked_at NULLS FIRST,id LIMIT 32 FOR UPDATE SKIP LOCKED
	)
	UPDATE relay_nodes SET probe_checked_at=statement_timestamp() FROM due WHERE relay_nodes.id=due.id
	RETURNING relay_nodes.id,endpoint,build,identity_key,config_version,capacity,probe_checked_at`)
	if err != nil {
		return err
	}
	targets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (relayProbeTarget, error) {
		var target relayProbeTarget
		err := row.Scan(&target.nodeID, &target.endpoint, &target.build, &target.key, &target.version,
			&target.capacity, &target.checked)
		return target, err
	})
	if err != nil {
		return err
	}
	var tasks sync.WaitGroup
	results := make(chan error, len(targets))
	for _, target := range targets {
		tasks.Go(func() {
			active, result := observeRelay(ctx, target)
			if ctx.Err() != nil {
				return
			}
			results <- s.recordRelayProbe(ctx, target, active, result)
		})
	}
	tasks.Wait()
	close(results)
	for err := range results {
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s Store) recordRelayProbe(ctx context.Context, target relayProbeTarget, active int, result string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var task, org, state string
		var previous, failures int
		success := result == "verified"
		err := tx.QueryRow(ctx, `WITH before AS (
			SELECT id,probe_failures AS previous FROM relay_nodes
			WHERE id=$1 AND config_version=$2 AND probe_checked_at=$3 AND mode<>'disabled' FOR UPDATE
		)
		UPDATE relay_nodes SET
			probe_succeeded_at=CASE WHEN $4 THEN statement_timestamp() ELSE probe_succeeded_at END,
			probe_config_version=CASE WHEN $4 THEN config_version ELSE probe_config_version END,
			probe_active_sessions=CASE WHEN $4 THEN $5 ELSE probe_active_sessions END,
			probe_failures=CASE WHEN $4 THEN 0 ELSE least(probe_failures+1,3) END
			FROM before WHERE relay_nodes.id=before.id
			RETURNING task_id,organization_id,`+relayStateSQL+`,before.previous,probe_failures`,
			target.nodeID, target.version, target.checked, success, active).Scan(&task, &org, &state, &previous, &failures)
		current := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var history int64
		err = tx.QueryRow(ctx, `INSERT INTO relay_probe_history(node_id,endpoint,config_version,checked_at,result)
			VALUES($1,$2,$3,$4,$5) RETURNING id`, target.nodeID, target.endpoint, target.version, target.checked, result).Scan(&history)
		if err != nil || !current {
			return err
		}
		if (previous == 3) != (failures == 3) {
			action := "relay.probe_recovered"
			if failures == 3 {
				action = "relay.probe_lost"
			}
			_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: org, ResourceID: target.nodeID, TaskID: task,
				Action: action, Result: result, Source: "relay_probe", SourceKey: fmt.Sprintf("history:%d", history)})
		}
		if err != nil || !success || state != "available" {
			return err
		}
		tag, err := tx.Exec(ctx, `WITH finished AS (
			UPDATE task_targets SET state='succeeded',result_code='relay_available',version=version+1
			WHERE task_id=$1 AND object_key=$2 AND state='waiting' AND result_code='relay_pending_probe'
			RETURNING id,version
		)
		INSERT INTO task_attempts(id,target_id,requested_by,sequence,result_code,final_version,finished_at)
		SELECT gen_random_uuid(),finished.id,tasks.owner_id,0,'relay_available',finished.version,statement_timestamp()
		FROM finished CROSS JOIN tasks WHERE tasks.id=$1`, task, target.nodeID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: org, TaskID: task, ResourceID: target.nodeID,
			Action: "relay.available", Result: "success", Source: "relay_probe", SourceKey: target.nodeID})
		return err
	})
}
