package m1

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"deskrealm/internal/relayprotocol"
	"github.com/jackc/pgx/v5"
)

const relayProbeInterval = 5 * time.Second
const relayHealthySQL = `health_at>statement_timestamp()-interval '15 seconds' AND protocol_ready`
const relayStateSQL = `CASE
	WHEN registered_at IS NULL THEN 'pending_enrollment'
	WHEN NOT COALESCE(` + relayHealthySQL + `,false) THEN 'awaiting_health'
	WHEN probe_failures>=3 THEN 'probe_unreachable'
	WHEN probe_config_version IS DISTINCT FROM config_version
		OR probe_succeeded_at IS NULL OR probe_succeeded_at<=statement_timestamp()-interval '15 seconds' THEN 'awaiting_probe'
	WHEN greatest(active_sessions,probe_active_sessions)>=capacity THEN 'at_capacity'
	ELSE 'available' END`

type relayProbeTarget struct {
	nodeID, endpoint, build string
	key                     []byte
	version                 int64
	capacity                int
	checked                 time.Time
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
		SELECT id FROM relay_nodes WHERE registered_at IS NOT NULL AND `+relayHealthySQL+`
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
			active, err := relayprotocol.Probe(ctx, target.endpoint, target.key, target.build)
			if ctx.Err() != nil {
				return
			}
			results <- s.recordRelayProbe(ctx, target, active, err == nil && active <= target.capacity)
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

func (s Store) recordRelayProbe(ctx context.Context, target relayProbeTarget, active int, success bool) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var task, org, state string
		err := tx.QueryRow(ctx, `UPDATE relay_nodes SET
			probe_succeeded_at=CASE WHEN $4 THEN statement_timestamp() ELSE probe_succeeded_at END,
			probe_config_version=CASE WHEN $4 THEN config_version ELSE probe_config_version END,
			probe_active_sessions=CASE WHEN $4 THEN $5 ELSE probe_active_sessions END,
			probe_failures=CASE WHEN $4 THEN 0 ELSE least(probe_failures+1,3) END
			WHERE id=$1 AND config_version=$2 AND probe_checked_at=$3
			RETURNING task_id,organization_id,`+relayStateSQL, target.nodeID, target.version, target.checked, success, active).
			Scan(&task, &org, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
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
