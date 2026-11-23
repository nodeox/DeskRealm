package install

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"deskrealm/internal/m1"
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
	record, err := ensureInstallRecord(c.Directory, center.ID)
	if err != nil {
		return centerStatus{}, fmt.Errorf("load installation task: %w", err)
	}
	recordedCluster, err := readDatabaseRecord(c.Directory, center.ID)
	if err != nil {
		return centerStatus{}, err
	}
	services, err := runningServices(ctx, c, center, run)
	if err != nil {
		return centerStatus{}, err
	}
	if err := record.step(ctx, c.Directory, imagesStep, false, func() error {
		haveVerifiedImages := recordedCluster != "" || !record.Steps[imagesStep].FirstSuccess.IsZero()
		return ensureImages(ctx, c, center, platform, haveVerifiedImages, run)
	}); err != nil {
		return centerStatus{}, err
	}
	if err := record.step(ctx, c.Directory, databaseStep, false, func() error {
		if recordedCluster == "" && !record.Steps[databaseStep].FirstSuccess.IsZero() {
			return errors.New("PostgreSQL initialization record is missing; restore the original deployment record")
		}
		return ensureDatabase(ctx, c, center, recordedCluster, services, run)
	}); err != nil {
		return centerStatus{}, err
	}
	startupNeeded, _ := serviceHealth(services)
	if err := record.step(ctx, c.Directory, servicesStep, startupNeeded != "", func() error {
		return ensureServices(ctx, c, center, run)
	}); err != nil {
		return centerStatus{}, err
	}
	var status centerStatus
	err = record.step(ctx, c.Directory, healthStep, startupNeeded != "", func() error {
		var err error
		status, err = waitForCenter(ctx, c, center, run)
		return err
	})
	return status, err
}

func ensureDatabase(ctx context.Context, c config, center publicDeployment, recordedCluster string, services map[string]containerStatus, run dockerRunner) error {
	postgresImage := center.runtimeImages(c.Images).PostgreSQL
	cluster, empty, err := databaseStorage(ctx, c.Directory, postgresImage, run)
	if err != nil {
		return err
	}
	// A database that has already hosted the schema must never be recreated
	// automatically after its bind directory has been emptied or replaced.
	if cluster == "" && (!empty || recordedCluster != "" || services["database"].State == "running" ||
		services["migrate"].State != "" || services["center"].State != "") {
		return errors.New("PostgreSQL data is missing or incomplete; restore the original database before starting services")
	}
	if recordedCluster != "" && cluster != recordedCluster {
		return errors.New("PostgreSQL data belongs to a different cluster; restore the original database before starting services")
	}
	if !serviceReady("database", services["database"]) {
		if err := startServices(ctx, run, center, false, "database"); err != nil {
			return err
		}
		actual, _, err := databaseStorage(ctx, c.Directory, postgresImage, run)
		if err != nil || actual == "" || (cluster != "" && cluster != actual) {
			return errors.New("PostgreSQL started without a verified persistent data directory")
		}
		cluster = actual
	}
	if recordedCluster == "" {
		if err := recordDatabase(c.Directory, center.ID, cluster); err != nil {
			return fmt.Errorf("record PostgreSQL initialization: %w", err)
		}
	}
	return nil
}

func ensureServices(ctx context.Context, c config, center publicDeployment, run dockerRunner) error {
	services, err := runningServices(ctx, c, center, run)
	if err != nil {
		return err
	}
	if step, _ := serviceHealth(services); step == "" {
		return nil
	}
	if serviceReady("database", services["database"]) && serviceReady("migrate", services["migrate"]) {
		var remaining []string
		for _, service := range []string{"center", "rendezvous"} {
			if !serviceReady(service, services[service]) {
				remaining = append(remaining, service)
			}
		}
		return startServices(ctx, run, center, true, remaining...)
	}
	return startServices(ctx, run, center, false)
}

func startServices(ctx context.Context, run dockerRunner, center publicDeployment, noDependencies bool, service ...string) error {
	args := []string{"up", "--detach", "--no-build", "--pull", "never", "--wait", "--wait-timeout", "90"}
	if noDependencies {
		args = append(args, "--no-deps")
	}
	_, err := composeCommand(ctx, run, center, append(args, service...)...)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(service) == 1 && service[0] == "database" {
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
	return writeAtomic(directory, databaseRecord, []byte("1 "+id+" "+cluster+"\n"), 0600)
}

func writeAtomic(directory, name string, data []byte, mode os.FileMode) error {
	stage, err := os.MkdirTemp(directory, "."+name+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	file := filepath.Join(stage, name)
	if err := writeSynced(file, data, mode); err != nil {
		return err
	}
	if err := os.Rename(file, filepath.Join(directory, name)); err != nil {
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

func importCenterRecord(ctx context.Context, center publicDeployment, actor string, record installRecord, output *json.Encoder) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, _, err := checkRuntime(ctx, docker); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	withRecord := func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stdin = bytes.NewReader(data)
		return cmd.Output()
	}
	reply, err := composeCommand(ctx, withRecord, center, "exec", "-T", "center",
		"/usr/local/bin/deskrealm", "import-install-record", actor)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			for _, reason := range []error{m1.ErrInstallationRecordActor, m1.ErrInstallationRecordConflict, m1.ErrInstallationRecordInvalid} {
				if strings.TrimSpace(string(exitErr.Stderr)) == "deskrealm: "+reason.Error() {
					return reason
				}
			}
		}
		return errors.New("installation record import failed; check that the center is running with the latest migrations")
	}
	var result m1.InstallationImport
	if err := json.Unmarshal(reply, &result); err != nil || result.TaskID != record.TaskID || result.Revision != record.Revision {
		return errors.New("the center returned an invalid installation import result")
	}
	return output.Encode(result)
}
