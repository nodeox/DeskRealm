package m1

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	errAuditInvalid  = errors.New("invalid_audit_request")
	errAuditMissing  = errors.New("audit_event_not_found")
	errAuditConflict = errors.New("audit_event_conflict")
	auditCode        = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,79}$`)
	auditSourceKey   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.-]{0,127}$`)
)

type auditEvent struct {
	OrganizationID string
	ActorID        string
	ResourceID     string
	DeviceGroupID  string
	SessionID      string
	TaskID         string
	Action         string
	Result         string
	Reason         string
	Source         string
	SourceKey      string
	ReceiptSource  string
	OccurredAt     time.Time
}

func addAuditEvent(ctx context.Context, tx pgx.Tx, e auditEvent) (string, error) {
	if e.Source == "" {
		e.Source = "control_plane"
	}
	if !auditCode.MatchString(e.Source) || !auditCode.MatchString(e.Action) || !auditCode.MatchString(e.Result) ||
		(e.SourceKey != "" && !auditSourceKey.MatchString(e.SourceKey)) ||
		(e.ReceiptSource != "" && (!auditCode.MatchString(e.ReceiptSource) || e.SourceKey == "")) {
		return "", errAuditInvalid
	}
	var occurred *time.Time
	if !e.OccurredAt.IsZero() {
		t := e.OccurredAt.UTC().Truncate(time.Microsecond)
		occurred = &t
	}
	var id string
	err := tx.QueryRow(ctx, `WITH inserted AS (
		INSERT INTO audit_events (organization_id,actor_id,resource_id,action,result,reason,source,source_key,
			session_id,task_id,device_group_id,user_group_ids,occurred_at,receipt_state,receipt_source)
		VALUES ($1,NULLIF($2,'')::uuid,NULLIF($3,'')::uuid,$4,$5,NULLIF($6,''),$7,NULLIF($8,''),
			NULLIF($9,'')::uuid,NULLIF($10,'')::uuid,COALESCE(NULLIF($11,'')::uuid,
			  (SELECT group_id FROM devices WHERE organization_id=$1 AND id=NULLIF($3,'')::uuid AND $4 LIKE 'device.%'),
			  (SELECT id FROM device_groups WHERE organization_id=$1 AND id=NULLIF($3,'')::uuid AND $4 LIKE 'device_group.%')),
			ARRAY(SELECT ug.id FROM user_groups ug WHERE ug.organization_id=$1 AND (
			    ($4 LIKE 'group.%' AND ug.id=NULLIF($3,'')::uuid) OR
			    ($4 LIKE 'user.%' AND EXISTS (SELECT 1 FROM user_group_members m
			      WHERE m.group_id=ug.id AND m.user_id=NULLIF($3,'')::uuid))
			) ORDER BY ug.id),
			COALESCE($12::timestamptz,clock_timestamp()),CASE WHEN $13='' THEN 'confirmed' ELSE 'pending' END,NULLIF($13,''))
		ON CONFLICT (organization_id,source,source_key) DO NOTHING RETURNING id
	)
	INSERT INTO outbox(event_id) SELECT id FROM inserted RETURNING event_id`, e.OrganizationID, e.ActorID, e.ResourceID,
		e.Action, e.Result, e.Reason, e.Source, e.SourceKey, e.SessionID, e.TaskID, e.DeviceGroupID, occurred, e.ReceiptSource).Scan(&id)
	if !errors.Is(err, pgx.ErrNoRows) {
		return id, err
	}
	var same bool
	err = tx.QueryRow(ctx, `SELECT id, actor_id IS NOT DISTINCT FROM NULLIF($4,'')::uuid
		AND resource_id IS NOT DISTINCT FROM NULLIF($5,'')::uuid AND action=$6 AND result=$7
		AND reason IS NOT DISTINCT FROM NULLIF($8,'') AND session_id IS NOT DISTINCT FROM NULLIF($9,'')::uuid
		AND task_id IS NOT DISTINCT FROM NULLIF($10,'')::uuid AND receipt_source IS NOT DISTINCT FROM NULLIF($11,'')
		AND ($12='' OR device_group_id IS NOT DISTINCT FROM NULLIF($12,'')::uuid) AND ($13::timestamptz IS NULL OR occurred_at=$13)
		FROM audit_events WHERE organization_id=$1 AND source=$2 AND source_key=$3`,
		e.OrganizationID, e.Source, e.SourceKey, e.ActorID, e.ResourceID, e.Action, e.Result, e.Reason, e.SessionID, e.TaskID,
		e.ReceiptSource, e.DeviceGroupID, occurred).Scan(&id, &same)
	if err == nil && !same {
		return "", errAuditConflict
	}
	return id, err
}

// receiveAuditReceipt is used by trusted execution code after authenticating its source.
// The receipt and related domain updates may share a transaction.
func receiveAuditReceipt(ctx context.Context, tx pgx.Tx, org, event, source, key, result string) error {
	if !userIDPattern.MatchString(org) || !userIDPattern.MatchString(event) || !auditCode.MatchString(source) ||
		!auditSourceKey.MatchString(key) || (result != "success" && result != "failure") {
		return errAuditInvalid
	}
	var currentSource, currentKey, currentResult string
	err := tx.QueryRow(ctx, `SELECT COALESCE(receipt_source,''),COALESCE(receipt_key,''),COALESCE(receipt_result,'')
		FROM audit_events WHERE organization_id=$1 AND id=$2 FOR UPDATE`, org, event).Scan(&currentSource, &currentKey, &currentResult)
	if errors.Is(err, pgx.ErrNoRows) {
		return errAuditMissing
	}
	if err != nil {
		return err
	}
	if currentSource != source || (currentKey != "" && (currentKey != key || currentResult != result)) {
		return errAuditConflict
	}
	if currentKey != "" {
		_, err = tx.Exec(ctx, `UPDATE audit_events SET duplicate_receipts=LEAST(duplicate_receipts::bigint+1,2147483647)
			WHERE id=$1`, event)
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE audit_events SET receipt_state='confirmed',receipt_key=$2,receipt_result=$3,
		acknowledged_at=clock_timestamp() WHERE id=$1`, event, key, result)
	if uniqueViolation(err) {
		return errAuditConflict
	}
	return err
}
