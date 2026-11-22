package m0

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	databaseFile = "db_v2.sqlite3"
	readyFile    = ".initialized"
	sqliteHeader = "SQLite format 3\x00"
)

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type container struct {
	ID     string `json:"Id"`
	Config struct {
		Image      string
		Cmd        []string
		Entrypoint []string
		WorkingDir string
		Labels     map[string]string
	}
	HostConfig struct {
		PortBindings map[string][]portBinding
	}
	NetworkSettings struct {
		Ports map[string][]portBinding
	}
	State struct {
		Running bool
	}
	Mounts []struct {
		Type        string
		Source      string
		Destination string
		RW          bool
	}
}

type status struct {
	deployment
	Image         string `json:"image"`
	ContainerID   string `json:"container_id,omitempty"`
	CenterBind    string `json:"center_bind,omitempty"`
	Running       bool   `json:"running"`
	Ready         bool   `json:"ready"`
	DatabaseReady *bool  `json:"database_ready,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

var dockerID = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

func (d deployment) inspect(ctx context.Context, run runner) (status, error) {
	out := status{deployment: d, Image: image}
	name, model := expectedService(d)
	raw, err := d.compose(ctx, run, "ps", "-a", "-q", name)
	if err != nil {
		return out, err
	}
	ids := strings.Fields(string(raw))
	if len(ids) == 0 {
		out.Detail = "no container in this deployment"
		return out, nil
	}
	if len(ids) != 1 || !dockerID.MatchString(ids[0]) {
		return out, errors.New("expected one Docker container ID for this deployment")
	}
	document, err := run(ctx, "inspect", "--type", "container", ids[0])
	if err != nil {
		return out, err
	}
	var rows []container
	if err := json.Unmarshal(document, &rows); err != nil || len(rows) != 1 {
		return out, errors.New("invalid Docker inspect response")
	}
	item := rows[0]
	if !dockerID.MatchString(item.ID) || !strings.HasPrefix(item.ID, ids[0]) {
		return out, errors.New("Docker inspect returned a different container ID")
	}
	labels := item.Config.Labels
	if labels["com.docker.compose.project"] != d.Project ||
		filepath.Clean(labels["com.docker.compose.project.working_dir"]) != d.Directory ||
		labels["com.docker.compose.service"] != name {
		return out, errors.New("container does not belong to this deployment")
	}
	if item.Config.Image != model.Image || !slices.Equal(item.Config.Cmd, model.Command) ||
		item.Config.WorkingDir != model.WorkingDir || len(item.Config.Entrypoint) != 0 ||
		!matchPorts(item.HostConfig.PortBindings, model.Ports, false) {
		return out, errors.New("container image, command, working directory or port configuration differs")
	}
	if len(item.Mounts) != len(model.Volumes) {
		return out, errors.New("container mounts differ from deployment")
	}
	if d.Role == center {
		mount := item.Mounts[0]
		expected := model.Volumes[0]
		if mount.Type != expected.Type || filepath.Clean(mount.Source) != expected.Source ||
			mount.Destination != expected.Target || !mount.RW {
			return out, errors.New("center container bind does not match the persisted data directory")
		}
		out.CenterBind = mount.Source
	}
	out.ContainerID, out.Running = item.ID, item.State.Running
	if !out.Running {
		out.Detail = "container is not running"
		return out, nil
	}
	if !matchPorts(item.NetworkSettings.Ports, model.Ports, true) {
		return out, errors.New("running container has unexpected published ports")
	}
	if d.Role == center {
		ready, err := d.databaseReady()
		if err != nil {
			return out, err
		}
		out.DatabaseReady = &ready
		if !ready {
			out.Detail = "center SQLite database is not ready"
			return out, nil
		}
		if err := validateIdentity(d); err != nil {
			return out, err
		}
	}
	out.Ready = true
	return out, nil
}

func matchPorts(actual map[string][]portBinding, expected []publishedPort, running bool) bool {
	for _, port := range expected {
		bindings := actual[fmt.Sprintf("%d/%s", port.Target, port.Protocol)]
		if len(bindings) == 0 || (!running && len(bindings) != 1) {
			return false
		}
		for _, b := range bindings {
			if b.HostPort != port.Published || (b.HostIP != "" && b.HostIP != "0.0.0.0" && b.HostIP != "::") {
				return false
			}
		}
	}
	bound := 0
	for _, bindings := range actual {
		if len(bindings) != 0 {
			bound++
		}
	}
	return bound == len(expected)
}

func (d deployment) databaseReady() (bool, error) {
	path := filepath.Join(d.Directory, dataDir, databaseFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("center database %s must be a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	header := make([]byte, len(sqliteHeader))
	if _, err := io.ReadFull(file, header); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	return bytes.Equal(header, []byte(sqliteHeader)), nil
}

func (d deployment) protectDatabase() error {
	if d.Role != center {
		return nil
	}
	raw, err := readRegular(filepath.Join(d.Directory, readyFile), true)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(raw) != d.Fingerprint {
		return errors.New("center initialization marker does not match the original identity")
	}
	ready, err := d.databaseReady()
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("restore the original center database in %s before continuing", filepath.Join(d.Directory, dataDir))
	}
	return nil
}

func (d deployment) recordReady() error {
	if d.Role != center {
		return nil
	}
	file, err := os.CreateTemp(d.Directory, ".initialized-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := io.WriteString(file, d.Fingerprint)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(d.Directory, readyFile))
}

func (d deployment) lifecycle(ctx context.Context, run runner, action string) (status, error) {
	if err := d.protectDatabase(); err != nil {
		return status{}, err
	}
	before, err := d.inspect(ctx, run)
	if err != nil || action == "status" {
		return before, err
	}
	name, _ := expectedService(d)
	args := []string{"up", "-d"}
	if action == "recreate" {
		if d.Role != center || !before.Ready {
			return before, errors.New("recreate requires a running center with its original database")
		}
		args = append(args, "--force-recreate")
	}
	if _, err := d.compose(ctx, run, append(args, name)...); err != nil {
		return before, err
	}
	check, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		after, err := d.inspect(check, run)
		if err != nil {
			return after, err
		}
		if after.Ready {
			if action == "recreate" && before.ContainerID == after.ContainerID {
				return after, errors.New("center container ID did not change after recreate")
			}
			return after, d.recordReady()
		}
		select {
		case <-check.Done():
			return after, fmt.Errorf("readiness check: %s: %w", after.Detail, check.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}
