package m1

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"deskrealm/internal/relayprotocol"
	"github.com/jackc/pgx/v5"
)

var relayUpgradeDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

// The relay needs time to expire outstanding pairing tickets after the center
// stops issuing them. Leave margin for the deployment's clock skew.
const relayUpgradeGrace = relaySelectionWindow + 5*time.Second

type relayUpgradeRecord struct {
	relayprotocol.Upgrade
	claimHealthy bool
}

func readRelayUpgrade(ctx context.Context, tx pgx.Tx, node, task string) (relayUpgradeRecord, error) {
	var u relayUpgradeRecord
	err := tx.QueryRow(ctx, `SELECT relay_upgrades.task_id,candidate_sha,COALESCE(original_sha,''),relay_upgrades.state,
		ready_at<=clock_timestamp(),COALESCE(health_at>=claimed_at AND `+relayHealthySQL+`,false)
		FROM relay_upgrades JOIN relay_nodes ON relay_nodes.id=node_id
		WHERE node_id=$1 AND (NULLIF($2,'')::uuid IS NULL OR relay_upgrades.task_id=NULLIF($2,'')::uuid)
		ORDER BY ready_at DESC,relay_upgrades.task_id DESC LIMIT 1`, node, task).
		Scan(&u.TaskID, &u.CandidateSHA, &u.OriginalSHA, &u.State, &u.Ready, &u.claimHealthy)
	return u, err
}

func (a *API) adminRelayUpgrade(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	var in struct {
		Revision int64  `json:"revision"`
		Action   string `json:"action"`
		SHA256   string `json:"sha256"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	nodeID, ctx := r.PathValue("id"), r.Context()
	if !userIDPattern.MatchString(nodeID) || in.Revision < 1 ||
		!(in.Action == "start" && relayUpgradeDigest.MatchString(in.SHA256) || in.Action == "cancel" && in.SHA256 == "") {
		failure(w, "manage relay upgrade", errRelayInput)
		return
	}
	var out relayUpgradeRecord
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		node, err := relayChangeTarget(ctx, tx, nodeID, who.Organization, true)
		if err != nil {
			return err
		}
		if len(node.key) == 0 || node.revision != in.Revision {
			return errRelayConflict
		}
		if in.Action == "cancel" {
			u, err := readRelayUpgrade(ctx, tx, nodeID, "")
			if errors.Is(err, pgx.ErrNoRows) || err == nil && u.State != "pending" {
				return errRelayConflict
			}
			if err != nil {
				return err
			}
			if err := finishRelayUpgrade(ctx, tx, nodeID, u.TaskID, "canceled", who.ID); err != nil {
				return err
			}
			out, err = readRelayUpgrade(ctx, tx, nodeID, u.TaskID)
			return err
		}
		if node.mode == "disabled" || node.upgradeHeld {
			return errRelayConflict
		}
		var task string
		if err := tx.QueryRow(ctx, `INSERT INTO tasks(organization_id,kind,owner_id)
			VALUES ($1,'relay_lifecycle',$2) RETURNING id`, who.Organization, who.ID).Scan(&task); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_targets(task_id,object_key,state,result_code)
			VALUES($1,$2,'waiting','relay_upgrade_pending')`, task, nodeID); err != nil {
			return err
		}
		// Take the row out of selection and invalidate tickets in the same transaction as the hold.
		if _, err := tx.Exec(ctx, `UPDATE relay_nodes SET mode='draining',revision=revision+1,last_operation_task_id=$2 WHERE id=$1`,
			nodeID, task); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO relay_upgrades(task_id,node_id,candidate_sha,ready_at)
			VALUES($1,$2,$3,clock_timestamp()+$4::int*interval '1 second')`, task, nodeID, in.SHA256, int(relayUpgradeGrace.Seconds())); err != nil {
			return err
		}
		_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: who.Organization, ActorID: who.ID, TaskID: task,
			ResourceID: nodeID, Action: "relay.upgrade_start", Result: "pending", Reason: "relay_upgrade_pending",
			Source: "relay_upgrade", SourceKey: task + ":start"})
		if err != nil {
			return err
		}
		out, err = readRelayUpgrade(ctx, tx, nodeID, task)
		return err
	})
	if err != nil {
		failure(w, "manage relay upgrade", err)
		return
	}
	writeJSON(w, http.StatusOK, out.Upgrade)
}

func (a *API) relayUpgrade(w http.ResponseWriter, r *http.Request) {
	var in struct {
		relayprotocol.UpgradeRequest
		Signature string `json:"signature"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !userIDPattern.MatchString(in.NodeID) || !userIDPattern.MatchString(in.TaskID) || !validToken(in.Nonce) ||
		in.ConfigVersion < 1 || !relayUpgradeDigest.MatchString(in.SHA256) ||
		(in.Action != "claim" && in.Action != "succeeded" && in.Action != "rolled_back" && in.Action != "status") {
		failure(w, "relay upgrade", errRelayInput)
		return
	}
	ctx := r.Context()
	load := func(tx pgx.Tx, lock bool) (relayProbeTarget, relayUpgradeRecord, error) {
		var now time.Time
		var mode string
		node := relayProbeTarget{nodeID: in.NodeID}
		query := `SELECT endpoint,build,identity_key,config_version,capacity,mode,clock_timestamp()
			FROM relay_nodes WHERE id=$1 AND registered_at IS NOT NULL`
		if lock {
			query += " FOR UPDATE"
		}
		err := tx.QueryRow(ctx, query, in.NodeID).Scan(&node.endpoint, &node.build, &node.key, &node.version, &node.capacity, &mode, &now)
		if errors.Is(err, pgx.ErrNoRows) {
			err = errManagedIdentity
		}
		if err != nil {
			return node, relayUpgradeRecord{}, err
		}
		window := relayTimestampWindow.Milliseconds()
		if in.Action == "claim" {
			window = (2 * time.Second).Milliseconds()
		}
		if in.ObservedMS < now.UnixMilli()-window || in.ObservedMS > now.UnixMilli()+window ||
			!relayprotocol.Verify(node.key, in.UpgradeRequest.Frame(), in.Signature) {
			return node, relayUpgradeRecord{}, errManagedIdentity
		}
		u, err := readRelayUpgrade(ctx, tx, in.NodeID, in.TaskID)
		if errors.Is(err, pgx.ErrNoRows) || node.version != in.ConfigVersion ||
			(err == nil && (u.State == "pending" || u.State == "claimed") && mode != "draining") {
			err = errRelayConflict
		}
		return node, u, err
	}
	desired := func(u relayUpgradeRecord) bool {
		switch in.Action {
		case "claim", "rolled_back":
			return u.OriginalSHA == in.SHA256
		default:
			return u.CandidateSHA == in.SHA256
		}
	}
	var target relayProbeTarget
	var snapshot relayUpgradeRecord
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		var err error
		target, snapshot, err = load(tx, false)
		return err
	})
	if err != nil {
		failure(w, "relay upgrade", err)
		return
	}
	if in.Action == "status" && desired(snapshot) ||
		in.Action == "claim" && snapshot.State == "claimed" && desired(snapshot) ||
		in.Action == snapshot.State && desired(snapshot) {
		writeJSON(w, http.StatusOK, snapshot.Upgrade)
		return
	}
	if !(in.Action == "claim" && snapshot.State == "pending" && snapshot.Ready && in.SHA256 != snapshot.CandidateSHA ||
		(in.Action == "succeeded" || in.Action == "rolled_back") && snapshot.State == "claimed" && desired(snapshot) && snapshot.claimHealthy) {
		failure(w, "relay upgrade", errRelayConflict)
		return
	}
	if active, status := observeRelay(ctx, target); status != "verified" || active != 0 {
		failure(w, "relay upgrade", errRelayConflict)
		return
	}
	err = pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		current, u, err := load(tx, true)
		if err != nil {
			return err
		}
		if current.endpoint != target.endpoint || current.build != target.build || u.State != snapshot.State || u.OriginalSHA != snapshot.OriginalSHA {
			return errRelayConflict
		}
		if in.Action == "claim" {
			var leased bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM managed_session_redemptions r
				LEFT JOIN managed_session_stops e ON e.session_id=r.session_id
				WHERE r.path='relay' AND r.relay_id=$1 AND e.session_id IS NULL
				AND `+currentSessionDeadline+`>statement_timestamp())`, in.NodeID).Scan(&leased); err != nil {
				return err
			}
			if leased {
				return errRelayConflict
			}
			if _, err := tx.Exec(ctx, `UPDATE relay_upgrades SET state='claimed',original_sha=$2,claimed_at=clock_timestamp()
				WHERE task_id=$1`, in.TaskID, in.SHA256); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE task_targets SET result_code='relay_upgrade_claimed',version=version+1 WHERE task_id=$1`, in.TaskID)
			if err != nil {
				return err
			}
		} else {
			if !u.claimHealthy {
				return errRelayConflict
			}
			if err := finishRelayUpgrade(ctx, tx, in.NodeID, in.TaskID, in.Action, ""); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO relay_probe_history(node_id,endpoint,config_version,checked_at,result,task_id)
				VALUES($1,$2,$3,clock_timestamp(),'verified',$4)`, in.NodeID, current.endpoint, current.version, in.TaskID); err != nil {
				return err
			}
		}
		snapshot, err = readRelayUpgrade(ctx, tx, in.NodeID, in.TaskID)
		return err
	})
	if err != nil {
		failure(w, "relay upgrade", err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot.Upgrade)
}

func finishRelayUpgrade(ctx context.Context, tx pgx.Tx, node, task, state, actorID string) error {
	result := "relay_upgrade_" + state
	targetState := state
	if state == "rolled_back" {
		targetState = "failed"
	}
	if _, err := tx.Exec(ctx, `UPDATE relay_upgrades SET state=$2 WHERE task_id=$1`, task, state); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE relay_nodes SET revision=revision+1 WHERE id=$1`, node); err != nil {
		return err
	}
	var owner, org string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(NULLIF($2,'')::uuid,owner_id),organization_id FROM tasks WHERE id=$1`, task, actorID).
		Scan(&owner, &org); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `WITH updated AS (
		UPDATE task_targets SET state=$2,result_code=$3,version=version+1 WHERE task_id=$1 RETURNING id,version
	) INSERT INTO task_attempts(id,target_id,requested_by,sequence,result_code,final_version,finished_at)
	SELECT gen_random_uuid(),id,$4,0,$3,version,clock_timestamp() FROM updated`, task, targetState, result, owner); err != nil {
		return err
	}
	_, err := addAuditEvent(ctx, tx, auditEvent{OrganizationID: org, ActorID: owner, TaskID: task,
		ResourceID: node, Action: "relay.upgrade_" + state, Result: targetState, Reason: result,
		Source: "relay_upgrade", SourceKey: task + ":finish"})
	return err
}
