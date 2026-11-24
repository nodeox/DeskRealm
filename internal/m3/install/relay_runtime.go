package install

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"deskrealm/internal/relayprotocol"
)

const relaySequenceFile = "health-sequence"

func reserveRelaySequence(directory string, now time.Time) (int64, error) {
	sequence := now.UnixMilli()
	data, err := readSmallRegular(filepath.Join(directory, relaySequenceFile), true)
	if err == nil {
		last, err := strconv.ParseInt(strings.TrimSuffix(string(data), "\n"), 10, 64)
		if err != nil || last < 1 || last == math.MaxInt64 {
			return 0, errors.New("invalid persisted relay health sequence")
		}
		sequence = max(sequence, last+1)
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if err := writeAtomic(directory, relaySequenceFile, []byte(strconv.FormatInt(sequence, 10)+"\n"), 0600); err != nil {
		return 0, err
	}
	return sequence, nil
}

func reportRelayHealth(ctx context.Context, client *http.Client, directory, endpoint string, state relayHost, key ed25519.PrivateKey) error {
	active, err := relayprotocol.Probe(ctx, endpoint, key.Public().(ed25519.PublicKey), state.Config.Build)
	ready := err == nil && active <= state.Config.Capacity
	if !ready {
		active = 0
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	now := time.Now()
	sequence, err := reserveRelaySequence(directory, now)
	if err != nil {
		return err
	}
	health := relayprotocol.Health{NodeID: state.NodeID, ConfigVersion: state.Config.Version,
		Sequence: sequence, ObservedMS: now.UnixMilli(), Build: state.Config.Build, ProtocolReady: ready, ActiveSessions: active}
	return relayPost(ctx, client, state.Center, "/v1/relays/health", "", struct {
		relayprotocol.Health
		Signature string `json:"signature"`
	}{health, base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, health.Frame()))}, nil)
}

func runRelayHost(ctx context.Context, client *http.Client, directory, binary, port string, state relayHost, managed bool, stdout, stderr io.Writer) error {
	if state.Config == nil {
		return errors.New("enroll the relay before starting hbbr")
	}
	if !filepath.IsAbs(binary) {
		return errors.New("--hbbr must be the absolute path of the managed hbbr binary")
	}
	state, err := enrollRelayHost(ctx, client, directory, "", state)
	if err != nil {
		return err
	}
	if port == "" {
		_, port, _ = net.SplitHostPort(state.Config.Endpoint)
	}
	number, err := parsePort(port)
	if err != nil {
		return fmt.Errorf("invalid relay port: %w", err)
	}
	port = strconv.FormatUint(uint64(number), 10)
	var centerKey struct {
		ID     string `json:"kid"`
		Public string `json:"public_key"`
	}
	err = relayCall(ctx, client, http.MethodGet, state.Center, "/v1/managed/signing-key", "", nil, &centerKey)
	if err != nil && !errors.Is(err, relayHTTPStatus(http.StatusServiceUnavailable)) {
		return fmt.Errorf("read center signing key: %w", err)
	}
	if err == nil && (centerKey.ID == "" || relayprotocol.PublicKey(centerKey.Public) == nil) {
		return errors.New("center returned an invalid managed signing key")
	}
	start := func(path string) (*relayProcess, error) {
		return startRelayProcess(ctx, directory, path, port, centerKey.Public, stdout, stderr)
	}
	endpoint := net.JoinHostPort("127.0.0.1", port)
	if managed {
		return superviseRelayUpgrade(ctx, client, directory, binary, endpoint, state, start, stderr)
	}
	if _, err := os.Lstat(filepath.Join(directory, relayUpgradeDir)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("relay state uses managed executables; restart with --managed-upgrades")
	}
	process, err := start(binary)
	if err != nil {
		return err
	}
	defer process.close()
	childCtx, stop := context.WithCancel(ctx)
	go func() { <-process.done; stop() }()
	defer stop()
	key := ed25519.NewKeyFromSeed(state.Seed)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for childCtx.Err() == nil {
		err := reportRelayHealth(childCtx, client, directory, endpoint, state, key)
		if errors.Is(err, relayHTTPStatus(http.StatusConflict)) {
			updated, refreshErr := enrollRelayHost(childCtx, client, directory, "", state)
			if refreshErr != nil {
				err = errors.Join(err, refreshErr)
			} else if updated.Config.Version != state.Config.Version {
				state = updated
				err = reportRelayHealth(childCtx, client, directory, endpoint, state, key)
			}
		}
		if err != nil && childCtx.Err() == nil {
			fmt.Fprintln(stderr, "relay health:", err)
		}
		select {
		case <-childCtx.Done():
		case <-tick.C:
		}
	}
	<-process.done
	if ctx.Err() != nil {
		return nil
	}
	return process.exitError()
}

type relayProcess struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func startRelayProcess(ctx context.Context, directory, binary, port, centerKey string, stdout, stderr io.Writer) (*relayProcess, error) {
	childCtx, stop := context.WithCancel(ctx)
	cmd := exec.CommandContext(childCtx, binary, "--port", port, "--deskrealm-regional")
	cmd.Dir = directory
	cmd.Env = append(os.Environ(), "DESKREALM_RELAY_STATE="+filepath.Join(directory, relayHostFile),
		"DESKREALM_REGIONAL_CENTER_PROBE_KEY="+centerKey)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		stop()
		return nil, err
	}
	process := &relayProcess{cancel: stop, done: make(chan struct{})}
	go func() {
		defer close(process.done)
		process.err = cmd.Wait()
	}()
	return process, nil
}

func (p *relayProcess) close() {
	p.cancel()
	<-p.done
}

func (p *relayProcess) exitError() error {
	if p.err != nil {
		return fmt.Errorf("hbbr exited: %w", p.err)
	}
	return errors.New("hbbr exited")
}
