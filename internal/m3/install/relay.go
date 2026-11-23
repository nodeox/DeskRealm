package install

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"deskrealm/internal/m1"
)

const (
	relayEnvFile    = "secrets/relay.env"
	relayIDEnv      = "DESKREALM_MANAGED_RELAY_ID"
	relayAddressEnv = "DESKREALM_MANAGED_RELAY_ADDRESS"
	relayTokenEnv   = "DESKREALM_MANAGED_RELAY_TOKEN"
)

type publicRelay struct {
	ID      string `json:"id"`
	Address string `json:"address"`
}

type relayConfig struct {
	ID        string `json:"id"`
	Address   string `json:"address"`
	TokenFile string `json:"token_file"`
}

var relayIdentifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
var containerIdentifier = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

func normalizeRelay(relay relayConfig) (relayConfig, error) {
	if relay == (relayConfig{}) {
		return relay, nil
	}
	if !relayIdentifier.MatchString(relay.ID) {
		return relayConfig{}, errors.New("default_relay.id must be a valid managed relay ID")
	}
	address, err := normalizeEndpoint(relay.Address)
	if err != nil {
		return relayConfig{}, fmt.Errorf("default_relay.address: %w", err)
	}
	host, port, _ := net.SplitHostPort(address)
	relay.Address = net.JoinHostPort(strings.TrimSuffix(host, "."), port)
	if !filepath.IsAbs(relay.TokenFile) || strings.IndexFunc(relay.TokenFile, unicode.IsControl) >= 0 {
		return relayConfig{}, errors.New("default_relay.token_file must be an absolute path")
	}
	relay.TokenFile = filepath.Clean(relay.TokenFile)
	return relay, nil
}

func readRelayToken(path string) (string, error) {
	data, err := readSmallRegular(path, true)
	if err != nil {
		return "", errors.New("default relay token file must be a readable private regular file")
	}
	token := strings.TrimSuffix(string(data), "\n")
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(token) != 43 || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != token {
		return "", errors.New("default relay token must be a 32-byte unpadded base64url value")
	}
	return token, nil
}

func deploymentRelayToken(config config, rendezvousToken string) (string, error) {
	token, err := readRelayToken(config.DefaultRelay.TokenFile)
	if err != nil {
		return "", err
	}
	if _, err := m1.ConfigureManagedTransport(rendezvousToken, config.DefaultRelay.ID, token); err != nil {
		return "", err
	}
	return token, nil
}

func checkDefaultRelay(ctx context.Context, c config, center publicDeployment, run dockerRunner) error {
	data, err := readSmallRegular(filepath.Join(c.Directory, relayEnvFile), true)
	token, ok := strings.CutPrefix(strings.TrimSuffix(string(data), "\n"), relayTokenEnv+"=")
	if err != nil || !ok || len(token) != 43 {
		return errors.New("cannot read the persisted default relay credentials")
	}
	for _, service := range []struct {
		name    string
		address string
		token   string
	}{
		{"center", "", token},
		{"rendezvous", c.DefaultRelay.Address, ""},
	} {
		actual, err := runningRelayEnvironment(ctx, center, service.name, run)
		if err != nil {
			return err
		}
		if actual[relayIDEnv] != c.DefaultRelay.ID || actual[relayAddressEnv] != service.address ||
			subtle.ConstantTimeCompare([]byte(actual[relayTokenEnv]), []byte(service.token)) != 1 {
			return fmt.Errorf("running %s default relay configuration differs from the deployment", service.name)
		}
	}
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", c.DefaultRelay.Address)
	if err != nil {
		return errors.New("configured default relay TCP endpoint is unreachable from the installer host")
	}
	return connection.Close()
}

func runningRelayEnvironment(ctx context.Context, center publicDeployment, service string, run dockerRunner) (map[string]string, error) {
	data, err := composeCommand(ctx, run, center, "ps", "--quiet", service)
	id := strings.TrimSpace(string(data))
	if err != nil || !containerIdentifier.MatchString(id) {
		return nil, fmt.Errorf("cannot identify the running %s container for default relay verification", service)
	}
	data, err = run(ctx, "container", "inspect", "--format", "{{json .Config.Env}}", id)
	var fields []string
	if err != nil || json.Unmarshal(data, &fields) != nil || fields == nil {
		return nil, fmt.Errorf("cannot inspect the running %s default relay configuration", service)
	}
	relay := make(map[string]string, 3)
	for _, field := range fields {
		key, value, _ := strings.Cut(field, "=")
		switch key {
		case relayIDEnv, relayAddressEnv, relayTokenEnv:
			if _, duplicate := relay[key]; duplicate {
				return nil, fmt.Errorf("running %s has duplicate default relay settings", service)
			}
			relay[key] = value
		}
	}
	return relay, nil
}
