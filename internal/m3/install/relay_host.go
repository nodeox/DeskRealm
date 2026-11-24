package install

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"deskrealm/internal/m1"
	"deskrealm/internal/relayprotocol"
)

const relayHostFile = "relay-node.json"

var relayNodeID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type relayHost struct {
	Version int                          `json:"version"`
	NodeID  string                       `json:"node_id"`
	Center  string                       `json:"center"`
	Seed    []byte                       `json:"seed"`
	Config  *relayprotocol.Configuration `json:"configuration,omitempty"`
}

type relayHostStatus struct {
	NodeID    string                       `json:"node_id"`
	Center    string                       `json:"center"`
	PublicKey string                       `json:"public_key"`
	Phase     string                       `json:"phase"`
	Config    *relayprotocol.Configuration `json:"configuration,omitempty"`
}

func relayOrigin(raw string) (string, error) {
	origin, err := m1.ParseOrigin(raw, false)
	if err != nil || !validHost(origin.Hostname()) {
		return "", errors.New("center must be a valid HTTPS origin")
	}
	if strings.HasSuffix(origin.Host, ":") {
		return "", errors.New("center has an invalid HTTPS port")
	}
	if origin.Port() != "" {
		if _, err := parsePort(origin.Port()); err != nil {
			return "", errors.New("center has an invalid HTTPS port")
		}
	}
	return origin.String(), nil
}

func decodeRelayResponse(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
		return errors.New("invalid relay JSON response or state")
	}
	return nil
}

func validRelayHostConfig(node string, value *relayprotocol.Configuration) bool {
	if value == nil || value.NodeID != node || value.Version < 1 || value.Capacity < 1 || value.Capacity > 100000 ||
		value.Site == "" || value.Region == "" || value.Build == "" {
		return false
	}
	endpoint, err := normalizeEndpoint(value.Endpoint)
	return err == nil && endpoint == value.Endpoint
}

func readRelayHost(directory string) (relayHost, error) {
	var state relayHost
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return state, errors.New("relay state must be an existing private directory")
	}
	data, err := readSmallRegular(filepath.Join(directory, relayHostFile), true)
	if err != nil {
		return state, errors.New("relay identity is missing or unreadable; restore the original node state")
	}
	if decodeRelayResponse(data, &state) != nil || state.Version != 1 || !relayNodeID.MatchString(state.NodeID) ||
		len(state.Seed) != ed25519.SeedSize || (state.Config != nil && !validRelayHostConfig(state.NodeID, state.Config)) {
		return relayHost{}, errors.New("invalid relay identity; restore the original node state")
	}
	if origin, err := relayOrigin(state.Center); err != nil || origin != state.Center {
		return relayHost{}, errors.New("invalid persisted relay center origin")
	}
	return state, nil
}

func prepareRelayHost(directory, center, node string) (relayHost, error) {
	if _, err := os.Lstat(directory); err == nil {
		state, err := readRelayHost(directory)
		if err != nil {
			return state, err
		}
		if state.Center != center || state.NodeID != node {
			return relayHost{}, errors.New("relay identity is already bound to a different center or node")
		}
		return state, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return relayHost{}, err
	}
	state := relayHost{Version: 1, Center: center, NodeID: node, Seed: make([]byte, ed25519.SeedSize)}
	if _, err := rand.Read(state.Seed); err != nil {
		return relayHost{}, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(directory), ".deskrealm-relay-")
	if err != nil {
		return relayHost{}, err
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0700); err != nil {
		return relayHost{}, err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return relayHost{}, err
	}
	if err := writeSynced(filepath.Join(stage, relayHostFile), data, 0600); err != nil {
		return relayHost{}, err
	}
	if err := syncDirectory(stage); err != nil {
		return relayHost{}, err
	}
	if err := os.Rename(stage, directory); err != nil {
		if info, statErr := os.Lstat(directory); statErr == nil && info.IsDir() {
			return prepareRelayHost(directory, center, node)
		}
		return relayHost{}, err
	}
	return state, syncDirectory(filepath.Dir(directory))
}

func (s relayHost) status() relayHostStatus {
	out := relayHostStatus{NodeID: s.NodeID, Center: s.Center, Phase: "prepared", Config: s.Config,
		PublicKey: base64.RawURLEncoding.EncodeToString(ed25519.NewKeyFromSeed(s.Seed).Public().(ed25519.PublicKey))}
	if s.Config != nil {
		out.Phase = "enrolled"
	}
	return out
}

func newRelayClient(ca string) (*http.Client, error) {
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if ca != "" {
		data, err := readOfflineFile(ca, 1<<20)
		roots := x509.NewCertPool()
		if err != nil || !roots.AppendCertsFromPEM(data) {
			return nil, errors.New("could not load the configured center CA")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

type relayHTTPStatus int

func (s relayHTTPStatus) Error() string { return fmt.Sprintf("center returned HTTP %d", s) }

func relayPost(ctx context.Context, client *http.Client, origin, path, credential string, body, out any) error {
	return relayCall(ctx, client, http.MethodPost, origin, path, credential, body, out)
}

func relayCall(ctx context.Context, client *http.Client, method, origin, path, credential string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	endpoint, err := url.JoinPath(origin, path)
	if err != nil {
		return errors.New("invalid relay request URL")
	}
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("center HTTPS connection: %w", err)
	}
	defer response.Body.Close()
	expected := http.StatusOK
	if out == nil {
		expected = http.StatusNoContent
	}
	if response.StatusCode != expected {
		return relayHTTPStatus(response.StatusCode)
	}
	if out == nil {
		return nil
	}
	const maximum = 8192
	data, err = io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil || len(data) > maximum {
		return errors.New("invalid relay response size")
	}
	return decodeRelayResponse(data, out)
}

func enrollRelayHost(ctx context.Context, client *http.Client, directory, credentialFile string, node relayHost) (relayHost, error) {
	var configuration relayprotocol.Configuration
	key := ed25519.NewKeyFromSeed(node.Seed)
	sign := func(frame []byte) string { return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, frame)) }
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return node, err
	}
	recover := relayprotocol.ConfigRequest{NodeID: node.NodeID, Nonce: base64.RawURLEncoding.EncodeToString(nonce[:]),
		ObservedMS: time.Now().UnixMilli()}
	err := relayPost(ctx, client, node.Center, "/v1/relays/configuration", "", struct {
		relayprotocol.ConfigRequest
		Signature string `json:"signature"`
	}{recover, sign(recover.Frame())}, &configuration)
	if errors.Is(err, relayHTTPStatus(http.StatusUnauthorized)) && node.Config == nil {
		if credentialFile == "" {
			return node, errors.New("provide the one-time credential file for first enrollment")
		}
		raw, err := readSmallRegular(credentialFile, true)
		if err != nil {
			return node, errors.New("enrollment credential must be in a readable private regular file")
		}
		credential := strings.TrimSuffix(string(raw), "\n")
		value, ok := strings.CutPrefix(credential, "dre_")
		if !ok || len(value) != 43 {
			return node, errors.New("invalid one-time relay enrollment credential")
		}
		decoded, decodeErr := base64.RawURLEncoding.Strict().DecodeString(value)
		if decodeErr != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != value {
			return node, errors.New("invalid one-time relay enrollment credential")
		}
		request := struct {
			NodeID    string `json:"node_id"`
			PublicKey string `json:"public_key"`
			Signature string `json:"signature,omitempty"`
		}{NodeID: node.NodeID, PublicKey: node.status().PublicKey}
		var challenge relayprotocol.Enrollment
		if err := relayPost(ctx, client, node.Center, "/v1/relays/enrollment/challenge", credential, request, &challenge); err != nil {
			return node, fmt.Errorf("relay enrollment challenge: %w", err)
		}
		if !validRelayHostConfig(node.NodeID, &challenge.Configuration) || challenge.PublicKey != request.PublicKey ||
			len(challenge.Challenge) != 43 || challenge.ExpiresMS <= time.Now().UnixMilli() {
			return node, errors.New("center challenge does not match the persistent relay identity")
		}
		request.Signature = sign(challenge.Frame())
		if err := relayPost(ctx, client, node.Center, "/v1/relays/enrollment/complete", credential, request, &configuration); err != nil {
			return node, fmt.Errorf("relay enrollment completion: %w", err)
		}
		if configuration != challenge.Configuration {
			return node, errors.New("center returned configuration different from the signed challenge")
		}
	} else if err != nil {
		return node, fmt.Errorf("relay configuration: %w", err)
	}
	if !validRelayHostConfig(node.NodeID, &configuration) ||
		(node.Config != nil && (node.Config.Version > configuration.Version ||
			(node.Config.Version == configuration.Version && *node.Config != configuration))) {
		return node, errors.New("center configuration has a mismatched identity or version")
	}
	if node.Config != nil && *node.Config == configuration {
		return node, nil
	}
	node.Config = &configuration
	data, err := json.MarshalIndent(node, "", "  ")
	if err != nil {
		return node, err
	}
	return node, writeAtomic(directory, relayHostFile, data, 0600)
}

func RunRelay(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	const help = `Usage:
  deskrealm-relay prepare --state DIR --center HTTPS_ORIGIN --node NODE_ID
  deskrealm-relay enroll  --state DIR [--ca PEM] [--credential-file FILE]
  deskrealm-relay run     --state DIR --hbbr ABSOLUTE_PATH [--port PORT] [--ca PEM]
  deskrealm-relay status  --state DIR
`
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(stdout, help)
		return err
	}
	if len(args) == 0 || (args[0] != "prepare" && args[0] != "enroll" && args[0] != "run" && args[0] != "status") {
		return errors.New(help)
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("state", "", "absolute private node state directory")
	var center, id, credential, ca, hbbr, port string
	switch command {
	case "prepare":
		flags.StringVar(&center, "center", "", "HTTPS center origin")
		flags.StringVar(&id, "node", "", "center-assigned node ID")
	case "enroll":
		flags.StringVar(&credential, "credential-file", "", "private one-time credential file")
		flags.StringVar(&ca, "ca", "", "optional center CA certificate")
	case "run":
		flags.StringVar(&hbbr, "hbbr", "", "absolute path of the managed hbbr binary")
		flags.StringVar(&port, "port", "", "local TCP port, defaults to the published port")
		flags.StringVar(&ca, "ca", "", "optional center CA certificate")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	directory, err := canonicalDirectory(*path)
	if err != nil {
		return err
	}
	var state relayHost
	if command == "prepare" {
		origin, err := relayOrigin(center)
		if err != nil {
			return err
		}
		if !relayNodeID.MatchString(id) {
			return errors.New("--node must be the UUID assigned by the center")
		}
		state, err = prepareRelayHost(directory, origin, id)
		if err != nil {
			return err
		}
	} else if command == "enroll" || command == "run" {
		lock, err := lockDeployment(directory)
		if err != nil {
			return err
		}
		defer lock.Close()
		state, err = readRelayHost(directory)
		if err != nil {
			return err
		}
		client, err := newRelayClient(ca)
		if err != nil {
			return err
		}
		defer client.CloseIdleConnections()
		if command == "run" {
			return runRelayHost(ctx, client, directory, hbbr, port, state, stdout, stderr)
		}
		state, err = enrollRelayHost(ctx, client, directory, credential, state)
		if err != nil {
			return err
		}
	} else {
		state, err = readRelayHost(directory)
		if err != nil {
			return err
		}
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(state.status())
}
