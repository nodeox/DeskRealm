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

func runRelayHost(ctx context.Context, client *http.Client, directory, binary, port string, state relayHost, stdout, stderr io.Writer) error {
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
	childCtx, stop := context.WithCancel(ctx)
	cmd := exec.CommandContext(childCtx, binary, "--port", port, "--deskrealm-regional")
	cmd.Dir = directory
	cmd.Env = append(os.Environ(), "DESKREALM_RELAY_STATE="+filepath.Join(directory, relayHostFile))
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		stop()
		return err
	}
	done := make(chan struct{})
	var processErr error
	go func() {
		defer close(done)
		processErr = cmd.Wait()
		stop()
	}()
	defer func() {
		stop()
		<-done
	}()
	key := ed25519.NewKeyFromSeed(state.Seed)
	endpoint := net.JoinHostPort("127.0.0.1", port)
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
	<-done
	if ctx.Err() != nil {
		return nil
	}
	if processErr != nil {
		return fmt.Errorf("hbbr exited: %w", processErr)
	}
	return errors.New("hbbr exited")
}
