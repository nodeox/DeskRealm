package install

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCenterUpgradeStateTransitions(t *testing.T) {
	for _, tc := range []struct {
		name           string
		candidateFails bool
		schemaChanges  bool
		resume         bool
		want           string
		transitions    []string
	}{
		{name: "healthy candidate", want: upgradeSucceeded, transitions: []string{"candidate"}},
		{name: "compatible rollback", candidateFails: true, want: upgradeRolledBack, transitions: []string{"candidate", "original"}},
		{name: "incompatible database", schemaChanges: true, want: upgradeRecovery, transitions: []string{"candidate", "stop"}},
		{name: "resume interrupted rollback", resume: true, want: upgradeRolledBack, transitions: []string{"original"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := validateConfig(fixtureConfig(t), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			center, err := prepareDeployment(c)
			if err != nil {
				t.Fatal(err)
			}
			target := "registry.example.test/center@sha256:" + strings.Repeat("b", 64)
			candidate, err := centerWithImage(c, center, target, true)
			if err != nil {
				t.Fatal(err)
			}
			schema := "baseline database schema"
			record := &centerUpgrade{Version: 1, ID: "existing-operation", Deployment: center.ID,
				From: c.Images.Center, Target: target, BackupSHA: strings.Repeat("c", 64),
				Schema: upgradeSchema{Cluster: "1234", Version: 28, SHA: sha256Hex([]byte(schema))}}
			stage := upgradeSwitching
			if tc.resume {
				stage = upgradeRollback
			}
			if err := record.save(c.Directory, stage, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := readPrepared(c); err == nil {
				t.Fatal("normal operations were allowed during an upgrade")
			}
			var calls []string
			run := func(_ context.Context, args ...string) ([]byte, error) {
				switch {
				case slices.Contains(args, "psql"):
					return []byte("1234:28\n"), nil
				case slices.Contains(args, "pg_dump"):
					return []byte(schema), nil
				case slices.Contains(args, "up"):
					if !slices.Contains(args, "--no-deps") || args[len(args)-1] != "center" {
						t.Fatalf("upgrade changed services outside center: %v", args)
					}
					if slices.Contains(args, candidate.centerImageFile) {
						calls = append(calls, "candidate")
						if tc.schemaChanges {
							schema = "database schema changed outside the migration ledger"
						}
					} else {
						calls = append(calls, "original")
					}
				case slices.Contains(args, "stop"):
					if args[len(args)-1] != "center" {
						t.Fatalf("recovery stopped services outside center: %v", args)
					}
					calls = append(calls, "stop")
				default:
					t.Fatalf("unexpected upgrade Docker operation: %v", args)
				}
				return nil, nil
			}
			check := func(_ context.Context, _ config, instance publicDeployment, _ dockerRunner) (centerStatus, error) {
				if instance.centerImage == target && tc.candidateFails {
					return centerStatus{}, errors.New("fixture candidate failed")
				}
				return centerStatus{}, nil
			}
			if tc.resume {
				record, err = centerUpgradeStatus(c)
				if err != nil {
					t.Fatal(err)
				}
			}
			record, err = advanceCenterUpgrade(t.Context(), c, center, record, run, check)
			if record.State != tc.want || (err == nil) != (tc.want == upgradeSucceeded) || !slices.Equal(calls, tc.transitions) {
				t.Fatalf("upgrade outcome %v, transitions %v, error %v", record, calls, err)
			}
			saved, err := centerUpgradeStatus(c)
			if err != nil || saved.ID != "existing-operation" || saved.State != tc.want {
				t.Fatalf("journal outcome %v: %v", saved, err)
			}
			active, err := readPrepared(c)
			switch tc.want {
			case upgradeSucceeded:
				if err != nil || active.centerImage != target || active.centerImageFile != filepath.Join(c.Directory, centerImageName(target)) {
					t.Fatalf("active candidate did not persist: %v", err)
				}
			case upgradeRolledBack:
				if err != nil || active.centerImage != "" {
					t.Fatalf("original baseline was not restored: %v", err)
				}
			default:
				if err == nil {
					t.Fatal("normal operations were allowed with unresolved compatibility")
				}
			}
		})
	}
}
