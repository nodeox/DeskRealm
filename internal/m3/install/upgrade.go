package install

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	upgradeName       = "center-upgrade.json"
	upgradeSwitching  = "switching"
	upgradeRollback   = "rolling_back"
	upgradeSucceeded  = "succeeded"
	upgradeRolledBack = "rolled_back"
	upgradeRecovery   = "recovery_required"
)

type upgradeSchema struct {
	Cluster string `json:"postgres_system_identifier"`
	Version int64  `json:"migration_version"`
	SHA     string `json:"schema_sha256"`
}

type centerUpgrade struct {
	Version    int           `json:"version"`
	ID         string        `json:"id"`
	Deployment string        `json:"deployment_id"`
	From       string        `json:"from_image"`
	Target     string        `json:"target_image"`
	BackupSHA  string        `json:"backup_sha256"`
	Schema     upgradeSchema `json:"database"`
	State      string        `json:"state"`
	Detail     string        `json:"detail,omitempty"`
	UpdatedAt  time.Time     `json:"updated_at"`
}

func (u centerUpgrade) finished() bool {
	return u.State == upgradeSucceeded || u.State == upgradeRolledBack
}

func (u centerUpgrade) activeImage() string {
	if u.State == upgradeSucceeded {
		return u.Target
	}
	return u.From
}

func readCenterUpgrade(dir, deployment string) (*centerUpgrade, error) {
	raw, err := readSmallRegular(filepath.Join(dir, upgradeName), true)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot read private center upgrade record")
	}
	return parseCenterUpgrade(raw, deployment)
}

func parseCenterUpgrade(raw []byte, deployment string) (*centerUpgrade, error) {
	var u centerUpgrade
	if json.Unmarshal(raw, &u) != nil || u.Version != 1 || u.ID == "" || u.UpdatedAt.IsZero() ||
		u.Deployment != deployment || !validImage(u.From) || !validImage(u.Target) || u.From == u.Target ||
		!digestHex.MatchString(u.BackupSHA) || !validClusterID(u.Schema.Cluster) || u.Schema.Version < 1 ||
		!digestHex.MatchString(u.Schema.SHA) {
		return nil, errors.New("center upgrade record is invalid")
	}
	switch u.State {
	case upgradeSwitching, upgradeRollback, upgradeSucceeded, upgradeRolledBack, upgradeRecovery:
		return &u, nil
	default:
		return nil, errors.New("unknown center upgrade state")
	}
}

func (u *centerUpgrade) save(dir, state, detail string) error {
	next := *u
	next.State, next.Detail, next.UpdatedAt = state, detail, time.Now().UTC()
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(dir, upgradeName, raw, 0600); err != nil {
		return err
	}
	*u = next
	return nil
}

func centerImageName(image string) string {
	return "center-image-" + sha256Hex([]byte(image)) + ".json"
}

func centerWithImage(c config, center publicDeployment, image string, create bool) (publicDeployment, error) {
	if !validImage(image) || c.Offline != (offlineConfig{}) {
		return publicDeployment{}, errors.New("center upgrade requires an online digest-pinned deployment")
	}
	center.centerImage, center.centerImageFile = "", ""
	if image == c.Images.Center {
		return center, nil
	}
	raw, err := json.Marshal(map[string]any{"services": map[string]any{"center": map[string]string{"image": image}}})
	if err != nil {
		return publicDeployment{}, err
	}
	name := centerImageName(image)
	path := filepath.Join(c.Directory, name)
	existing, err := readSmallRegular(path, true)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := writeAtomic(c.Directory, name, raw, 0600); err != nil {
			return publicDeployment{}, err
		}
		existing, err = readSmallRegular(path, true)
	}
	if err != nil || !bytes.Equal(existing, raw) {
		return publicDeployment{}, errors.New("center image override is missing or changed")
	}
	center.centerImage, center.centerImageFile = image, path
	return center, nil
}

func centerUpgradeStatus(c config) (*centerUpgrade, error) {
	center, err := readPreparedBase(c)
	if err != nil {
		return nil, err
	}
	record, err := readCenterUpgrade(c.Directory, center.ID)
	if err == nil && record == nil {
		return nil, errors.New("no center upgrade has been recorded")
	}
	return record, err
}

func upgradeCenter(ctx context.Context, c config, target, recovery string, retry bool, run dockerRunner) (*centerUpgrade, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if !validImage(target) || c.Offline != (offlineConfig{}) {
		return nil, errors.New("upgrade requires a digest-pinned center image and an online deployment")
	}
	lock, err := lockDeployment(c.Directory)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	center, err := readPreparedBase(c)
	if err != nil {
		return nil, err
	}
	current, err := readCenterUpgrade(c.Directory, center.ID)
	if err != nil {
		return nil, err
	}
	if current != nil {
		switch {
		case current.State == upgradeRecovery:
			return current, errors.New("database compatibility is unresolved; use the verified backup recovery procedure")
		case !current.finished():
			if current.Target != target {
				return current, errors.New("resume the existing center upgrade with its original target")
			}
			return advanceCenterUpgrade(ctx, c, center, current, run, waitForCenter)
		case current.Target == target && (current.State == upgradeSucceeded || !retry):
			if current.State == upgradeRolledBack {
				return current, errors.New("previous attempt rolled back; use --retry to begin a new attempt")
			}
			return current, nil
		}
	}
	active := c.Images.Center
	if current != nil {
		active = current.activeImage()
	}
	if active == target {
		return current, errors.New("target center image is already active")
	}
	cluster, err := readDatabaseRecord(c.Directory, center.ID)
	if err != nil || cluster == "" {
		return nil, errors.New("center upgrade requires the original database installation record")
	}
	platform, _, err := checkRuntime(ctx, run)
	if err != nil {
		return nil, err
	}
	from, err := centerWithImage(c, center, active, false)
	if err != nil {
		return nil, err
	}
	if _, err := waitForCenter(ctx, c, from, run); err != nil {
		return nil, errors.New("existing center is not healthy")
	}
	schema, err := inspectUpgradeSchema(ctx, from, run)
	if err != nil || schema.Cluster != cluster {
		return nil, errors.New("running PostgreSQL does not match the original deployment and schema")
	}
	recovered, err := verifyUpgradeRecovery(ctx, c, center, schema, active, recovery, run)
	if err != nil {
		return nil, err
	}
	if err := ensurePlatformImage(ctx, active, platform, run); err != nil {
		return nil, fmt.Errorf("rollback image: %w", err)
	}
	if err := ensurePlatformImage(ctx, target, platform, run); err != nil {
		return nil, err
	}
	data, err := run(ctx, "run", "--rm", "--pull", "never", "--network", "none", "--read-only",
		"--entrypoint", "/usr/local/bin/deskrealm", target, "schema-version")
	if err != nil || strings.TrimSpace(string(data)) != strconv.FormatInt(schema.Version, 10) {
		return nil, errors.New("candidate does not declare the running schema version; use a schema migration procedure")
	}
	if _, err := centerWithImage(c, center, target, true); err != nil {
		return nil, err
	}
	next := &centerUpgrade{Version: 1, ID: rand.Text(), Deployment: center.ID, From: active, Target: target,
		BackupSHA: recovered.BackupSHA, Schema: schema}
	if err := next.save(c.Directory, upgradeSwitching, ""); err != nil {
		return nil, err
	}
	return advanceCenterUpgrade(ctx, c, center, next, run, waitForCenter)
}

type upgradeHealth func(context.Context, config, publicDeployment, dockerRunner) (centerStatus, error)

func advanceCenterUpgrade(ctx context.Context, c config, center publicDeployment, u *centerUpgrade, run dockerRunner,
	health upgradeHealth) (*centerUpgrade, error) {
	from, err := centerWithImage(c, center, u.From, false)
	if err != nil {
		return u, err
	}
	to, err := centerWithImage(c, center, u.Target, false)
	if err != nil {
		return u, err
	}
	check := func(instance publicDeployment) error {
		current, err := inspectUpgradeSchema(ctx, instance, run)
		if err != nil || current != u.Schema {
			return errors.New("database identity or schema differs from the pre-upgrade snapshot")
		}
		return nil
	}
	requireRecovery := func(reason string) (*centerUpgrade, error) {
		if _, err := composeCommand(ctx, run, to, "stop", "center"); err != nil {
			reason += "; center shutdown could not be confirmed"
		}
		if err := u.save(c.Directory, upgradeRecovery, reason); err != nil {
			return u, err
		}
		return u, errors.New(reason)
	}
	if err := check(from); err != nil {
		if ctx.Err() != nil {
			return u, ctx.Err()
		}
		return requireRecovery("database compatibility could not be confirmed")
	}
	if u.State == upgradeSwitching {
		err := startServices(ctx, run, to, true, "center")
		if err == nil {
			_, err = health(ctx, c, to, run)
		}
		if ctx.Err() != nil {
			return u, ctx.Err()
		}
		if schemaErr := check(to); schemaErr != nil {
			if ctx.Err() != nil {
				return u, ctx.Err()
			}
			return requireRecovery("database compatibility changed during the upgrade; automatic rollback was not attempted")
		}
		if err == nil {
			return u, u.save(c.Directory, upgradeSucceeded, "candidate center healthy on the original database")
		}
		if err := u.save(c.Directory, upgradeRollback, "candidate center failed startup or health checks"); err != nil {
			return u, err
		}
	}
	if err = startServices(ctx, run, from, true, "center"); err == nil {
		_, err = health(ctx, c, from, run)
	}
	if ctx.Err() != nil {
		return u, ctx.Err()
	}
	if err != nil || check(from) != nil {
		if ctx.Err() != nil {
			return u, ctx.Err()
		}
		return requireRecovery("original center did not pass rollback verification; refer to the verified backup")
	}
	if err := u.save(c.Directory, upgradeRolledBack, "original center recovered and database compatibility verified"); err != nil {
		return u, err
	}
	return u, errors.New("candidate center failed; original center was restored")
}

func verifyUpgradeRecovery(ctx context.Context, c config, center publicDeployment, schema upgradeSchema, active, path string,
	run dockerRunner) (restoreResult, error) {
	var report restoreResult
	directory, err := canonicalDirectory(path)
	if err != nil || pathsOverlap(directory, c.Directory) {
		return report, errors.New("upgrade requires a separate verified restore directory")
	}
	raw, err := readSmallRegular(filepath.Join(directory, recoveryReport), true)
	if err != nil || json.Unmarshal(raw, &report) != nil || !digestHex.MatchString(report.BackupSHA) ||
		report.VerifiedAt.IsZero() || report.Directory != directory || report.Config != filepath.Join(directory, recoveryConfig) ||
		report.DeploymentID != center.ID || report.Cluster != schema.Cluster || report.Schema != schema ||
		report.RendezvousSHA != center.RendezvousKeySHA256 ||
		report.SigningSHA != center.SigningKeySHA256 {
		return report, errors.New("restore verification record does not match the source center")
	}
	recovered, err := loadConfig(report.Config, time.Now())
	if err != nil || recovered.Directory != directory {
		return report, errors.New("verified recovery configuration is missing or invalid")
	}
	id, err := readPrepared(recovered)
	if err != nil || id.ID != center.ID || id.RendezvousKey != center.RendezvousKey || id.SigningKey != center.SigningKey {
		return report, errors.New("the recovered deployment identity is invalid")
	}
	recoveredCluster, err := readDatabaseRecord(directory, center.ID)
	image := recovered.Images.Center
	if id.centerImage != "" {
		image = id.centerImage
	}
	if err != nil || recoveredCluster != schema.Cluster || image != active || recovered.Images.PostgreSQL != c.Images.PostgreSQL {
		return report, errors.New("the recovery directory must contain the original cluster and active center image")
	}
	actual, _, err := databaseStorage(ctx, directory, c.Images.PostgreSQL, run)
	if err != nil || actual != schema.Cluster {
		return report, errors.New("verified recovery directory no longer contains the original database cluster")
	}
	return report, nil
}

const upgradeSchemaQuery = `SELECT system_identifier || ':' || (SELECT max(version_id) FROM
		(SELECT DISTINCT ON (version_id) version_id,is_applied FROM public.goose_db_version ORDER BY version_id,id DESC) v
		WHERE is_applied) FROM pg_control_system()`

// The dump is only hashed. A fixed key makes successive schema dumps comparable.
const schemaDumpRestrict = "--restrict-key=DeskRealmSchemaSnapshotV1"

func identifyUpgradeSchema(identity, checksum string) (upgradeSchema, error) {
	cluster, version, ok := strings.Cut(strings.TrimSpace(identity), ":")
	number, err := strconv.ParseInt(version, 10, 64)
	if !ok || !validClusterID(cluster) || err != nil || number < 1 || !digestHex.MatchString(checksum) {
		return upgradeSchema{}, errors.New("cannot identify the database schema")
	}
	return upgradeSchema{Cluster: cluster, Version: number, SHA: checksum}, nil
}

func inspectUpgradeSchema(ctx context.Context, center publicDeployment, run dockerRunner) (upgradeSchema, error) {
	db := []string{"exec", "-T", "--user", "postgres", "database"}
	identity, err := composeCommand(ctx, run, center, append(db, "psql", "-XAt", "--no-password", "-U", "postgres", "-d", "deskrealm",
		"-c", upgradeSchemaQuery)...)
	if err != nil {
		return upgradeSchema{}, errors.New("cannot identify the running database schema")
	}
	dump, err := composeCommand(ctx, run, center, append(db, "pg_dump", "--schema-only", "--no-password", "-U", "postgres", "-d", "deskrealm",
		schemaDumpRestrict)...)
	if err != nil || len(dump) == 0 {
		return upgradeSchema{}, errors.New("cannot capture database schema")
	}
	return identifyUpgradeSchema(string(identity), sha256Hex(dump))
}

func ensurePlatformImage(ctx context.Context, image, platform string, run dockerRunner) error {
	args := []string{"image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", image}
	out, err := run(ctx, args...)
	if err == nil && strings.TrimSpace(string(out)) == platform {
		return nil
	}
	if _, err := run(ctx, "pull", "--platform", platform, image); err != nil {
		return fmt.Errorf("cannot pull the configured digest for %s", platform)
	}
	out, err = run(ctx, args...)
	if err != nil || strings.TrimSpace(string(out)) != platform {
		return fmt.Errorf("configured digest is unavailable for %s", platform)
	}
	return nil
}
