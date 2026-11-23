package install

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

//go:embed docker-release.asc
var dockerReleaseKey []byte

const dockerKeySHA256 = "1500c1f56fa9e26b9b8f42452a553675796ade0807cdce11975eb98170b3a570"
const runtimeDirectory = "/var/lib/deskrealm-runtime"

type runtimePackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type runtimePlan struct {
	Version  int              `json:"version"`
	Distro   string           `json:"distribution"`
	Release  string           `json:"release"`
	Suite    string           `json:"suite"`
	Arch     string           `json:"architecture"`
	Source   string           `json:"repository"`
	Key      string           `json:"apt_key_sha256"`
	Packages []runtimePackage `json:"packages"`
}

type runtimeResult struct {
	Mode     string `json:"mode"`
	Platform string `json:"docker_platform"`
	Engine   string `json:"docker_engine_version"`
	Compose  string `json:"compose_version"`
}

func planRuntime(osRelease []byte, arch string) (runtimePlan, error) {
	values := make(map[string]string)
	for _, line := range strings.Split(string(osRelease), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found {
			values[key] = strings.Trim(value, `"'`)
		}
	}
	id, release, suite := values["ID"], values["VERSION_ID"], values["VERSION_CODENAME"]
	if (id != "ubuntu" || release != "24.04" || suite != "noble") &&
		(id != "debian" || release != "12" || suite != "bookworm") {
		return runtimePlan{}, errors.New("automatic Docker preparation supports Ubuntu 24.04 and Debian 12")
	}
	if arch != "amd64" && arch != "arm64" {
		return runtimePlan{}, errors.New("automatic Docker preparation supports amd64 and arm64")
	}
	suffix := "~" + id + "." + release + "~" + suite
	return runtimePlan{
		Version: 1, Distro: id, Release: release, Suite: suite, Arch: arch,
		Source: "https://download.docker.com/linux/" + id, Key: dockerKeySHA256,
		Packages: []runtimePackage{
			{"docker-ce", "5:29.9.3-1" + suffix}, {"docker-ce-cli", "5:29.9.3-1" + suffix},
			{"containerd.io", "2.4.2-4" + suffix}, {"docker-compose-plugin", "5.8.0-1" + suffix},
		},
	}, nil
}

func hostRuntimePlan() (runtimePlan, error) {
	if runtime.GOOS != "linux" {
		return runtimePlan{}, errors.New("automatic Docker preparation requires a supported Linux host")
	}
	release, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return runtimePlan{}, errors.New("cannot identify the Linux distribution from /etc/os-release")
	}
	return planRuntime(release, runtime.GOARCH)
}

func runtimeCommand(ctx context.Context, directory, program string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = directory
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "APT_CONFIG=") && !strings.HasPrefix(env, "DEBIAN_FRONTEND=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "DEBIAN_FRONTEND=noninteractive")
	return cmd.Output()
}

func compatibleRuntime(ctx context.Context, plan runtimePlan, run dockerRunner) (runtimeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	platform, compose, err := checkRuntime(ctx, run)
	if err != nil {
		return runtimeResult{}, err
	}
	if platform != "linux/"+plan.Arch {
		return runtimeResult{}, errors.New("Docker Engine architecture differs from the Linux host")
	}
	flags, err := run(ctx, "compose", "up", "--help")
	if err != nil || !bytes.Contains(flags, []byte("--wait-timeout")) || !bytes.Contains(flags, []byte("--pull")) {
		return runtimeResult{}, errors.New("Docker Compose lacks the required startup and pull options")
	}
	version, err := run(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil || strings.TrimSpace(string(version)) == "" {
		return runtimeResult{}, errors.New("cannot determine local Docker Engine version")
	}
	return runtimeResult{Mode: "reused", Platform: platform, Engine: strings.TrimSpace(string(version)), Compose: compose}, nil
}

func prepareRuntime(ctx context.Context, plan runtimePlan) (runtimeResult, error) {
	if result, err := compatibleRuntime(ctx, plan, docker); err == nil {
		return result, nil
	}
	if os.Geteuid() != 0 {
		return runtimeResult{}, errors.New("installing Docker on a fresh host requires root")
	}
	if os.Getenv("DOCKER_HOST") != "" || os.Getenv("DOCKER_CONTEXT") != "" {
		return runtimeResult{}, errors.New("fresh Docker preparation requires the default local system context")
	}
	if value, err := runtimeCommand(ctx, "", "systemctl", "show", "--property=Version", "--value"); err != nil || len(bytes.TrimSpace(value)) == 0 {
		return runtimeResult{}, errors.New("fresh Docker preparation requires an active systemd host")
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	marker := filepath.Join(runtimeDirectory, "selection.json")
	selection, err := json.Marshal(plan)
	if err != nil {
		return runtimeResult{}, err
	}
	recorded, err := readSmallRegular(marker, true)
	if errors.Is(err, os.ErrNotExist) {
		if err := refuseExistingRuntime(ctx); err != nil {
			return runtimeResult{}, err
		}
		if err := os.Mkdir(runtimeDirectory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return runtimeResult{}, err
		}
	} else if err != nil || !bytes.Equal(recorded, selection) {
		return runtimeResult{}, errors.New("existing Docker preparation record differs from this pinned selection")
	}
	info, err := os.Lstat(runtimeDirectory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return runtimeResult{}, errors.New("unsafe Docker preparation state directory")
	}
	lock, err := lockDeployment(runtimeDirectory)
	if err != nil {
		return runtimeResult{}, err
	}
	defer lock.Close()
	current, err := readSmallRegular(marker, true)
	if errors.Is(err, os.ErrNotExist) {
		entries, err := os.ReadDir(runtimeDirectory)
		if err != nil || len(entries) != 0 {
			return runtimeResult{}, errors.New("Docker preparation state exists without a valid pinned selection")
		}
		if err := writeAtomic(runtimeDirectory, "selection.json", selection, 0600); err != nil {
			return runtimeResult{}, err
		}
	} else if err != nil || !bytes.Equal(current, selection) {
		return runtimeResult{}, errors.New("existing Docker preparation record differs from this pinned selection")
	}
	if err := installRuntimePackages(ctx, plan); err != nil {
		return runtimeResult{}, err
	}
	if _, err := runtimeCommand(ctx, "", "systemctl", "enable", "--now", "docker.service"); err != nil {
		return runtimeResult{}, errors.New("installed Docker packages, but systemd could not start docker.service")
	}
	systemDocker := func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "context" {
			return []byte("unix:///var/run/docker.sock"), nil
		}
		return docker(ctx, append([]string{"--host", "unix:///var/run/docker.sock"}, args...)...)
	}
	result, err := compatibleRuntime(ctx, plan, systemDocker)
	if err != nil {
		return runtimeResult{}, err
	}
	result.Mode = "installed"
	return result, nil
}

func refuseExistingRuntime(ctx context.Context) error {
	for _, program := range []string{"docker", "dockerd", "containerd", "runc"} {
		if _, err := exec.LookPath(program); err == nil {
			return errors.New("an existing runtime is unavailable or incompatible; resolve it before fresh Docker preparation")
		}
	}
	for _, directory := range []string{"/var/lib/docker", "/var/lib/containerd", "/etc/docker"} {
		entries, err := os.ReadDir(directory)
		if (err == nil && len(entries) > 0) || (err != nil && !errors.Is(err, os.ErrNotExist)) {
			return errors.New("existing runtime state found; fresh Docker preparation stopped")
		}
	}
	// Detect packages whose binaries are absent or whose configuration was interrupted.
	data, _ := runtimeCommand(ctx, "", "dpkg-query", "-W", "-f=${Status}\n", "docker-ce", "docker-ce-cli",
		"docker.io", "docker-compose", "docker-compose-v2", "docker-compose-plugin", "podman-docker", "containerd", "containerd.io", "runc")
	for _, status := range strings.FieldsFunc(string(data), func(r rune) bool { return r == '\n' }) {
		if !strings.HasSuffix(status, " not-installed") && !strings.HasSuffix(status, " config-files") {
			return errors.New("existing runtime packages found; fresh Docker preparation stopped")
		}
	}
	return nil
}

func installRuntimePackages(ctx context.Context, plan runtimePlan) error {
	if sha256Hex(dockerReleaseKey) != dockerKeySHA256 {
		return errors.New("embedded Docker APT signing key fails its digest pin")
	}
	scratch, err := os.MkdirTemp("/var/tmp", "deskrealm-docker-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	if err := os.Chmod(scratch, 0755); err != nil {
		return err
	}
	key, source, lists := filepath.Join(scratch, "docker.asc"), filepath.Join(scratch, "docker.sources"), filepath.Join(scratch, "lists")
	if err := os.Mkdir(lists, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(key, dockerReleaseKey, 0644); err != nil {
		return err
	}
	spec := fmt.Sprintf("Types: deb\nURIs: %s\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: %s\n", plan.Source, plan.Suite, plan.Arch, key)
	if err := os.WriteFile(source, []byte(spec), 0644); err != nil {
		return err
	}
	runAPT := func(step string, args ...string) error {
		common := []string{"-o", "APT::Get::AllowUnauthenticated=false", "-o", "Acquire::AllowInsecureRepositories=false"}
		if _, err := runtimeCommand(ctx, scratch, "apt-get", append(common, args...)...); err != nil {
			return fmt.Errorf("Docker preparation failed at %s", step)
		}
		return nil
	}
	if err := runAPT("distribution metadata", "-o", "APT::Update::Error-Mode=any", "update"); err != nil {
		return err
	}
	install := []string{"install", "--yes", "--no-install-recommends", "--no-remove"}
	if err := runAPT("distribution CA certificates", append(install, "ca-certificates")...); err != nil {
		return err
	}
	isolated := []string{"-o", "Dir::Etc::sourcelist=" + source, "-o", "Dir::Etc::sourceparts=-", "-o", "Dir::State::lists=" + lists,
		"-o", "Dir::Cache::pkgcache=", "-o", "Dir::Cache::srcpkgcache=", "-o", "APT::Update::Error-Mode=any"}
	if err := runAPT("authenticated Docker repository metadata", append(isolated, "update")...); err != nil {
		return err
	}
	args := append(isolated, "download")
	for _, p := range plan.Packages {
		args = append(args, p.Name+":"+plan.Arch+"="+p.Version)
	}
	if err := runAPT("pinned Docker package downloads", args...); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(scratch, "*.deb"))
	if err != nil || len(files) != len(plan.Packages) {
		return errors.New("Docker repository did not return the complete pinned package set")
	}
	remaining := make(map[string]string, len(plan.Packages))
	for _, p := range plan.Packages {
		remaining[p.Name] = p.Version
	}
	for _, file := range files {
		output, err := runtimeCommand(ctx, scratch, "dpkg-deb", "--show", "--showformat=${Package}\t${Version}\t${Architecture}\n", file)
		fields := strings.Split(strings.TrimSpace(string(output)), "\t")
		if err != nil || len(fields) != 3 || remaining[fields[0]] != fields[1] || fields[2] != plan.Arch {
			return errors.New("downloaded Docker package identity, version or architecture does not match the pin")
		}
		delete(remaining, fields[0])
	}
	if err := runAPT("installation of verified Docker packages", append(install, files...)...); err != nil {
		return err
	}
	for _, p := range plan.Packages {
		output, err := runtimeCommand(ctx, "", "dpkg-query", "-W", "-f=${Status}\n${Version}", p.Name)
		if err != nil || string(output) != "install ok installed\n"+p.Version {
			return fmt.Errorf("installed Docker package differs from the pin for %s", p.Name)
		}
	}
	return nil
}
