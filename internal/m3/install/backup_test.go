package install

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"deskrealm/internal/m1"
)

const backupTestPostgres = "docker.io/library/postgres:18.7-bookworm@sha256:194e55feb2597b1465dbee81cc9fd8d204566cd2eb42c8d22fd6c931a38781b1"

func TestCenterBackupRestoresOriginalCluster(t *testing.T) {
	if os.Getenv("DESKREALM_TEST_CENTER_BACKUP") != "1" {
		t.Skip("run scripts/deskrealm-m3-test center-restore")
	}
	ctx := t.Context()
	c := fixtureConfig(t)
	c.Images.PostgreSQL = backupTestPostgres
	c, err := validateConfig(c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	center, err := prepareDeployment(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := docker(ctx, "image", "inspect", backupTestPostgres); err != nil {
		if out, err := docker(ctx, "pull", backupTestPostgres); err != nil {
			t.Fatalf("pull PostgreSQL: %v %s", err, out)
		}
	}
	parent := filepath.Dir(c.Directory)
	override := filepath.Join(parent, "postgres-test.json")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, []byte(`{"networks":{"database":{"internal":false}},"services":{"database":{"ports":["`+endpoint+`:5432"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	runFixture := func(ctx context.Context, args ...string) ([]byte, error) {
		return docker(ctx, append(append(composeArgs(center), "--file", override), args...)...)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = runFixture(cleanup, "down", "--volumes")
	})
	if _, err := runFixture(ctx, "up", "-d", "--wait", "--no-deps", "database"); err != nil {
		t.Fatalf("start fixture database: %v", err)
	}
	cluster, _, err := databaseStorage(ctx, c.Directory, backupTestPostgres, docker)
	if err != nil || !validClusterID(cluster) {
		t.Fatalf("test cluster identity: %q %v", cluster, err)
	}
	if err := recordDatabase(c.Directory, center.ID, cluster); err != nil {
		t.Fatal(err)
	}
	const migrations = "postgresql://postgres@/deskrealm?host=/var/run/postgresql"
	query := func(instance publicDeployment, sql string) string {
		t.Helper()
		out, err := composeCommand(ctx, docker, instance, "exec", "-T", "--user", "postgres", "database",
			"psql", "-XAt", "--set", "ON_ERROR_STOP=1", migrations, "-c", sql)
		if err != nil {
			t.Fatalf("fixture PostgreSQL query: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	env, err := readSmallRegular(filepath.Join(c.Directory, "secrets/postgres.env"), true)
	if err != nil {
		t.Fatal(err)
	}
	dsn := "postgresql://deskrealm_migrate:" + envValue(env, "DESKREALM_DB_MIGRATION_PASSWORD") + "@" + endpoint + "/deskrealm?sslmode=disable"
	if err := m1.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	query(center, `
INSERT INTO organizations(name) VALUES('Restore fixture');
INSERT INTO users(organization_id,username,password_hash)
 SELECT id,'test_admin','fixture' FROM organizations;
INSERT INTO device_groups(organization_id,name) SELECT id,'Original endpoints' FROM organizations;
INSERT INTO devices(organization_id,group_id,platform,rustdesk_id,public_key,registration_state)
 SELECT organization_id,id,'windows-amd64','restore01',decode(repeat('ab',32),'hex'),'approved' FROM device_groups;
INSERT INTO managed_policy_versions(organization_id,name,schema_version,allowed_actions,unattended_purposes,payload_sha256,published_by)
 SELECT organization_id,'Restore policy',1,ARRAY['view'],ARRAY[]::text[],decode(repeat('cd',32),'hex'),id FROM users;
INSERT INTO tasks(organization_id,kind,owner_id,device_id)
 SELECT u.organization_id,'device_enrollment',u.id,d.id FROM users u JOIN devices d USING (organization_id);
INSERT INTO audit_events(organization_id,actor_id,action,result)
 SELECT organization_id,id,'backup.fixture','success' FROM users;
UPDATE bootstrap_state SET initialized_at=now() WHERE singleton;
`)
	const recordIDs = `SELECT json_build_array((SELECT id FROM users),(SELECT id FROM devices),
		(SELECT version FROM managed_policy_versions),(SELECT id FROM tasks),(SELECT id FROM audit_events WHERE action='backup.fixture'))`
	expectedIDs := query(center, recordIDs)
	schema, err := inspectUpgradeSchema(ctx, center, docker)
	if err != nil || schema.Cluster != cluster {
		t.Fatalf("capture source database schema: %+v %v", schema, err)
	}
	configFile := filepath.Join(parent, "center.json")
	configJSON, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSynced(configFile, configJSON, 0600); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(parent, "backup")
	var stdout bytes.Buffer
	if err := Run(ctx, []string{"backup", "--config", configFile, "--output", backupDir}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result backupResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Cluster != cluster || result.DeploymentID != center.ID || result.RendezvousSHA != center.RendezvousKeySHA256 ||
		result.SigningSHA != center.SigningKeySHA256 || !digestHex.MatchString(result.BackupSHA) {
		t.Fatalf("backup public identity differs: %+v", result)
	}
	// This later row must remain exclusively in the source.
	query(center, `INSERT INTO audit_events(organization_id,action,result) SELECT id,'after.backup','success' FROM organizations;`)
	destination := filepath.Join(parent, "restored")
	stdout.Reset()
	if err := Run(ctx, []string{"restore-check", "--backup", backupDir, "--sha256", result.BackupSHA, "--output", destination}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	var restored restoreResult
	if err := json.Unmarshal(stdout.Bytes(), &restored); err != nil {
		t.Fatal(err)
	}
	expected := restoredCounts{Initialized: true, Users: 1, Devices: 1, Policies: 1, Tasks: 1, AuditEvents: 1}
	if restored.Database != expected || restored.Schema != schema || restored.Cluster != cluster || restored.RendezvousSHA != result.RendezvousSHA ||
		restored.SigningSHA != result.SigningSHA || restored.DeploymentID != result.DeploymentID {
		t.Fatalf("isolated restore differs: %+v", restored)
	}
	restoredConfig, err := loadConfig(restored.Config, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := readPrepared(restoredConfig)
	if err != nil || identity.SigningKey != center.SigningKey || identity.RendezvousKey != center.RendezvousKey {
		t.Fatalf("restored normal installer identity differs: %+v %v", identity, err)
	}
	if live := query(center, "SELECT count(*) FROM audit_events"); live != "2" {
		t.Fatalf("restore modified source: %s", live)
	}
	if _, err := verifyUpgradeRecovery(ctx, c, center, schema, c.Images.Center, destination, docker); err != nil {
		t.Fatalf("matching restored schema did not qualify for upgrade: %v", err)
	}
	query(center, "ALTER TABLE public.organizations ADD COLUMN schema_test_column text")
	changed, err := inspectUpgradeSchema(ctx, center, docker)
	if err != nil || changed.Cluster != cluster || changed.Version != schema.Version || changed.SHA == schema.SHA {
		t.Fatalf("unrecorded schema change was not detected: %+v %v", changed, err)
	}
	if _, err := verifyUpgradeRecovery(ctx, c, center, changed, c.Images.Center, destination, docker); err == nil {
		t.Fatal("recovery with an old schema qualified for upgrade")
	}
	report, err := readSmallRegular(filepath.Join(destination, recoveryReport), true)
	if err != nil || !json.Valid(report) || !bytes.Contains(report, []byte(result.BackupSHA)) {
		t.Fatalf("restore verification record was not persisted: %v", err)
	}
	if _, err := restoreCenterBackup(ctx, backupDir, result.BackupSHA, c.Directory); err == nil {
		t.Fatal("restore allowed original deployment as output")
	}
	archive, err := os.OpenFile(filepath.Join(backupDir, backupDatabase), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	offset := postgresVersionOffset(t, archive)
	var last [1]byte
	if _, err := archive.ReadAt(last[:], offset); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.WriteAt([]byte{last[0] ^ 1}, offset); err != nil {
		t.Fatal(err)
	}
	corrupted := filepath.Join(parent, "corrupted")
	if _, err := restoreCenterBackup(ctx, backupDir, result.BackupSHA, corrupted); err == nil || !strings.Contains(err.Error(), "digest differs") {
		t.Fatalf("same-size archive change accepted: %v", err)
	}
	if _, err := os.Stat(corrupted); !os.IsNotExist(err) {
		t.Fatalf("corrupt recovery left destination behind: %v", err)
	}
	if _, err := archive.WriteAt(last[:], offset); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(backupDir, "secrets/signing.seed")); err != nil {
		t.Fatal(err)
	}
	failed := filepath.Join(parent, "missing-key")
	if _, err := restoreCenterBackup(ctx, backupDir, result.BackupSHA, failed); err == nil || !strings.Contains(err.Error(), "signing.seed") {
		t.Fatalf("missing key accepted: %v", err)
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatalf("failed recovery left destination behind: %v", err)
	}
	if _, err := runFixture(ctx, "down", "--volumes"); err != nil {
		t.Fatal(err)
	}
	if err := ensureDatabase(ctx, restoredConfig, identity, cluster, nil, docker); err != nil {
		t.Fatalf("restart recovered database through the normal installer: %v", err)
	}
	if actual := query(identity, recordIDs); actual != expectedIDs {
		t.Fatalf("restored database business record IDs or version changed: got %s, want %s", actual, expectedIDs)
	}
	t.Logf("Restored original cluster %s, both center keys and application records: %+v", cluster, restored.Database)
}

func postgresVersionOffset(t *testing.T, file *os.File) int64 {
	t.Helper()
	reader := tar.NewReader(file)
	for {
		entry, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		if entry.Name == "PG_VERSION" {
			offset, err := file.Seek(0, io.SeekCurrent)
			if err != nil {
				t.Fatal(err)
			}
			return offset
		}
	}
}

func TestRecoveryRejectsDifferentManifestDigest(t *testing.T) {
	dir := t.TempDir()
	raw, err := json.Marshal(backupManifest{Version: 1, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backupManifestName), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readCenterBackup(dir, strings.Repeat("a", 64)); err == nil {
		t.Fatal("accepted a different manifest digest")
	}
}

func TestRecoveryRejectsArchiveChanges(t *testing.T) {
	file := filepath.Join(t.TempDir(), backupDatabase)
	if err := os.WriteFile(file, []byte("invalid archive"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyBackupArchive(t.Context(), file, "", nil); err == nil {
		t.Fatal("accepted a damaged archive")
	}
}
