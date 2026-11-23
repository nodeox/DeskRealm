package m1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"deskrealm/internal/installrecord"
	"github.com/jackc/pgx/v5"
)

var (
	ErrInstallationRecordInvalid  = errors.New("invalid installation record on standard input")
	ErrInstallationRecordConflict = errors.New("installation record conflicts with an imported task or revision")
	ErrInstallationRecordActor    = errors.New("the import requires an active platform administrator as its audit actor")
)

type InstallationImport struct {
	TaskID   string `json:"task_id"`
	Revision uint64 `json:"revision"`
	Imported bool   `json:"imported"`
}

func (s Store) ImportInstallation(ctx context.Context, username string, input io.Reader) (InstallationImport, error) {
	record, err := installrecord.Decode(input)
	if err != nil {
		return InstallationImport{}, ErrInstallationRecordInvalid
	}
	snapshot, err := json.Marshal(record)
	if err != nil {
		return InstallationImport{}, ErrInstallationRecordInvalid
	}
	result := InstallationImport{TaskID: record.TaskID, Revision: record.Revision}
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var organization, owner string
		// Match the organization-first order used by authorization-changing writes.
		err := tx.QueryRow(ctx, "SELECT id FROM organizations WHERE singleton FOR NO KEY UPDATE").Scan(&organization)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInstallationRecordActor
		}
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT u.id FROM users u WHERE u.organization_id=$1 AND u.username=$2
			AND u.status='active' AND `+adminBinding, organization, normalizeUsername(username)).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInstallationRecordActor
		}
		if err != nil {
			return err
		}

		// The host record's deterministic ID is also the task ID exposed to users.
		if _, err := tx.Exec(ctx, `INSERT INTO tasks(id,organization_id,kind,owner_id)
			VALUES ($1,$2,'center_install',$3) ON CONFLICT (id) DO NOTHING`, record.TaskID, organization, owner); err != nil {
			return err
		}
		var kindOK bool
		err = tx.QueryRow(ctx, `SELECT organization_id=$2 AND kind='center_install' FROM tasks WHERE id=$1`,
			record.TaskID, organization).Scan(&kindOK)
		if err != nil {
			return err
		}
		if !kindOK {
			return ErrInstallationRecordConflict
		}
		var same bool
		err = tx.QueryRow(ctx, `SELECT snapshot=$3::jsonb FROM installation_task_records WHERE task_id=$1 AND revision=$2`,
			record.TaskID, int64(record.Revision), snapshot).Scan(&same)
		if err == nil {
			if !same {
				return ErrInstallationRecordConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var previous []byte
		err = tx.QueryRow(ctx, `SELECT snapshot FROM installation_task_records WHERE task_id=$1 ORDER BY revision DESC LIMIT 1`,
			record.TaskID).Scan(&previous)
		if err == nil {
			prior, decodeErr := installrecord.Decode(bytes.NewReader(previous))
			if decodeErr != nil || !installationAdvances(prior, record) {
				return ErrInstallationRecordConflict
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO installation_task_records(task_id,revision,snapshot)
			VALUES ($1,$2,$3)`, record.TaskID, int64(record.Revision), snapshot); err != nil {
			return err
		}
		for _, step := range record.Steps {
			state, code := "waiting", "install_"+step.State
			switch step.State {
			case "completed":
				state = "succeeded"
			case "failed", "interrupted":
				state = "failed"
			}
			// Local execution has no center lease. Only host imports can update these targets.
			if _, err := tx.Exec(ctx, `INSERT INTO task_targets(task_id,object_key,state,result_code)
				VALUES ($1,$2,$3,$4) ON CONFLICT (task_id,object_key) DO UPDATE
				SET state=EXCLUDED.state,result_code=EXCLUDED.result_code,version=task_targets.version+1
				WHERE (task_targets.state,task_targets.result_code) IS DISTINCT FROM (EXCLUDED.state,EXCLUDED.result_code)`,
				record.TaskID, step.Name, state, code); err != nil {
				return err
			}
		}
		_, err = addAuditEvent(ctx, tx, auditEvent{
			OrganizationID: organization, ActorID: owner, TaskID: record.TaskID, ResourceID: record.TaskID,
			Action: "task.install_record_imported", Result: "accepted", Source: "center_install",
			SourceKey: fmt.Sprintf("%s:%d", record.TaskID, record.Revision),
		})
		result.Imported = err == nil
		return err
	})
	if err != nil {
		return InstallationImport{}, err
	}
	return result, nil
}

func installationAdvances(old, next installrecord.Record) bool {
	if old.DeploymentID != next.DeploymentID || old.Revision >= next.Revision {
		return false
	}
	for i, step := range old.Steps {
		newStep := next.Steps[i]
		if newStep.Attempts < step.Attempts ||
			(!step.FirstSuccess.IsZero() && !newStep.FirstSuccess.Equal(step.FirstSuccess)) {
			return false
		}
	}
	return true
}
