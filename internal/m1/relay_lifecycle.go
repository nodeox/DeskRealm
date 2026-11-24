package m1

import (
	"context"
	"errors"
	"net/http"
	"time"

	"deskrealm/internal/relayprotocol"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	errRelayInput            = errors.New("invalid_relay_change")
	errRelayConflict         = errors.New("relay_lifecycle_conflict")
	errRelayEndpointConflict = errors.New("relay_endpoint_conflict")
	errRelayMissing          = errors.New("relay_not_found")
)

type relayProbeHistory struct {
	Endpoint string    `json:"endpoint"`
	Version  int64     `json:"config_version"`
	Checked  time.Time `json:"checked_at"`
	Result   string    `json:"result"`
	TaskID   string    `json:"task_id,omitempty"`
}

type relaySession struct {
	ID         string     `json:"id"`
	State      string     `json:"state"`
	LeaseUntil *time.Time `json:"lease_until,omitempty"`
}

func (a *API) relayNodeDetail(w http.ResponseWriter, r *http.Request) {
	id, ctx := r.PathValue("id"), r.Context()
	if !userIDPattern.MatchString(id) {
		failure(w, "read relay", errRelayMissing)
		return
	}
	out := struct {
		relayNodeSummary
		Probes   []relayProbeHistory    `json:"probes"`
		Sessions []relaySession         `json:"sessions"`
		Upgrade  *relayprotocol.Upgrade `json:"upgrade,omitempty"`
	}{Probes: []relayProbeHistory{}, Sessions: []relaySession{}}
	err := a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		var err error
		out.relayNodeSummary, err = readRelaySummary(tx.QueryRow(ctx, `SELECT `+relaySummaryFields+
			` FROM relay_nodes WHERE id=$1 AND organization_id=$2`, id, who.Organization))
		if errors.Is(err, pgx.ErrNoRows) {
			return errRelayMissing
		}
		if err != nil {
			return err
		}
		upgrade, err := readRelayUpgrade(ctx, tx, id, "")
		if err == nil {
			out.Upgrade = &upgrade.Upgrade
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT endpoint,config_version,checked_at,result,COALESCE(task_id::text,'')
			FROM relay_probe_history WHERE node_id=$1 ORDER BY checked_at DESC,id DESC LIMIT 12`, id)
		if err != nil {
			return err
		}
		out.Probes, err = pgx.AppendRows(out.Probes, rows, pgx.RowToStructByPos[relayProbeHistory])
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT g.id,`+sessionActivitySelect+sessionActivityFrom+
			` WHERE g.organization_id=$1 AND r.path='relay' AND r.relay_id=$2
			ORDER BY r.redeemed_at DESC,g.id LIMIT 12`, who.Organization, id)
		if err != nil {
			return err
		}
		out.Sessions, err = pgx.AppendRows(out.Sessions, rows, func(row pgx.CollectableRow) (relaySession, error) {
			var sid string
			var activity sessionActivity
			err := row.Scan(append([]any{&sid}, activity.destinations()...)...)
			return relaySession{sid, activity.State, activity.LeaseUntil}, err
		})
		return err
	})
	if err != nil {
		failure(w, "read relay", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type relayChange struct {
	Action   string `json:"action"`
	Revision int64  `json:"revision"`
	Endpoint string `json:"endpoint"`
}

type relayChangeNode struct {
	relayProbeTarget
	mode        string
	revision    int64
	healthy     bool
	upgradeHeld bool
}

func relayChangeTarget(ctx context.Context, tx pgx.Tx, id, org string, write bool) (relayChangeNode, error) {
	node := relayChangeNode{relayProbeTarget: relayProbeTarget{nodeID: id}}
	query := `SELECT endpoint,build,identity_key,config_version,capacity,mode,revision,COALESCE(` + relayHealthySQL +
		`,false),EXISTS(SELECT 1 FROM relay_upgrades WHERE node_id=relay_nodes.id AND state IN ('pending','claimed'))
		FROM relay_nodes WHERE id=$1 AND organization_id=$2`
	if write {
		query += " FOR UPDATE"
	}
	err := tx.QueryRow(ctx, query, id, org).Scan(&node.endpoint, &node.build, &node.key, &node.version,
		&node.capacity, &node.mode, &node.revision, &node.healthy, &node.upgradeHeld)
	if errors.Is(err, pgx.ErrNoRows) {
		return node, errRelayMissing
	}
	return node, err
}

func (a *API) changeRelayLifecycle(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	id, ctx := r.PathValue("id"), r.Context()
	if !userIDPattern.MatchString(id) {
		failure(w, "change relay", errRelayMissing)
		return
	}
	var change relayChange
	if !readJSON(w, r, &change) {
		return
	}
	switch change.Action {
	case "change_endpoint":
		change.Endpoint = normalizedRelayEndpoint(change.Endpoint)
		if change.Endpoint == "" {
			failure(w, "change relay", errRelayInput)
			return
		}
	case "drain", "disable", "resume":
		if change.Endpoint != "" {
			failure(w, "change relay", errRelayInput)
			return
		}
	default:
		failure(w, "change relay", errRelayInput)
		return
	}
	if change.Revision < 1 {
		failure(w, "change relay", errRelayInput)
		return
	}
	var current relayChangeNode
	err := a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		var err error
		current, err = relayChangeTarget(ctx, tx, id, who.Organization, false)
		if err != nil {
			return err
		}
		if len(current.key) == 0 || current.revision != change.Revision || current.upgradeHeld ||
			(change.Action == "drain" && current.mode != "enabled") ||
			(change.Action == "disable" && current.mode == "disabled") ||
			(change.Action == "resume" && current.mode == "enabled") || change.Endpoint == current.endpoint {
			return errRelayConflict
		}
		if change.Action == "change_endpoint" {
			var taken bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM relay_nodes WHERE endpoint=$1)`, change.Endpoint).
				Scan(&taken); err != nil {
				return err
			}
			if taken {
				return errRelayEndpointConflict
			}
		}
		return nil
	})
	if err != nil {
		failure(w, "change relay", err)
		return
	}
	target := current.relayProbeTarget
	if change.Endpoint != "" {
		target.endpoint = change.Endpoint
	}
	var probe string
	var active int
	if change.Action == "resume" || change.Action == "change_endpoint" {
		active, probe = observeRelay(ctx, target)
	}
	out := struct {
		TaskID string `json:"task_id"`
		Result string `json:"result_code"`
	}{}
	err = a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		node, err := relayChangeTarget(ctx, tx, id, who.Organization, true)
		if err != nil {
			return err
		}
		if node.revision != current.revision || node.version != current.version || node.upgradeHeld {
			return errRelayConflict
		}
		result := map[string]string{"drain": "relay_draining", "disable": "relay_disabled",
			"resume": "relay_resumed", "change_endpoint": "relay_endpoint_changed"}[change.Action]
		mode, endpoint, version := node.mode, node.endpoint, node.version
		success := probe == "" || probe == "verified"
		switch {
		case !success:
			result = "relay_probe_failed"
		case change.Action == "drain":
			mode = "draining"
		case change.Action == "disable":
			mode = "disabled"
		case change.Action == "resume":
			if !node.healthy {
				result, success = "relay_health_required", false
			} else {
				mode = "enabled"
			}
		case change.Action == "change_endpoint":
			endpoint, version = target.endpoint, version+1
		}
		out.TaskID, err = finishRelayChangeTask(ctx, tx, who, id, change.Action, result, success)
		if err != nil {
			return err
		}
		out.Result = result
		verified := probe == "verified" && success
		_, err = tx.Exec(ctx, `UPDATE relay_nodes SET mode=$2,endpoint=$3,config_version=$4,
			revision=revision+1,last_operation_task_id=$5,
			probe_succeeded_at=CASE WHEN $6 THEN statement_timestamp() ELSE probe_succeeded_at END,
			probe_checked_at=CASE WHEN $6 THEN statement_timestamp() ELSE probe_checked_at END,
			probe_config_version=CASE WHEN $6 THEN $4 ELSE probe_config_version END,
			probe_active_sessions=CASE WHEN $6 THEN $7 ELSE probe_active_sessions END,
			probe_failures=CASE WHEN $6 THEN 0 ELSE probe_failures END WHERE id=$1`,
			id, mode, endpoint, version, out.TaskID, verified, active)
		var constraint *pgconn.PgError
		if errors.As(err, &constraint) && constraint.ConstraintName == "relay_nodes_endpoint_key" {
			return errRelayEndpointConflict
		}
		if err != nil || probe == "" {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO relay_probe_history(node_id,endpoint,config_version,checked_at,result,task_id)
			VALUES($1,$2,$3,statement_timestamp(),$4,$5)`, id, target.endpoint, version, probe, out.TaskID)
		return err
	})
	if err != nil {
		failure(w, "change relay", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func finishRelayChangeTask(ctx context.Context, tx pgx.Tx, who actor, node, action, result string, success bool) (string, error) {
	var task string
	if err := tx.QueryRow(ctx, `INSERT INTO tasks(organization_id,kind,owner_id)
		VALUES($1,'relay_lifecycle',$2) RETURNING id`, who.Organization, who.ID).Scan(&task); err != nil {
		return "", err
	}
	state := "failed"
	if success {
		state = "succeeded"
	}
	_, err := tx.Exec(ctx, `WITH target AS (
		INSERT INTO task_targets(task_id,object_key,state,result_code) VALUES($1,$2,$3,$4) RETURNING id,version
	)
	INSERT INTO task_attempts(id,target_id,requested_by,sequence,result_code,final_version,finished_at)
	SELECT gen_random_uuid(),id,$5,0,$4,version,statement_timestamp() FROM target`, task, node, state, result, who.ID)
	if err != nil {
		return "", err
	}
	_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: who.Organization, ActorID: who.ID, TaskID: task,
		ResourceID: node, Action: "relay." + action, Result: state, Reason: result, Source: "relay_lifecycle", SourceKey: task})
	return task, err
}
