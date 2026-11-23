package m1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5"
)

// DeliveryExecutor is supplied by the center for an approved delivery backend.
// The backend must record each outcome durably and deduplicate execution by
// operation ID and predecessor receipt, including across executor restarts.
type DeliveryExecutor interface {
	Lookup(context.Context, DeliveryOperation) (*DeliveryReceipt, error)
	Execute(context.Context, DeliveryOperation, string) (DeliveryReceipt, error)
}

type DeliveryOperation struct {
	ID           string `json:"id"`
	Organization string `json:"organization_id"`
	Task         string `json:"task_id"`
	Target       string `json:"target_id"`
	Object       string `json:"object"`
}

type DeliveryReceipt struct {
	ID        string            `json:"id"`
	Operation DeliveryOperation `json:"operation"`
	Succeeded bool              `json:"succeeded"`
}

var deliveryObject = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var errDeliveryReceipt = errors.New("external delivery receipt is invalid or already applied")

func (claim taskClaim) deliveryOperation() DeliveryOperation {
	return DeliveryOperation{claim.Operation, claim.Organization, claim.Task, claim.Target, claim.ObjectKey}
}

func (a *API) createDeliveryTask(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	var input struct {
		RequestKey string   `json:"request_key"`
		Objects    []string `json:"objects"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	slices.Sort(input.Objects)
	if !auditSourceKey.MatchString(input.RequestKey) || len(input.Objects) == 0 || len(input.Objects) > 50 {
		failure(w, "create delivery task", errTaskInvalid)
		return
	}
	for i, object := range input.Objects {
		if !deliveryObject.MatchString(object) || (i > 0 && object == input.Objects[i-1]) {
			failure(w, "create delivery task", errTaskInvalid)
			return
		}
	}
	ctx := r.Context()
	var taskID string
	objects, _ := json.Marshal(input.Objects)
	hash := digest(string(objects))
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		if a.store.Delivery == nil {
			return errTaskMissing
		}
		var previous []byte
		err := tx.QueryRow(ctx, `SELECT task_id,payload_digest FROM delivery_task_requests
			WHERE organization_id=$1 AND request_key=$2`, who.Organization, input.RequestKey).Scan(&taskID, &previous)
		if err == nil {
			if !bytes.Equal(hash, previous) {
				return errTaskConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO tasks(organization_id,kind,owner_id)
			VALUES($1,'delivery',$2) RETURNING id`, who.Organization, who.ID).Scan(&taskID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_task_requests(task_id,organization_id,request_key,payload_digest)
			VALUES($1,$2,$3,$4)`, taskID, who.Organization, input.RequestKey, hash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_targets(task_id,operation_id,object_key,state,requested_by)
			SELECT $1,gen_random_uuid(),object,'queued',$2 FROM unnest($3::text[]) AS object`,
			taskID, who.ID, input.Objects); err != nil {
			return err
		}
		_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: who.Organization, ActorID: who.ID, TaskID: taskID,
			ResourceID: taskID, Action: "task.delivery_created", Result: "accepted"})
		return err
	})
	if err != nil {
		failure(w, "create delivery task", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": taskID})
}

const deliveryClaimFilter = `t.kind='delivery' AND t.id=$1 AND t.organization_id=$2
	AND tt.id=$3 AND tt.operation_id=$4 AND tt.object_key=$5 AND a.id=$6 AND a.requested_by=$7`

func (s Store) runDelivery(ctx context.Context, claim taskClaim) error {
	op := claim.deliveryOperation()
	var last string
	var current bool
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(
		(SELECT r.receipt_id::text FROM delivery_task_receipts r JOIN task_attempts prev ON prev.id=r.attempt_id
		WHERE r.operation_id=tt.operation_id ORDER BY prev.sequence DESC LIMIT 1),''),
		COALESCE(tt.lease_token=a.id AND tt.version=$8 AND tt.lease_until>clock_timestamp(),false)
		FROM task_targets tt JOIN tasks t ON t.id=tt.task_id JOIN task_attempts a ON a.target_id=tt.id
		WHERE `+deliveryClaimFilter, op.Task, op.Organization, op.Target, op.ID, op.Object, claim.Token, claim.Requester, claim.Version).Scan(
		&last, &current)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !current) {
		return errTaskLease
	}
	if err != nil {
		return err
	}
	receipt, err := s.Delivery.Lookup(ctx, op)
	if err != nil {
		return err // Keep the lease. A later claim reconciles instead of assuming failure.
	}
	if receipt != nil {
		if receipt.Operation != op || !userIDPattern.MatchString(receipt.ID) {
			return errDeliveryReceipt
		}
		if receipt.ID != last {
			return s.commitDelivery(ctx, claim, receipt, "")
		}
		if receipt.Succeeded {
			return errDeliveryReceipt
		}
	} else if last != "" {
		return errDeliveryReceipt // Lost external state is not evidence that repeating the effect is safe.
	}
	if claim.Sequence > maxTaskAttempts {
		return s.commitDelivery(ctx, claim, nil, "lease_expired")
	}
	var permitted bool
	err = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users u WHERE u.id=$7 AND u.organization_id=$2
		AND u.status='active' AND `+adminBinding+`)
		FROM task_targets tt JOIN tasks t ON t.id=tt.task_id JOIN task_attempts a ON a.target_id=tt.id
		WHERE `+deliveryClaimFilter+` AND tt.lease_token=a.id AND tt.version=$8 AND tt.lease_until>clock_timestamp()`,
		op.Task, op.Organization, op.Target, op.ID, op.Object, claim.Token, claim.Requester, claim.Version).Scan(&permitted)
	if errors.Is(err, pgx.ErrNoRows) {
		return errTaskLease
	}
	if err != nil {
		return err
	}
	if !permitted {
		return s.commitDelivery(ctx, claim, nil, "authorization_changed")
	}
	result, err := s.Delivery.Execute(ctx, op, last)
	if err != nil {
		return err
	}
	return s.commitDelivery(ctx, claim, &result, "")
}

func (s Store) commitDelivery(ctx context.Context, claim taskClaim, receipt *DeliveryReceipt, localCode string) error {
	op := claim.deliveryOperation()
	state, code := "failed", localCode
	if receipt != nil {
		if receipt.Operation != op || !userIDPattern.MatchString(receipt.ID) {
			return errDeliveryReceipt
		}
		code = "delivery_failed"
		if receipt.Succeeded {
			state, code = "succeeded", "delivery_succeeded"
		}
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var version, currentVersion int64
		var current bool
		var priorCode, priorReceipt string
		var sequence int
		err := tx.QueryRow(ctx, `SELECT COALESCE(a.final_version,0),tt.version,COALESCE(a.result_code,''),a.sequence,
			COALESCE(tt.lease_token=a.id AND tt.version=$8 AND tt.lease_until>clock_timestamp(),false),
			COALESCE(r.receipt_id::text,'')
			FROM task_targets tt JOIN tasks t ON t.id=tt.task_id JOIN task_attempts a ON a.target_id=tt.id
			LEFT JOIN delivery_task_receipts r ON r.attempt_id=a.id WHERE `+deliveryClaimFilter+` FOR UPDATE OF tt`,
			op.Task, op.Organization, op.Target, op.ID, op.Object, claim.Token, claim.Requester, claim.Version).Scan(
			&version, &currentVersion, &priorCode, &sequence, &current, &priorReceipt)
		if errors.Is(err, pgx.ErrNoRows) {
			return errTaskLease
		}
		if err != nil {
			return err
		}
		if version == claim.Version+1 && currentVersion == version && priorCode == code &&
			((receipt != nil && priorReceipt == receipt.ID) || (receipt == nil && priorReceipt == "")) {
			return nil
		}
		if !current {
			return errTaskLease
		}
		if receipt != nil {
			tag, err := tx.Exec(ctx, `INSERT INTO delivery_task_receipts(receipt_id,attempt_id,operation_id,succeeded)
				VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, receipt.ID, claim.Token, op.ID, receipt.Succeeded)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errDeliveryReceipt
			}
		}
		return finalizeTaskAttempt(ctx, tx, claim, state, code, code == "delivery_failed" && sequence < maxTaskAttempts)
	})
}
