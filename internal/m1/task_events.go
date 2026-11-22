package m1

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// importAccount runs in a savepoint so a retryable object failure does not
// discard a successful sibling. A failed audit still rolls back that account.
func importAccount(ctx context.Context, tx pgx.Tx, org, actor, task, name string) (string, error) {
	var result string
	var objectFailure bool
	err := pgx.BeginFunc(ctx, tx, func(object pgx.Tx) error {
		var userID string
		err := object.QueryRow(ctx, `INSERT INTO users (organization_id,username,status)
			VALUES ($1,$2,'pending') ON CONFLICT (organization_id,username) DO NOTHING RETURNING id`, org, name).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			result = "exists"
			return nil
		}
		if err != nil {
			var dbErr *pgconn.PgError
			objectFailure = errors.As(err, &dbErr) && (strings.HasPrefix(dbErr.Code, "23") || dbErr.Code == "P0001")
			return err
		}
		result = "pending"
		_, err = addAuditEvent(ctx, object, auditEvent{OrganizationID: org, ActorID: actor,
			TaskID: task, ResourceID: userID, Action: "user.imported", Result: "success"})
		return err
	})
	// Transaction-wide failures should be retried by the caller. A database
	// constraint or object trigger failure can be recorded and retried alone.
	if objectFailure {
		return "import_failed", nil
	}
	return result, err
}

func createEnrollmentTask(ctx context.Context, tx pgx.Tx, org, issuer, device, rustdeskID string) (string, error) {
	var task string
	err := tx.QueryRow(ctx, `INSERT INTO tasks(organization_id,kind,owner_id,device_id)
		VALUES($1,'device_enrollment',$2,$3) RETURNING id`, org, issuer, device).Scan(&task)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO task_targets(task_id,object_key,state,result_code)
		VALUES($1,$2,'waiting','pending_approval')`, task, rustdeskID)
	return task, err
}

func finishEnrollmentTask(ctx context.Context, tx pgx.Tx, device, actor, result string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE device_id=$1`, device).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `WITH finished AS (
		UPDATE task_targets SET state=CASE WHEN $2='approved' THEN 'succeeded' ELSE 'failed' END,
		result_code=$2,version=version+1 WHERE task_id=$1 AND state='waiting' RETURNING id,version
	)
	INSERT INTO task_attempts(id,target_id,sequence,requested_by,result_code,final_version,finished_at)
	SELECT gen_random_uuid(),id,0,$3,$2,version,clock_timestamp() FROM finished`, id, result, actor)
	return id, err
}
