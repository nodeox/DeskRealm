package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallRecordResumesWithStableTaskAndRedactedOutcome(t *testing.T) {
	c := preparedConfig(t)
	center, err := prepareDeployment(c)
	if err != nil {
		t.Fatal(err)
	}
	record, err := loadInstallRecord(c.Directory, center.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	private := "postgres://operator:private-database-password@example.invalid"
	if err := record.step(ctx, c.Directory, healthStep, func() error {
		cancel()
		return errors.New(private)
	}); err == nil {
		t.Fatal("interrupted step reported success")
	}
	interrupted, err := loadInstallRecord(c.Directory, center.ID)
	if err != nil || interrupted.TaskID != record.TaskID || interrupted.Steps[healthStep].State != "interrupted" {
		t.Fatalf("invalid redacted outcome: %+v: %v", interrupted, err)
	}
	content, err := os.ReadFile(filepath.Join(c.Directory, journalName))
	if err != nil || strings.Contains(string(content), private) {
		t.Fatal("step record included raw execution output")
	}
	noFailure := func() error { return nil }
	if err := interrupted.step(context.Background(), c.Directory, healthStep, noFailure); err != nil {
		t.Fatal(err)
	}
	rev := interrupted.Revision
	if err := interrupted.step(context.Background(), c.Directory, healthStep, noFailure); err != nil {
		t.Fatal(err)
	}
	if interrupted.Revision != rev || interrupted.Steps[healthStep].Attempts != 2 ||
		interrupted.Steps[healthStep].State != "completed" {
		t.Fatal("rechecking a completed step rewrote its outcome")
	}
}

func TestInstallRecordAdoptsLegacyAndRejectsLostRecord(t *testing.T) {
	c := preparedConfig(t)
	center, err := prepareDeployment(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.Directory, journalName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareDeployment(c); err == nil || !strings.Contains(err.Error(), journalName) {
		t.Fatal("new deployment with a missing journal was automatically prepared")
	}
	manifest, err := os.ReadFile(filepath.Join(c.Directory, manifestName))
	var old deployment
	if err != nil || json.Unmarshal(manifest, &old) != nil {
		t.Fatal("read current deployment metadata")
	}
	old.InstallRecordVersion = 0
	legacy, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(c.Directory, manifestName, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if loaded, err := readPrepared(c); err != nil || loaded != center {
		t.Fatalf("legacy deployment did not preserve identity: %v", err)
	}
	if record, err := ensureInstallRecord(c.Directory, center.ID); err != nil || record.TaskID != installationTaskID(center.ID) {
		t.Fatalf("legacy deployment did not acquire a local installation record: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrepared(c); err == nil {
		t.Fatal("adopted deployment silently accepted a missing journal")
	}
}
