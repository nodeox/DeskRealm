package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const journalName = "install-record.json"

const (
	imagesStep = iota
	databaseStep
	servicesStep
	healthStep
)

var installSteps = []string{"images", "database", "services", "health"}

type installRecord struct {
	Version      int              `json:"version"`
	DeploymentID string           `json:"deployment_id"`
	TaskID       string           `json:"task_id"`
	Revision     uint64           `json:"revision"`
	Steps        []installOutcome `json:"steps"`
}

type installOutcome struct {
	Name         string    `json:"name"`
	State        string    `json:"state"`
	Attempts     uint64    `json:"attempts"`
	UpdatedAt    time.Time `json:"updated_at,omitzero"`
	FirstSuccess time.Time `json:"first_success_at,omitzero"`
	FailureCode  string    `json:"failure_code,omitempty"`
}

func installationTaskID(id string) string {
	digest := sha256.Sum256([]byte("DeskRealm/center-install/v1/" + id))
	digest[6] = (digest[6] & 15) | (8 << 4)
	digest[8] = (digest[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

func newInstallRecord(centerID string) installRecord {
	record := installRecord{Version: 1, DeploymentID: centerID, TaskID: installationTaskID(centerID), Revision: 1}
	for _, name := range installSteps {
		record.Steps = append(record.Steps, installOutcome{Name: name, State: "pending"})
	}
	return record
}

func loadInstallRecord(directory, id string) (installRecord, error) {
	var result installRecord
	content, err := readSmallRegular(filepath.Join(directory, journalName), true)
	if err != nil {
		return result, err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) ||
		result.Version != 1 || result.DeploymentID != id || result.TaskID != installationTaskID(id) ||
		result.Revision == 0 || len(result.Steps) != len(installSteps) {
		return installRecord{}, errors.New("unknown or invalid installation record")
	}
	for i, step := range result.Steps {
		if step.Name != installSteps[i] || !step.valid() {
			return installRecord{}, errors.New("invalid installation step record")
		}
	}
	return result, nil
}

func (s installOutcome) valid() bool {
	if s.State == "pending" {
		return s.Attempts == 0 && s.FailureCode == "" && s.UpdatedAt.IsZero() && s.FirstSuccess.IsZero()
	}
	if s.Attempts == 0 || s.UpdatedAt.IsZero() || s.FirstSuccess.After(s.UpdatedAt) {
		return false
	}
	switch s.State {
	case "running":
		return s.FailureCode == ""
	case "completed":
		return s.FailureCode == "" && !s.FirstSuccess.IsZero()
	case "failed":
		return s.FailureCode == s.Name+"_failed"
	case "interrupted":
		return s.FailureCode == "interrupted"
	}
	return false
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

func (r *installRecord) step(ctx context.Context, directory string, index int, work func() error) error {
	s := &r.Steps[index]
	began := s.State != "completed"
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
