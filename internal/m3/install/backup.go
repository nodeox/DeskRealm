package install

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"deskrealm/internal/m1"
)

const (
	backupManifestName = "backup.json"
	backupDatabase     = "postgres.tar"
	backupCertificate  = "secrets/recovery-tls.crt"
	backupPrivateKey   = "secrets/recovery-tls.key"
	backupCA           = "secrets/recovery-ca.pem"
	recoveryRelay      = "secrets/recovery-relay.token"
	recoveryConfig     = "center-config.json"
	recoveryReport     = "restore-verification.json"
)

type backupFile struct {
	Size int64  `json:"bytes"`
	SHA  string `json:"sha256"`
}

type backupManifest struct {
	Version   int                   `json:"version"`
	CreatedAt time.Time             `json:"created_at"`
	Center    publicDeployment      `json:"center"`
	Cluster   string                `json:"postgres_system_identifier"`
	Files     map[string]backupFile `json:"files"`
}

type backupResult struct {
	Directory     string `json:"directory"`
	BackupSHA     string `json:"backup_sha256"`
	DeploymentID  string `json:"deployment_id"`
	Cluster       string `json:"postgres_system_identifier"`
	RendezvousSHA string `json:"rendezvous_public_key_sha256"`
	SigningSHA    string `json:"signing_public_key_sha256"`
}

func (m backupManifest) result(directory, digest string) backupResult {
	return backupResult{Directory: directory, BackupSHA: digest, DeploymentID: m.Center.ID, Cluster: m.Cluster,
		RendezvousSHA: m.Center.RendezvousKeySHA256, SigningSHA: m.Center.SigningKeySHA256}
}

func centerBackup(ctx context.Context, c config, output string) (backupResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	directory, err := canonicalDirectory(output)
	if err != nil {
		return backupResult{}, err
	}
	if pathsOverlap(c.Directory, directory) {
		return backupResult{}, errors.New("backup directory must be separate from the deployment")
	}
	lock, err := lockDeployment(c.Directory)
	if err != nil {
		return backupResult{}, err
	}
	defer lock.Close()
	center, err := readPrepared(c)
	if err != nil {
		return backupResult{}, err
	}
	if _, _, err := checkRuntime(ctx, docker); err != nil {
		return backupResult{}, err
	}
	cluster, err := readDatabaseRecord(c.Directory, center.ID)
	if err != nil || cluster == "" {
		return backupResult{}, errors.New("a verified PostgreSQL installation record is required for a backup")
	}
	services, err := runningServices(ctx, c, center, docker)
	if err != nil {
		return backupResult{}, err
	}
	if !serviceReady("database", services["database"]) {
		return backupResult{}, errors.New("the center PostgreSQL service must be healthy")
	}
	active, err := composeCommand(ctx, docker, center, "exec", "-T", "--user", "postgres", "database",
		"psql", "-XAt", "--no-password", "-h", "/var/run/postgresql", "-U", "postgres", "-d", "deskrealm",
		"-c", "SELECT system_identifier FROM pg_control_system()")
	if err != nil || strings.TrimSpace(string(active)) != cluster {
		return backupResult{}, errors.New("running PostgreSQL does not match the installation record")
	}

	stage, err := newRecoveryStage(directory)
	if err != nil {
		return backupResult{}, err
	}
	defer stage.Close()
	m := backupManifest{Version: 1, CreatedAt: time.Now().UTC(), Center: center, Cluster: cluster, Files: make(map[string]backupFile)}
	files := backupMetadata(c)
	upgrade, err := readCenterUpgrade(c.Directory, center.ID)
	if err != nil {
		return backupResult{}, err
	}
	for _, name := range upgradeBackupNames(c, upgrade) {
		files[name] = filepath.Join(c.Directory, name)
	}
	for name, source := range files {
		data, err := readBackupMetadata(source)
		if err != nil {
			return backupResult{}, fmt.Errorf("cannot back up %s: %w", name, err)
		}
		if err := writeSynced(filepath.Join(stage.work, name), data, 0600); err != nil {
			return backupResult{}, err
		}
		m.Files[name] = backupFile{Size: int64(len(data)), SHA: sha256Hex(data)}
	}
	if err := validateIdentityFiles(stage.work, center); err != nil {
		return backupResult{}, err
	}
	if err := validateBackupTLS(stage.work, c); err != nil {
		return backupResult{}, err
	}
	var record backupFile
	record, err = streamBaseBackup(ctx, filepath.Join(stage.work, backupDatabase), center)
	if err != nil {
		return backupResult{}, err
	}
	m.Files[backupDatabase] = record
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return backupResult{}, err
	}
	if err := writeSynced(filepath.Join(stage.work, backupManifestName), raw, 0600); err != nil {
		return backupResult{}, err
	}
	if err := stage.Publish(); err != nil {
		return backupResult{}, err
	}
	return m.result(directory, sha256Hex(raw)), nil
}

func backupMetadata(c config) map[string]string {
	files := make(map[string]string)
	for _, name := range append(append([]string{}, deploymentFiles(c)...), manifestName, journalName, databaseRecord) {
		files[name] = filepath.Join(c.Directory, name)
	}
	if c.HTTPS.Mode == httpsProvided {
		files[backupCertificate] = c.HTTPS.Certificate
		files[backupPrivateKey] = c.HTTPS.PrivateKey
	}
	if c.HTTPS.CACertificate != "" {
		files[backupCA] = c.HTTPS.CACertificate
	}
	return files
}

func readBackupMetadata(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1<<20 {
		return nil, errors.New("expected a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, info.Size()+1))
	if err != nil || int64(len(data)) != info.Size() {
		return nil, errors.New("metadata changed during backup")
	}
	return data, nil
}

func streamBaseBackup(ctx context.Context, file string, center publicDeployment) (backupFile, error) {
	out, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return backupFile{}, err
	}
	cmd := exec.CommandContext(ctx, "docker", composeArgs(center, "exec", "-T", "--user", "postgres", "database",
		"pg_basebackup", "-D", "-", "-Ft", "--wal-method=fetch", "--checkpoint=fast", "--manifest-checksums=SHA256",
		"--no-password", "-h", "/var/run/postgresql", "-U", "postgres")...)
	cmd.Stdout = out
	if err := errors.Join(cmd.Run(), out.Sync(), out.Close()); err != nil {
		return backupFile{}, errors.New("PostgreSQL base backup failed")
	}
	return verifyBackupArchive(ctx, file, "", nil)
}

func validateIdentityFiles(directory string, identity publicDeployment) error {
	private, err := readSmallRegular(filepath.Join(directory, "rendezvous/id_ed25519"), true)
	key, decodeErr := base64.StdEncoding.Strict().DecodeString(string(private))
	if err != nil || decodeErr != nil || len(key) != ed25519.PrivateKeySize {
		return errors.New("invalid rendezvous private key in backup")
	}
	derived := ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])
	pub := base64.StdEncoding.EncodeToString(derived.Public().(ed25519.PublicKey))
	if !derived.Equal(ed25519.PrivateKey(key)) || pub != identity.RendezvousKey {
		return errors.New("rendezvous private key differs from the recorded identity")
	}
	signing, err := readSmallRegular(filepath.Join(directory, "secrets/signing.seed"), true)
	seed, decodeErr := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSuffix(string(signing), "\n"))
	if err != nil || decodeErr != nil || len(seed) != ed25519.SeedSize ||
		base64.RawURLEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)) != identity.SigningKey {
		return errors.New("managed signing key differs from the recorded identity")
	}
	env, err := readSmallRegular(filepath.Join(directory, "secrets/center.env"), true)
	if err != nil {
		return err
	}
	if key, err := m1.ParseTOTPKey(envValue(env, "DESKREALM_TOTP_KEY")); err != nil || key == nil {
		return errors.New("invalid TOTP encryption key in backup")
	}
	return nil
}

func envValue(env []byte, key string) string {
	for _, line := range strings.Split(string(env), "\n") {
		name, value, _ := strings.Cut(line, "=")
		if name == key {
			return value
		}
	}
	return ""
}

func validateBackupTLS(directory string, c config) error {
	if c.HTTPS.Mode == httpsProvided {
		if _, err := tls.LoadX509KeyPair(filepath.Join(directory, backupCertificate), filepath.Join(directory, backupPrivateKey)); err != nil {
			return errors.New("backed-up TLS certificate and private key do not match")
		}
	}
	if c.HTTPS.CACertificate != "" {
		pem, err := readSmallRegular(filepath.Join(directory, backupCA), true)
		if err != nil || !x509.NewCertPool().AppendCertsFromPEM(pem) {
			return errors.New("backup TLS CA is invalid")
		}
	}
	return nil
}

func pathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+string(os.PathSeparator)) || strings.HasPrefix(b, a+string(os.PathSeparator))
}
