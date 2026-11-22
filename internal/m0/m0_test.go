package m0

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func invoke(run runner, args ...string) ([]byte, error) {
	var output bytes.Buffer
	err := runCLI(context.Background(), run, args, &output, io.Discard)
	return output.Bytes(), err
}

func initCenterArgs(dir string) []string {
	return []string{"init", "--role", "center", "--dir", dir, "--center-host", "id.example.test", "--relay-host", "relay.example.test"}
}

func requireJSON[T any](t *testing.T, run runner, args ...string) T {
	t.Helper()
	body, err := invoke(run, args...)
	if err != nil {
		t.Fatalf("%s: %v", args[0], err)
	}
	var value T
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode %s output: %v", args[0], err)
	}
	return value
}

func TestInitAndComposeConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "center with spaces")
	relayDir := filepath.Join(t.TempDir(), "relay")
	t.Setenv("COMPOSE_FILE", "/does-not-exist")
	t.Setenv("COMPOSE_PROJECT_NAME", "wrong-project")

	c := requireJSON[deployment](t, docker, initCenterArgs(dir)...)
	r := requireJSON[deployment](t, docker, "init", "--role", "relay", "--dir", relayDir, "--center-key", c.PublicKey)
	if c.Project == r.Project || c.PublicKey != r.PublicKey || c.Fingerprint != r.Fingerprint {
		t.Fatal("center and relay must share public identity and have separate Compose projects")
	}
	if same := requireJSON[deployment](t, docker, initCenterArgs(dir)...); same != c {
		t.Fatal("repeated initialization changed the deployment")
	}
	absDir := c.Directory
	cases := []struct {
		d       deployment
		name    string
		command []string
		ports   []publishedPort
	}{
		{c, "hbbs", []string{"hbbs", "-k", "_", "-r", "relay.example.test:21117"}, []publishedPort{{21115, "21115", "tcp"}, {21116, "21116", "tcp"}, {21116, "21116", "udp"}}},
		{r, "hbbr", []string{"hbbr", "-k", c.PublicKey}, []publishedPort{{21117, "21117", "tcp"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var actual = requireJSON[struct {
				Name     string
				Services map[string]service
			}](t, docker, "config", "--dir", tc.d.Directory)
			s, found := actual.Services[tc.name]
			if actual.Name != tc.d.Project || len(actual.Services) != 1 || !found ||
				s.Image != image || s.WorkingDir != "/root" || !slices.Equal(s.Command, tc.command) || !slices.Equal(s.Ports, tc.ports) {
				t.Fatalf("unexpected Compose model: %#v", actual)
			}
			if tc.name == "hbbs" {
				if len(s.Volumes) != 1 || s.Volumes[0].Type != "bind" || s.Volumes[0].Target != "/root" ||
					s.Volumes[0].Source != filepath.Join(absDir, dataDir) || s.Volumes[0].Bind.CreateHostPath {
					t.Fatalf("persistent bind does not match: %#v", s.Volumes)
				}
			} else if len(s.Volumes) != 0 {
				t.Fatal("relay must start without access to center data")
			}
		})
	}

	t.Run("reject malformed input and private keys", func(t *testing.T) {
		if _, err := invoke(docker, initCenterArgs(filepath.Join(t.TempDir(), "center-$HOME"))...); err == nil {
			t.Fatal("accepted a path with Compose interpolation syntax")
		}
		for _, bad := range []string{"", "https://example.test", "a,b", "a\nb", "a:21116", "[fe80::1%eth0]", "-r"} {
			args := initCenterArgs(dir)
			args[len(args)-3] = bad
			if _, err := invoke(docker, args...); err == nil {
				t.Fatalf("accepted invalid endpoint %q", bad)
			}
		}
		for _, bad := range []string{"", "_", c.PublicKey + "\n", base64.StdEncoding.EncodeToString(make([]byte, 64))} {
			if _, err := invoke(docker, "init", "--role", "relay", "--dir", relayDir, "--center-key", bad); err == nil {
				t.Fatal("accepted an invalid public key")
			}
		}
		if _, err := invoke(docker, "init", "--role", "center", "--dir", dir, "--center-host", "changed.test", "--relay-host", "relay.example.test"); err == nil {
			t.Fatal("changed an existing deployment")
		}
		ip, err := endpoint("[2001:db8::2]", "21116")
		if err != nil || ip != "[2001:db8::2]:21116" {
			t.Fatalf("IPv6 endpoint %q: %v", ip, err)
		}
	})

	t.Run("identity and model mismatches fail closed", func(t *testing.T) {
		for _, name := range []string{filepath.Join(dataDir, privateFile), filepath.Join(dataDir, publicFile), composeFile} {
			path := filepath.Join(absDir, name)
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{initCenterArgs(dir), {"config", "--dir", dir}} {
				if _, err := invoke(docker, args...); err == nil {
					t.Fatalf("%s accepted corrupted %s", args[0], name)
				}
			}
			if err := os.WriteFile(path, saved, 0600); err != nil {
				t.Fatal(err)
			}
		}
		data := filepath.Join(absDir, dataDir)
		if err := os.Rename(data, data+".bak"); err != nil {
			t.Fatal(err)
		}
		if _, err := invoke(docker, initCenterArgs(dir)...); err == nil {
			t.Fatal("accepted missing data directory")
		}
		if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("missing data directory was automatically replaced")
		}
	})

	t.Run("concurrent init publishes one identity", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "new-center")
		a, b := make(chan []byte, 1), make(chan []byte, 1)
		for _, ch := range []chan []byte{a, b} {
			go func(out chan []byte) {
				body, _ := invoke(docker, initCenterArgs(dir)...)
				out <- body
			}(ch)
		}
		first, second := <-a, <-b
		if len(first) == 0 || !bytes.Equal(first, second) {
			t.Fatal("concurrent initialization disagrees about identity")
		}
	})
}

func mockContainer(d deployment, id string, running bool) container {
	name, model := expectedService(d)
	c := container{ID: id}
	c.Config.Image, c.Config.Cmd, c.Config.WorkingDir = model.Image, model.Command, model.WorkingDir
	c.Config.Labels = map[string]string{
		"com.docker.compose.project":             d.Project,
		"com.docker.compose.project.working_dir": d.Directory,
		"com.docker.compose.service":             name,
	}
	c.HostConfig.PortBindings = map[string][]portBinding{}
	c.NetworkSettings.Ports = map[string][]portBinding{}
	for _, p := range model.Ports {
		host := p.Published + "/" + p.Protocol
		c.HostConfig.PortBindings[host] = []portBinding{{HostPort: p.Published}}
		c.NetworkSettings.Ports[host] = []portBinding{{HostIP: "0.0.0.0", HostPort: p.Published}, {HostIP: "::", HostPort: p.Published}}
	}
	if d.Role == center {
		c.Mounts = append(c.Mounts, struct {
			Type        string
			Source      string
			Destination string
			RW          bool
		}{"bind", model.Volumes[0].Source, "/root", true})
	}
	c.State.Running = running
	return c
}

func TestLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "center")
	d := requireJSON[deployment](t, docker, initCenterArgs(dir)...)
	db := filepath.Join(d.Directory, dataDir, databaseFile)
	savedDB := []byte(sqliteHeader + "example SQLite contents")
	name, _ := expectedService(d)
	oldID, newID := strings.Repeat("a", 64), strings.Repeat("b", 64)
	var item container
	var upCount int
	var upErr error
	changeID := true
	run := func(_ context.Context, args ...string) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"inspect", "--type", "container", item.ID}) {
			return json.Marshal([]container{item})
		}
		scope := []string{"compose", "-p", d.Project, "-f", filepath.Join(d.Directory, composeFile),
			"--project-directory", d.Directory, "--env-file", os.DevNull}
		if len(args) < len(scope) || !slices.Equal(args[:len(scope)], scope) {
			t.Fatalf("unexpected Docker scope: %q", args)
		}
		args = args[len(scope):]
		switch {
		case slices.Equal(args, []string{"ps", "-a", "-q", name}):
			return []byte(item.ID + "\n"), nil
		case slices.Equal(args, []string{"up", "-d", name}):
			upCount++
			item = mockContainer(d, oldID, true)
			return nil, os.WriteFile(db, savedDB, 0600)
		case slices.Equal(args, []string{"up", "-d", "--force-recreate", name}):
			upCount++
			if upErr != nil {
				return nil, upErr
			}
			if changeID {
				item = mockContainer(d, newID, true)
			}
			return nil, nil
		default:
			t.Fatalf("unexpected Docker command: %q", args)
			return nil, nil
		}
	}
	if got := requireJSON[status](t, run, "status", "--dir", dir); got.Ready || got.Running || got.Detail == "" {
		t.Fatal("missing container incorrectly reported as ready")
	}
	started := requireJSON[status](t, run, "up", "--dir", dir)
	if !started.Ready || started.ContainerID != oldID || started.PublicKey != d.PublicKey {
		t.Fatal("start did not report original identity")
	}
	again := requireJSON[status](t, run, "up", "--dir", dir)
	if !again.Ready || again.Fingerprint != d.Fingerprint {
		t.Fatal("same-config up changed identity")
	}
	rebuilt := requireJSON[status](t, run, "recreate", "--dir", dir)
	if !rebuilt.Ready || rebuilt.ContainerID != newID || rebuilt.CenterBind != started.CenterBind || rebuilt.Fingerprint != started.Fingerprint {
		t.Fatal("recreate changed persisted bind or identity, or retained container")
	}
	if raw, err := os.ReadFile(db); err != nil || !bytes.Equal(raw, savedDB) {
		t.Fatal("recreate altered persisted SQLite file")
	}
	for _, mutate := range []func(*container){
		func(c *container) { c.Config.Image = "untrusted" },
		func(c *container) { c.Config.Cmd[0] = "wrong-command" },
		func(c *container) { c.Config.WorkingDir = "/tmp" },
		func(c *container) { c.HostConfig.PortBindings["21115/tcp"][0].HostPort = "9999" },
		func(c *container) { delete(c.NetworkSettings.Ports, "21116/udp") },
		func(c *container) { c.Mounts[0].Source = "/another/deployment" },
		func(c *container) { c.Config.Labels["com.docker.compose.project.working_dir"] = "/wrong-project" },
	} {
		item = mockContainer(d, newID, true)
		mutate(&item)
		if _, err := invoke(run, "status", "--dir", dir); err == nil {
			t.Fatal("status accepted runtime configuration drift")
		}
	}
	item = mockContainer(d, newID, false)
	if got := requireJSON[status](t, run, "status", "--dir", dir); got.Ready || got.Running {
		t.Fatal("stopped container was reported as ready")
	}
	item = mockContainer(d, newID, true)
	upErr = errors.New("example image error")
	if _, err := invoke(run, "recreate", "--dir", dir); err == nil || !errors.Is(err, upErr) {
		t.Fatalf("recreate did not propagate Docker failure: %v", err)
	}
	upErr, changeID = nil, false
	if _, err := invoke(run, "recreate", "--dir", dir); err == nil || !strings.Contains(err.Error(), "ID did not change") {
		t.Fatalf("unchanged container was accepted: %v", err)
	}
	before := upCount
	if err := os.Remove(db); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(run, "up", "--dir", dir); err == nil || upCount != before {
		t.Fatalf("start was attempted after the initialized database disappeared: %v", err)
	}

	t.Run("relay lifecycle uses relay only", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "relay")
		d := requireJSON[deployment](t, docker, "init", "--role", "relay", "--dir", dir, "--center-key", d.PublicKey)
		var running bool
		run := func(_ context.Context, args ...string) ([]byte, error) {
			if args[0] == "inspect" && args[len(args)-1] == oldID {
				return json.Marshal([]container{mockContainer(d, oldID, running)})
			}
			if args[0] != "compose" || args[2] != d.Project || args[len(args)-1] != "hbbr" {
				t.Fatalf("unexpected relay command: %q", args)
			}
			if slices.Equal(args[len(args)-3:], []string{"up", "-d", "hbbr"}) {
				running = true
			}
			if running {
				return []byte(oldID + "\n"), nil
			}
			return nil, nil
		}
		if got := requireJSON[status](t, run, "up", "--dir", dir); !got.Ready || got.DatabaseReady != nil || got.CenterBind != "" {
			t.Fatal("relay uses center-only resources")
		}
		if _, err := invoke(run, "recreate", "--dir", dir); err == nil {
			t.Fatal("relay unexpectedly accepted center-only recreate")
		}
	})
}

func TestM0RunbookContract(t *testing.T) {
	body, err := os.ReadFile("../../docs/DeskRealm-M0-Runbook.md")
	if err != nil {
		t.Fatal(err)
	}
	runbook := string(body)
	for _, marker := range []string{
		"Phase 5", "1.1.16", "1.5.3", "init --role center", "init --role relay", "config --dir",
		"up --dir", "status --dir", "recreate --dir", "21116/udp", "relay-servers", "<ID>/r",
		"SQLite", "公钥指纹", "候选提交", "UTC", "SHA-256", "容器 ID", "got paired",
	} {
		if !strings.Contains(runbook, marker) {
			t.Errorf("runbook does not include %q", marker)
		}
	}
	last := -1
	for _, section := range []string{"### 1. 双端注册", "### 2. 安全直连", "### 3. 强制中继", "### 4. 重建后沿用原公钥"} {
		at := strings.Index(runbook, section)
		if at <= last {
			t.Errorf("missing or out of order: %s", section)
		}
		last = at
	}
}
