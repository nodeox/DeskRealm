package install

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type restoredCounts struct {
	Initialized bool  `json:"initialized"`
	Users       int64 `json:"users"`
	Devices     int64 `json:"devices"`
	Policies    int64 `json:"policies"`
	Tasks       int64 `json:"tasks"`
	AuditEvents int64 `json:"audit_events"`
}

type restoreResult struct {
	backupResult
	Config     string         `json:"config"`
	VerifiedAt time.Time      `json:"verified_at"`
	Database   restoredCounts `json:"database"`
}

func restoreCenterBackup(ctx context.Context, backup, expectedSHA, output string) (restoreResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	source, err := canonicalDirectory(backup)
	if err != nil {
		return restoreResult{}, err
	}
	directory, err := canonicalDirectory(output)
	if err != nil {
		return restoreResult{}, err
	}
	if pathsOverlap(source, directory) || !digestHex.MatchString(expectedSHA) {
		return restoreResult{}, errors.New("restore requires a separate directory and the recorded backup SHA-256")
	}
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return restoreResult{}, errors.New("backup directory must be a private directory")
	}
	sourceLock, err := lockDeployment(source)
	if err != nil {
		return restoreResult{}, err
	}
	defer sourceLock.Close()
	manifest, original, err := readCenterBackup(source, expectedSHA)
	if err != nil {
		return restoreResult{}, err
	}
	if pathsOverlap(original.Config.Directory, directory) {
		return restoreResult{}, errors.New("restore directory overlaps the original deployment")
	}
	stage, err := newRecoveryStage(directory)
	if err != nil {
		return restoreResult{}, err
	}
	defer stage.Close()
	for name, entry := range manifest.Files {
		if name == backupDatabase {
			continue
		}
		data, err := readSmallRegular(filepath.Join(source, name), true)
		if err != nil || int64(len(data)) != entry.Size || sha256Hex(data) != entry.SHA {
			return restoreResult{}, fmt.Errorf("backup metadata %s is missing or differs from the manifest", name)
		}
		if err := writeSynced(filepath.Join(stage.work, name), data, 0600); err != nil {
			return restoreResult{}, err
		}
	}
	if err := validateIdentityFiles(stage.work, manifest.Center); err != nil {
		return restoreResult{}, err
	}
	if err := validateBackupTLS(stage.work, original.Config); err != nil {
		return restoreResult{}, err
	}
	if _, err := loadInstallRecord(stage.work, original.Center.ID); err != nil {
		return restoreResult{}, err
	}
	cluster, err := readDatabaseRecord(stage.work, original.Center.ID)
	if err != nil || cluster != manifest.Cluster {
		return restoreResult{}, errors.New("backup does not contain the original PostgreSQL installation record")
	}
	if err := rebuildRecoveryConfig(stage.work, directory, original); err != nil {
		return restoreResult{}, err
	}
	pgRoot := filepath.Join(stage.work, "postgres")
	pgData := filepath.Join(pgRoot, "18", "docker")
	if err := os.MkdirAll(pgData, 0700); err != nil {
		return restoreResult{}, err
	}
	database := manifest.Files[backupDatabase]
	if _, err := verifyBackupArchive(ctx, filepath.Join(source, backupDatabase), pgData, &database); err != nil {
		return restoreResult{}, err
	}
	if _, _, err := checkRuntime(ctx, docker); err != nil {
		return restoreResult{}, err
	}
	counts, err := verifyRestoredPostgres(ctx, pgRoot, manifest.Cluster, original.Center.runtimeImages(original.Config.Images).PostgreSQL, docker)
	if err != nil {
		return restoreResult{}, err
	}
	result := restoreResult{backupResult: manifest.result(directory, expectedSHA), VerifiedAt: time.Now().UTC(),
		Config: filepath.Join(directory, recoveryConfig), Database: counts}
	report, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return restoreResult{}, err
	}
	if err := writeSynced(filepath.Join(stage.work, recoveryReport), report, 0600); err != nil {
		return restoreResult{}, err
	}
	if err := stage.Publish(); err != nil {
		return restoreResult{}, err
	}
	return result, nil
}

func readCenterBackup(directory, expectedSHA string) (backupManifest, deployment, error) {
	var m backupManifest
	var d deployment
	raw, err := readSmallRegular(filepath.Join(directory, backupManifestName), true)
	if err != nil || sha256Hex(raw) != expectedSHA || json.Unmarshal(raw, &m) != nil ||
		m.Version != 1 || m.CreatedAt.IsZero() || !validClusterID(m.Cluster) {
		return m, d, errors.New("backup manifest does not match the recorded SHA-256")
	}
	data, err := readSmallRegular(filepath.Join(directory, manifestName), true)
	if err != nil || json.Unmarshal(data, &d) != nil || d.Version != 1 || d.Center != m.Center ||
		!filepath.IsAbs(d.Config.Directory) ||
		!d.Center.Offline.valid(d.Config.Offline != (offlineConfig{})) ||
		!validImage(d.Config.Images.PostgreSQL) || !validImage(d.Config.Images.Center) || !validImage(d.Config.Images.Rendezvous) {
		return m, d, errors.New("backup deployment metadata is invalid")
	}
	wanted := backupMetadata(d.Config)
	if len(m.Files) != len(wanted)+1 {
		return m, d, errors.New("backup does not contain the expected file inventory")
	}
	wanted[backupDatabase] = ""
	for name := range wanted {
		item := m.Files[name]
		if item.Size <= 0 || !digestHex.MatchString(item.SHA) {
			return m, d, fmt.Errorf("backup inventory has no valid record for %s", name)
		}
	}
	if item := m.Files[manifestName]; item.Size != int64(len(data)) || item.SHA != sha256Hex(data) {
		return m, d, errors.New("backup deployment metadata differs from its inventory")
	}
	for _, name := range deploymentFiles(d.Config) {
		if m.Files[name].SHA != d.Checksums[name] {
			return m, d, fmt.Errorf("backup %s differs from the deployment checksums", name)
		}
	}
	return m, d, nil
}

func rebuildRecoveryConfig(work, directory string, d deployment) error {
	c := d.Config
	c.Directory = directory
	if c.HTTPS.Mode == httpsProvided {
		c.HTTPS.Certificate = filepath.Join(directory, backupCertificate)
		c.HTTPS.PrivateKey = filepath.Join(directory, backupPrivateKey)
	}
	if c.HTTPS.CACertificate != "" {
		c.HTTPS.CACertificate = filepath.Join(directory, backupCA)
	}
	if c.DefaultRelay.ID != "" {
		raw, err := readSmallRegular(filepath.Join(work, relayEnvFile), true)
		if err != nil {
			return err
		}
		token := envValue(raw, relayTokenEnv)
		if err := writeSynced(filepath.Join(work, recoveryRelay), []byte(token+"\n"), 0600); err != nil {
			return err
		}
		c.DefaultRelay.TokenFile = filepath.Join(directory, recoveryRelay)
	}
	d.Config = c
	d.RunAs = strconv.Itoa(os.Geteuid()) + ":" + strconv.Itoa(os.Getegid())
	d.Center.ComposeFile = filepath.Join(directory, composeName)
	compose, err := renderCompose(d)
	if err != nil {
		return err
	}
	d.Checksums[composeName] = sha256Hex(compose)
	deploymentJSON, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	configJSON, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(work, composeName, compose, 0600); err != nil {
		return err
	}
	if err := writeAtomic(work, manifestName, deploymentJSON, 0600); err != nil {
		return err
	}
	return writeSynced(filepath.Join(work, recoveryConfig), configJSON, 0600)
}

func verifyRestoredPostgres(ctx context.Context, root, expectedCluster, image string, run dockerRunner) (restoredCounts, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var buf strings.Builder
	csvWriter := csv.NewWriter(&buf)
	if err := csvWriter.Write([]string{"type=bind", "source=" + root, "target=/var/lib/postgresql"}); err != nil {
		return restoredCounts{}, err
	}
	csvWriter.Flush()
	if err := csvWriter.Error(); err != nil {
		return restoredCounts{}, err
	}
	name := "deskrealm-restore-" + strings.ToLower(rand.Text())
	cleanup := func() error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		// Ignore an absent container, but report a real removal failure.
		out, err := run(ctx, "container", "ls", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}")
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return err
		}
		_, err = run(ctx, "container", "rm", "--force", name)
		return err
	}
	defer cleanup()
	out, err := run(ctx, "run", "--rm", "--name", name, "--pull", "never", "--network", "none", "--read-only",
		"--tmpfs", "/tmp:rw,nosuid,nodev,noexec", "--user", "0:0",
		"--entrypoint", "/bin/sh", "--mount", strings.TrimSuffix(buf.String(), "\n"), image,
		"-ec", isolatedPostgresCheck, "deskrealm-restore", expectedCluster)
	if err != nil {
		if failure, ok := errors.AsType[*exec.ExitError](err); ok {
			for _, phase := range []string{"manifest and WAL", "startup", "application records", "shutdown"} {
				if bytes.Contains(failure.Stderr, []byte("DeskRealm restore step: "+phase+"\n")) {
					return restoredCounts{}, fmt.Errorf("isolated PostgreSQL verification failed at %s", phase)
				}
			}
		}
		return restoredCounts{}, errors.New("isolated PostgreSQL startup, content verification or clean shutdown failed")
	}
	var report restoredCounts
	if err := json.Unmarshal(bytes.TrimSpace(out), &report); err != nil {
		return restoredCounts{}, errors.New("isolated PostgreSQL returned invalid verification evidence")
	}
	return report, nil
}

const isolatedPostgresCheck = `
set -eu
data=/var/lib/postgresql/18/docker
socket=/tmp/deskrealm-restore
step='manifest and WAL'
started=0
owner=
cleanup() {
  code=$?
  if [ "$code" != 0 ]; then printf 'DeskRealm restore step: %s\n' "$step" >&2; fi
  if [ "$started" = 1 ]; then
    gosu postgres pg_ctl -D "$data" -m fast -w -t 60 stop >/dev/null || true
  fi
  if [ -n "$owner" ]; then chown -R "$owner" "$data"; fi
}
trap cleanup EXIT
trap 'exit 143' TERM
[ "$(cat "$data/PG_VERSION")" = 18 ]
[ "$(LC_ALL=C pg_controldata "$data" | sed -n 's/^Database system identifier:[[:space:]]*//p')" = "$1" ]
pg_verifybackup --quiet "$data"
mkdir -m 700 "$socket"
owner="$(stat -c '%u:%g' "$data")"
step=startup
chown -R postgres:postgres "$data" "$socket"
cat > "$socket/postgresql.conf" <<'EOF'
listen_addresses = ''
unix_socket_directories = '/tmp/deskrealm-restore'
unix_socket_permissions = 0700
hba_file = '/tmp/deskrealm-restore/pg_hba.conf'
ident_file = '/tmp/deskrealm-restore/pg_ident.conf'
hot_standby = off
max_connections = 256
max_worker_processes = 32
max_wal_senders = 16
max_prepared_transactions = 0
max_locks_per_transaction = 128
archive_mode = off
restore_command = ''
shared_preload_libraries = ''
logging_collector = off
EOF
printf '%s\n' 'local all postgres trust' > "$socket/pg_hba.conf"
touch "$socket/pg_ident.conf"
# The copied archive came from a managed compose cluster. Disable its local
# ALTER SYSTEM overrides for the isolated startup and retain them for recovery.
if [ -f "$data/postgresql.auto.conf" ]; then
  cp -p "$data/postgresql.auto.conf" "$socket/original.auto.conf"
  : > "$data/postgresql.auto.conf"
fi
started=1
gosu postgres pg_ctl -D "$data" -l "$socket/server.log" -o "-c config_file=$socket/postgresql.conf" -w -t 100 start >/dev/null
step='application records'
gosu postgres psql -h "$socket" -U postgres -d deskrealm -XqAt --set ON_ERROR_STOP=1 <<'SQL'
SET ROLE deskrealm;
SELECT json_build_object(
  'initialized', (SELECT initialized_at IS NOT NULL FROM public.bootstrap_state WHERE singleton),
  'users', (SELECT count(*) FROM public.users),
  'devices', (SELECT count(*) FROM public.devices),
  'policies', (SELECT count(*) FROM public.managed_policy_versions),
  'tasks', (SELECT count(*) FROM public.tasks),
  'audit_events', (SELECT count(*) FROM public.audit_events)
);
SQL
step=shutdown
gosu postgres pg_ctl -D "$data" -m fast -w -t 60 stop >/dev/null
started=0
if [ -f "$socket/original.auto.conf" ]; then
  cp -p "$socket/original.auto.conf" "$data/postgresql.auto.conf"
fi
chown -R "$owner" "$data"
trap - EXIT
`
