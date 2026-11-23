package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"deskrealm/internal/installrecord"
)

const journalName = "install-record.json"

const (
	imagesStep = iota
	databaseStep
	servicesStep
	healthStep
)

type installRecord installrecord.Record

func installationTaskID(id string) string { return installrecord.TaskID(id) }

func newInstallRecord(id string) installRecord { return installRecord(installrecord.New(id)) }

func loadInstallRecord(directory, id string) (installRecord, error) {
	content, err := readSmallRegular(filepath.Join(directory, journalName), true)
	if err != nil {
		return installRecord{}, err
	}
	result, err := installrecord.Decode(bytes.NewReader(content))
	if err != nil {
		return installRecord{}, err
	}
	if result.DeploymentID != id {
		return installRecord{}, errors.New("installation record belongs to a different deployment")
	}
	return installRecord(result), nil
}

func writeInstallRecord(directory string, record installRecord, prepare bool) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if prepare {
		return writeSynced(filepath.Join(directory, journalName), data, 0600)
	}
	return writeAtomic(directory, journalName, data, 0600)
}

// Older prepared deployments acquire a record under the same deployment lock.
func ensureInstallRecord(directory, id string) (installRecord, error) {
	meta, err := readSmallRegular(filepath.Join(directory, manifestName), true)
	if err != nil {
		return installRecord{}, err
	}
	var d deployment
	if json.Unmarshal(meta, &d) != nil || d.Center.ID != id {
		return installRecord{}, errors.New("cannot attach installation record to this deployment")
	}
	record, err := loadInstallRecord(directory, id)
	if d.InstallRecordVersion == 1 {
		return record, err
	}
	if errors.Is(err, os.ErrNotExist) {
		record = newInstallRecord(id)
		err = writeInstallRecord(directory, record, false)
	}
	if err != nil {
		return installRecord{}, err
	}
	d.InstallRecordVersion = 1
	updated, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return installRecord{}, err
	}
	return record, writeAtomic(directory, manifestName, updated, 0600)
}

func (r *installRecord) step(ctx context.Context, directory string, index int, rerun bool, work func() error) error {
	s := &r.Steps[index]
	began := rerun || s.State != "completed"
	if began {
		s.State, s.FailureCode = "running", ""
		s.Attempts++
		s.UpdatedAt = time.Now().UTC()
		if s.UpdatedAt.Before(s.FirstSuccess) {
			s.UpdatedAt = s.FirstSuccess
		}
		r.Revision++
		if err := writeInstallRecord(directory, *r, false); err != nil {
			return fmt.Errorf("record %s start: %w", s.Name, err)
		}
	}
	workErr := work()
	if !began && workErr == nil {
		return nil
	}
	if !began {
		s.Attempts++
	}
	now := time.Now().UTC()
	// Wall-clock corrections must not put a later revision before its first success.
	if now.Before(s.FirstSuccess) {
		now = s.FirstSuccess
	}
	s.UpdatedAt = now
	switch {
	case workErr == nil:
		s.State, s.FailureCode = "completed", ""
		if s.FirstSuccess.IsZero() {
			s.FirstSuccess = now
		}
	case ctx.Err() != nil:
		s.State, s.FailureCode = "interrupted", "interrupted"
	default:
		s.State, s.FailureCode = "failed", s.Name+"_failed"
	}
	r.Revision++
	if err := writeInstallRecord(directory, *r, false); err != nil {
		return errors.Join(workErr, fmt.Errorf("record %s outcome: %w", s.Name, err))
	}
	return workErr
}
