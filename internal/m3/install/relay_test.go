package install

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureDefaultRelay(t *testing.T, address string) (config, string) {
	t.Helper()
	c := preparedConfig(t)
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	path := filepath.Join(t.TempDir(), "default-relay.token")
	writeFixture(t, path, []byte(token+"\n"))
	c.DefaultRelay = relayConfig{"existing-relay", address, path}
	c, err := validateConfig(c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return c, token
}

func TestDefaultRelayPersistsPublicConfigurationAndPrivateCredential(t *testing.T) {
	c, token := fixtureDefaultRelay(t, "Relay.Example.Test.:021117")
	center, err := prepareDeployment(c)
	if err != nil {
		t.Fatal(err)
	}
	if center.DefaultRelay != (publicRelay{"existing-relay", "relay.example.test:21117"}) {
		t.Fatal("prepared identity did not include normalized default relay")
	}
	data, err := os.ReadFile(center.ComposeFile)
	var compose composeConfig
	if err != nil || json.Unmarshal(data, &compose) != nil || strings.Contains(string(data), token) {
		t.Fatal("invalid Compose or embedded relay credential")
	}
	rendezvous := compose.Services["rendezvous"]
	app := compose.Services["center"]
	if rendezvous.Environment[relayAddressEnv] != c.DefaultRelay.Address ||
		rendezvous.Environment[relayIDEnv] != c.DefaultRelay.ID || app.Environment[relayIDEnv] != c.DefaultRelay.ID ||
		len(rendezvous.EnvFile) != 1 || len(app.EnvFile) != 2 || app.EnvFile[1] != "./"+relayEnvFile {
		t.Fatal("relay settings were not distributed to the intended services")
	}
	path := filepath.Join(c.Directory, relayEnvFile)
	if private, err := readSmallRegular(path, true); err != nil || string(private) != relayTokenEnv+"="+token+"\n" {
		t.Fatal("the relay credential is missing from private state")
	}
	if err := os.Remove(c.DefaultRelay.TokenFile); err != nil {
		t.Fatal(err)
	}
	if rerun, err := prepareDeployment(c); err != nil || rerun != center {
		t.Fatal("re-running prepare did not preserve original relay configuration")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrepared(c); err == nil {
		t.Fatal("deployment accepted loss of the persisted relay credential")
	}
}

func TestDefaultRelayChecksRunningConfigurationAndEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	c, token := fixtureDefaultRelay(t, listener.Addr().String())
	center, err := prepareDeployment(c)
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string][]string{
		"center":     {relayIDEnv + "=" + c.DefaultRelay.ID, relayTokenEnv + "=" + token},
		"rendezvous": {relayIDEnv + "=" + c.DefaultRelay.ID, relayAddressEnv + "=" + c.DefaultRelay.Address},
	}
	ids := map[string]string{"center": strings.Repeat("a", 64), "rendezvous": strings.Repeat("b", 64)}
	run := func(_ context.Context, args ...string) ([]byte, error) {
		last := args[len(args)-1]
		if args[0] == "compose" && ids[last] != "" {
			return []byte(ids[last] + "\n"), nil
		}
		for service, id := range ids {
			if args[0] == "container" && last == id {
				return json.Marshal(actual[service])
			}
		}
		t.Fatalf("unexpected Docker call: %v", args)
		return nil, nil
	}
	if err := checkDefaultRelay(context.Background(), c, center, run); err != nil {
		t.Fatal(err)
	}
	actual["rendezvous"] = append(actual["rendezvous"], relayTokenEnv+"="+token)
	if err := checkDefaultRelay(context.Background(), c, center, run); err == nil || strings.Contains(err.Error(), token) {
		t.Fatal("incorrect runtime configuration was accepted or leaked a credential")
	}
	actual["rendezvous"] = actual["rendezvous"][:2]
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checkDefaultRelay(context.Background(), c, center, run); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatal("installer accepted an unavailable default relay")
	}
}
