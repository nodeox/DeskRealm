package install

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const databaseRecord = "database-ready"

func composeCommand(ctx context.Context, run dockerRunner, center publicDeployment, args ...string) ([]byte, error) {
	command := []string{"compose", "--env-file", "/dev/null", "--project-name", center.Project, "--file", center.ComposeFile}
	return run(ctx, append(command, args...)...)
}

func lockDeployment(directory string) (*os.File, error) {
	dir, err := os.Open(directory)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		dir.Close()
		return nil, errors.New("cannot lock deployment; another installer may be using it")
	}
	return dir, nil
}

func installCenter(ctx context.Context, c config, run dockerRunner) (centerStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	runtimeCtx, stop := context.WithTimeout(ctx, 20*time.Second)
	platform, _, err := checkRuntime(runtimeCtx, run)
	stop()
	if err != nil {
		return centerStatus{}, err
	}
	if _, err := prepareDeployment(c); err != nil {
		return centerStatus{}, err
	}
	lock, err := lockDeployment(c.Directory)
	if err != nil {
		return centerStatus{}, err
	}
	defer lock.Close()
	center, err := readPrepared(c)
	if err != nil {
		return centerStatus{}, err
	}
	recordedCluster, err := readDatabaseRecord(c.Directory, center.ID)
	if err != nil {
		return centerStatus{}, err
	}
	services, err := runningServices(ctx, c, center, run)
	if err != nil {
		return centerStatus{}, err
	}
	if err := ensureImages(ctx, c, center, platform, recordedCluster != "", run); err != nil {
		return centerStatus{}, err
	}
	postgresImage := center.runtimeImages(c.Images).PostgreSQL
	cluster, empty, err := databaseStorage(ctx, c.Directory, postgresImage, run)
	if err != nil {
		return centerStatus{}, err
	}
	// A database that has already hosted the schema must never be recreated
	// automatically after its bind directory has been emptied or replaced.
	if cluster == "" && (!empty || recordedCluster != "" || services["database"].State == "running" ||
		services["migrate"].State != "" || services["center"].State != "") {
		return centerStatus{}, errors.New("PostgreSQL data is missing or incomplete; restore the original database before starting services")
	}
	if recordedCluster != "" && cluster != recordedCluster {
		return centerStatus{}, errors.New("PostgreSQL data belongs to a different cluster; restore the original database before starting services")
	}
	if !serviceReady("database", services["database"]) {
		if err := startServices(ctx, run, center, "database"); err != nil {
			return centerStatus{}, err
		}
		actual, _, err := databaseStorage(ctx, c.Directory, postgresImage, run)
		if err != nil || actual == "" || (cluster != "" && cluster != actual) {
			return centerStatus{}, errors.New("PostgreSQL started without a verified persistent data directory")
		}
		cluster = actual
	}
	if recordedCluster == "" {
		if err := recordDatabase(c.Directory, center.ID, cluster); err != nil {
			return centerStatus{}, fmt.Errorf("record PostgreSQL initialization: %w", err)
		}
	}
	if step, _ := serviceHealth(services); step != "" {
		if err := startServices(ctx, run, center); err != nil {
			return centerStatus{}, err
		}
	}
	return waitForCenter(ctx, c, center, run)
}

func startServices(ctx context.Context, run dockerRunner, center publicDeployment, service ...string) error {
	args := []string{"up", "--detach", "--no-build", "--pull", "never", "--wait", "--wait-timeout", "90"}
	_, err := composeCommand(ctx, run, center, append(args, service...)...)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(service) == 1 {
		return errors.New("PostgreSQL startup failed; check the database container and its persistent bind")
	}
	return errors.New("center service startup failed; use status to identify the failed migration, center or rendezvous service")
}

func ensureImages(ctx context.Context, c config, center publicDeployment, platform string, activated bool, run dockerRunner) error {
	if center.Offline != (offlineIdentity{}) {
		return ensureOfflineImages(ctx, c, center.Offline, platform, activated, run)
	}
	images := c.Images
	for _, image := range []struct{ service, ref string }{
		{"database", images.PostgreSQL}, {"center", images.Center}, {"rendezvous", images.Rendezvous},
	} {
		inspect := []string{"image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", image.ref}
		actual, err := run(ctx, inspect...)
		if err == nil && strings.TrimSpace(string(actual)) == platform {
			continue
		}
		if _, err := run(ctx, "pull", "--platform", platform, image.ref); err != nil {
			return fmt.Errorf("%s image: cannot pull the configured digest for %s", image.service, platform)
		}
		actual, err = run(ctx, inspect...)
		if err != nil || strings.TrimSpace(string(actual)) != platform {
			return fmt.Errorf("%s image: configured digest is unavailable for %s", image.service, platform)
		}
	}
	return nil
}

func readDatabaseRecord(directory, id string) (string, error) {
	path := filepath.Join(directory, databaseRecord)
	data, err := readSmallRegular(path, true)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	record, newline := strings.CutSuffix(string(data), "\n")
	cluster, prefix := strings.CutPrefix(record, "1 "+id+" ")
	if err != nil || !newline || !prefix || !validClusterID(cluster) {
		return "", errors.New("PostgreSQL initialization record is invalid; restore the original deployment record")
	}
	return cluster, nil
}

func validClusterID(value string) bool {
	number, err := strconv.ParseUint(value, 10, 64)
	return err == nil && number != 0 && strconv.FormatUint(number, 10) == value
}

func recordDatabase(directory, id, cluster string) error {
	stage, err := os.MkdirTemp(directory, ".database-ready-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	file := filepath.Join(stage, databaseRecord)
	if err := writeSynced(file, []byte("1 "+id+" "+cluster+"\n"), 0600); err != nil {
		return err
	}
	if err := os.Rename(file, filepath.Join(directory, databaseRecord)); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func databaseStorage(ctx context.Context, directory, image string, run dockerRunner) (cluster string, empty bool, err error) {
	var value strings.Builder
	writer := csv.NewWriter(&value)
	if err := writer.Write([]string{"type=bind", "source=" + filepath.Join(directory, "postgres"),
		"target=/var/lib/postgresql", "readonly"}); err != nil {
		return "", false, err
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return "", false, err
	}
	data, err := run(ctx, "run", "--rm", "--pull", "never", "--network", "none", "--read-only",
		"--user", "0:0", "--entrypoint", "/bin/sh", "--mount", strings.TrimSuffix(value.String(), "\n"),
		image, "-ec", `
if [ "${PGDATA:-}" != /var/lib/postgresql/18/docker ]; then exit 1; fi
if [ -f "$PGDATA/PG_VERSION" ] && [ "$(cat "$PGDATA/PG_VERSION")" = 18 ] && [ -s "$PGDATA/global/pg_control" ]; then
  report="$(LC_ALL=C pg_controldata "$PGDATA")"
  printf '%s\n' "$report" | sed -n 's/^Database system identifier:[[:space:]]*//p'
elif [ -z "$(find /var/lib/postgresql -mindepth 1 -print -quit)" ]; then
  printf empty
else
  printf incomplete
fi`)
	if err != nil {
		return "", false, errors.New("cannot inspect persistent storage with the configured PostgreSQL 18 image")
	}
	state := strings.TrimSpace(string(data))
	switch state {
	case "empty":
		return "", true, nil
	case "incomplete":
		return "", false, nil
	}
	if !validClusterID(state) {
		return "", false, errors.New("cannot verify PostgreSQL cluster identity")
	}
	return state, false, nil
}

func issueBootstrapTicket(ctx context.Context, c config, center publicDeployment, run dockerRunner) (string, error) {
	lock, err := lockDeployment(c.Directory)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	// Read the bootstrap state from the same HTTPS origin that will redeem the link.
	status, err := inspectCenter(ctx, c, center, run)
	if err != nil {
		return "", err
	}
	if status.Phase != "ready" {
		return "", fmt.Errorf("%s: %s", status.Phase, status.Detail)
	}
	if status.Initialized != nil && *status.Initialized {
		return "", errors.New("the center is already initialized")
	}
	data, err := composeCommand(ctx, run, center, "exec", "-T", "center", "/usr/local/bin/deskrealm", "bootstrap-ticket")
	if err != nil {
		return "", errors.New("cannot issue an initialization ticket from the running center")
	}
	link := strings.TrimSpace(string(data))
	token, ok := strings.CutPrefix(link, c.PublicOrigin+"/initialize#ticket=")
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if !ok || len(token) != 43 || err != nil || len(raw) != 32 {
		return "", errors.New("the center returned an invalid initialization link")
	}
	return link, nil
}
