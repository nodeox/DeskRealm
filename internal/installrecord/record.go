// Package installrecord defines the sanitized snapshot exchanged by the installer and center.
package installrecord

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

const maxSize = 32 << 10

var stepNames = []string{"images", "database", "services", "health"}

type Record struct {
	Version      int    `json:"version"`
	DeploymentID string `json:"deployment_id"`
	TaskID       string `json:"task_id"`
	Revision     uint64 `json:"revision"`
	Steps        []Step `json:"steps"`
}

type Step struct {
	Name         string    `json:"name"`
	State        string    `json:"state"`
	Attempts     uint64    `json:"attempts"`
	UpdatedAt    time.Time `json:"updated_at,omitzero"`
	FirstSuccess time.Time `json:"first_success_at,omitzero"`
	FailureCode  string    `json:"failure_code,omitempty"`
}

func TaskID(id string) string {
	digest := sha256.Sum256([]byte("DeskRealm/center-install/v1/" + id))
	digest[6] = (digest[6] & 15) | (8 << 4)
	digest[8] = (digest[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

func New(centerID string) Record {
	record := Record{Version: 1, DeploymentID: centerID, TaskID: TaskID(centerID), Revision: 1}
	for _, name := range stepNames {
		record.Steps = append(record.Steps, Step{Name: name, State: "pending"})
	}
	return record
}

func Decode(input io.Reader) (Record, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxSize+1))
	if err != nil || len(data) > maxSize {
		return Record{}, errors.New("cannot read bounded installation record")
	}
	var result Record
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
		return Record{}, errors.New("unknown or invalid installation record")
	}
	id, idErr := hex.DecodeString(result.DeploymentID)
	if idErr != nil || len(id) != 16 || hex.EncodeToString(id) != result.DeploymentID ||
		result.Version != 1 || result.TaskID != TaskID(result.DeploymentID) ||
		result.Revision == 0 || result.Revision > math.MaxInt64 || len(result.Steps) != len(stepNames) {
		return Record{}, errors.New("unknown or invalid installation record")
	}
	for i, step := range result.Steps {
		if step.Name != stepNames[i] || step.Attempts >= result.Revision || !step.valid() {
			return Record{}, errors.New("invalid installation step record")
		}
		result.Steps[i].UpdatedAt = step.UpdatedAt.UTC()
		result.Steps[i].FirstSuccess = step.FirstSuccess.UTC()
	}
	return result, nil
}

func (s Step) valid() bool {
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
