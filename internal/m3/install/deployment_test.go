package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"deskrealm/internal/m1"
)

func preparedConfig(t *testing.T) config {
	t.Helper()
	c, err := validateConfig(fixtureConfig(t), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPrepareDeploymentKeepsPrivateMaterialAndStableIdentity(t *testing.T) {
	c := preparedConfig(t)
	initial, err := prepareDeployment(c)
	if err != nil {
		t.Fatal(err)
	}
	rerun, err := prepareDeployment(c)
	if err != nil || rerun != initial {
		t.Fatalf("prepare changed the published identity: %+v, %v", rerun, err)
	}
	key, err := m1.LoadManagedSigningKey(filepath.Join(c.Directory, "secrets/signing.seed"), initial.SigningKeyID)
	if err != nil || key == nil {
		t.Fatalf("Go center cannot load the staged key: %v", err)
	}
	file, err := os.ReadFile(initial.ComposeFile)
	if err != nil {
		t.Fatal(err)
	}
	var model composeConfig
	if err := json.Unmarshal(file, &model); err != nil {
		t.Fatal(err)
	}
	for _, name := range requiredFiles {
		if name == "postgres-init.sql" {
			continue
		}
		info, err := os.Stat(filepath.Join(c.Directory, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s is missing or has unsafe permissions", name)
		}
	}
	if db := model.Services["database"]; len(db.Ports) != 0 || len(db.Networks) != 1 ||
		!model.Networks["database"].Internal || db.Volumes[0].Target != "/var/lib/postgresql" {
		t.Fatal("PostgreSQL is not confined to the private network and persistent bind")
	}
	center := model.Services["center"]
	if center.Ports[0].HostIP != "127.0.0.1" || center.DependsOn["migrate"].Condition != "service_completed_successfully" ||
		center.Environment["DESKREALM_SESSION_SIGNING_KID"] != initial.SigningKeyID {
		t.Fatal("external HTTPS binding, migration dependency or signing identity mismatch")
	}
	rendezvous := model.Services["rendezvous"]
	if len(rendezvous.Ports) != 3 || rendezvous.Ports[2].Protocol != "udp" ||
		rendezvous.Environment["DESKREALM_MANAGED_CENTER_PUBLIC_KEY"] != initial.SigningKey {
		t.Fatal("rendezvous is missing its native ports or trusted control-plane identity")
	}
	values := map[string][]byte{}
	for _, service := range []string{"postgres", "center", "migrate", "rendezvous"} {
		data, err := os.ReadFile(filepath.Join(c.Directory, "secrets", service+".env"))
		if err != nil {
			t.Fatal(err)
		}
		values[service] = data
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			_, secret, found := strings.Cut(line, "=")
			if !found || strings.Contains(string(file), secret) {
				t.Fatalf("Compose leaks a private value from %s", service)
			}
		}
	}
	if !strings.Contains(string(values["center"]), "postgres://deskrealm:") ||
		!strings.Contains(string(values["migrate"]), "postgres://deskrealm_migrate:") ||
		strings.Contains(string(values["rendezvous"]), "DATABASE") {
		t.Fatal("service credentials are not separated")
	}
}

func TestPrepareDeploymentRefusesIncompleteOrChangedState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *config)
	}{
		{"missing key", func(t *testing.T, c *config) {
			if err := os.Remove(filepath.Join(c.Directory, "rendezvous", "id_ed25519")); err != nil {
				t.Fatal(err)
			}
		}},
		{"changed endpoint", func(_ *testing.T, c *config) { c.IDServer = "replacement.example.test:21116" }},
		{"symlink", func(t *testing.T, c *config) {
			path := filepath.Join(c.Directory, "secrets", "signing.seed")
			if err := os.Rename(path, path+".original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+".original", path); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing data directory", func(t *testing.T, c *config) {
			if err := os.Remove(filepath.Join(c.Directory, "postgres")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := preparedConfig(t)
			if _, err := prepareDeployment(c); err != nil {
				t.Fatal(err)
			}
			tc.change(t, &c)
			if _, err := prepareDeployment(c); err == nil {
				t.Fatal("prepare accepted modified state")
			}
		})
	}
	c := preparedConfig(t)
	if err := os.Mkdir(c.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(c.Directory, "existing")
	if err := os.WriteFile(payload, []byte("keep this"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareDeployment(c); err == nil {
		t.Fatal("prepare accepted a foreign nonempty directory")
	}
	data, err := os.ReadFile(payload)
	if err != nil || string(data) != "keep this" {
		t.Fatal("prepare changed the existing payload")
	}
}

func TestPrepareDeploymentConcurrent(t *testing.T) {
	c := preparedConfig(t)
	const attempts = 6
	var group sync.WaitGroup
	results := make(chan publicDeployment, attempts)
	for range attempts {
		group.Go(func() {
			result, err := prepareDeployment(c)
			if err != nil {
				t.Error(err)
				return
			}
			results <- result
		})
	}
	group.Wait()
	close(results)
	want, err := prepareDeployment(c)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for result := range results {
		count++
		if result != want {
			t.Fatal("concurrent prepare returned a different identity")
		}
	}
	if count != attempts {
		t.Fatalf("only %d of %d callers received the deployment", count, attempts)
	}
}

func TestRenderComposeForProvidedTLS(t *testing.T) {
	c := preparedConfig(t)
	c.HTTPS = httpsConfig{Mode: httpsProvided, Certificate: "/path/$literal/fullchain.pem",
		PrivateKey: "/path/key.pem", CACertificate: "/path/root.pem"}
	c.Ports.API = 443
	_, files, err := newDeployment(c)
	if err != nil {
		t.Fatal(err)
	}
	var model composeConfig
	if err := json.Unmarshal(files[composeName], &model); err != nil {
		t.Fatal(err)
	}
	center := model.Services["center"]
	if center.Ports[0].Target != 443 || center.Ports[0].HostIP != "" ||
		center.Environment["DESKREALM_TLS_KEY_FILE"] != "/run/deskrealm/tls.key" ||
		center.Volumes[1].Source != "/path/$$literal/fullchain.pem" || !center.Volumes[1].ReadOnly ||
		center.Networks["control"].Aliases[0] != "console.example.test" {
		t.Fatal("provided TLS configuration does not match the validated origin")
	}
	if model.Services["rendezvous"].Environment["DESKREALM_MANAGED_CA_FILE"] != "/run/deskrealm/center-ca.pem" {
		t.Fatal("managed rendezvous did not receive the configured CA")
	}
}
