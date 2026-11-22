package m0

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const image = "docker.io/rustdesk/rustdesk-server:1.1.16@sha256:8ecdab65deb7c84652a626380e31d11a8f1fbafd97916d57f95c20628f943c00"

type publishedPort struct {
	Target    int    `json:"target"`
	Published string `json:"published"`
	Protocol  string `json:"protocol"`
}

type bindVolume struct {
	Type   string `json:"type"`
	Source string `json:"source"`
	Target string `json:"target"`
	Bind   struct {
		CreateHostPath bool `json:"create_host_path"`
	} `json:"bind"`
}

type service struct {
	Image      string          `json:"image"`
	Command    []string        `json:"command"`
	WorkingDir string          `json:"working_dir"`
	Ports      []publishedPort `json:"ports"`
	Volumes    []bindVolume    `json:"volumes,omitempty"`
	Restart    string          `json:"restart"`
}

type composeModel struct {
	Services map[string]service `json:"services"`
}

func expectedService(d deployment) (string, service) {
	s := service{Image: image, WorkingDir: "/root", Restart: "unless-stopped"}
	if d.Role == center {
		s.Command = []string{"hbbs", "-k", "_", "-r", d.RelayEndpoint}
		s.Ports = []publishedPort{{21115, "21115", "tcp"}, {21116, "21116", "tcp"}, {21116, "21116", "udp"}}
		s.Volumes = []bindVolume{{Type: "bind", Source: filepath.Join(d.Directory, dataDir), Target: "/root"}}
		return "hbbs", s
	}
	s.Command = []string{"hbbr", "-k", d.PublicKey}
	s.Ports = []publishedPort{{21117, "21117", "tcp"}}
	return "hbbr", s
}

func composeJSON(d deployment) ([]byte, error) {
	name, s := expectedService(d)
	return json.MarshalIndent(composeModel{Services: map[string]service{name: s}}, "", "  ")
}

type runner func(context.Context, ...string) ([]byte, error)

func docker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "COMPOSE_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if failure, ok := err.(*exec.ExitError); ok && len(failure.Stderr) != 0 {
			return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(failure.Stderr)))
		}
		return nil, fmt.Errorf("docker %s: %w", args[0], err)
	}
	return out, nil
}

func (d deployment) compose(ctx context.Context, run runner, args ...string) ([]byte, error) {
	scope := []string{"compose", "-p", d.Project, "-f", filepath.Join(d.Directory, composeFile),
		"--project-directory", d.Directory, "--env-file", os.DevNull}
	return run(ctx, append(scope, args...)...)
}
