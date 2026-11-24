package install

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"deskrealm/internal/relayprotocol"
)

type relaySupervisor struct {
	client   *http.Client
	dir      string
	endpoint string
	node     relayHost
	key      ed25519.PrivateKey
	start    func(string) (*relayProcess, error)
	process  *relayProcess
	image    string
}

func superviseRelayUpgrade(ctx context.Context, client *http.Client, directory, source, endpoint string, node relayHost,
	start func(string) (*relayProcess, error), stderr io.Writer) error {
	dir := filepath.Join(directory, relayUpgradeDir)
	if _, err := prepareManagedRelayBinary(dir, source, node.NodeID); err != nil {
		return err
	}
	s := relaySupervisor{client: client, dir: dir, endpoint: endpoint, node: node,
		key: ed25519.NewKeyFromSeed(node.Seed), start: start}
	defer s.stop()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		if err := s.step(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		if s.process != nil {
			if err := reportRelayHealth(ctx, client, directory, endpoint, s.node, s.key); err != nil && ctx.Err() == nil {
				fmt.Fprintln(stderr, "relay health:", err)
				if errors.Is(err, relayHTTPStatus(http.StatusConflict)) {
					updated, refreshErr := enrollRelayHost(ctx, client, directory, "", s.node)
					if refreshErr == nil {
						s.node = updated
					}
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
	return nil
}

func (s *relaySupervisor) stop() {
	if s.process != nil {
		s.process.close()
		s.process, s.image = nil, ""
	}
}

func (s *relaySupervisor) use(ctx context.Context, digest string) error {
	if s.process != nil && s.image == digest {
		select {
		case <-s.process.done:
			return s.process.exitError()
		default:
			return nil
		}
	}
	path, err := checkedRelayBinary(s.dir, digest)
	if err != nil {
		return err
	}
	s.stop()
	// A surviving process after an unclean supervisor exit might still sign
	// probes with this node's key. Do not credit its listener to the new child.
	dialer := net.Dialer{Timeout: 250 * time.Millisecond}
	if conn, err := dialer.DialContext(ctx, "tcp", s.endpoint); err == nil {
		conn.Close()
		return errors.New("relay TCP port is occupied by another process")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	process, err := s.start(path)
	if err == nil {
		s.process, s.image = process, digest
	}
	return err
}

func (s *relaySupervisor) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		active, err := relayprotocol.Probe(ctx, s.endpoint, s.key.Public().(ed25519.PublicKey), s.node.Config.Build)
		if err == nil && active == 0 {
			select {
			case <-s.process.done:
				return s.process.exitError()
			default:
				return nil
			}
		}
		select {
		case <-s.process.done:
			return s.process.exitError()
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (s *relaySupervisor) step(ctx context.Context) error {
	lock, err := lockDeployment(s.dir)
	if err != nil {
		// The request command may be staging a binary. Keep the current process up.
		return nil
	}
	defer lock.Close()
	u, err := readRelayRuntime(s.dir, s.node.NodeID)
	if err != nil {
		return err
	}
	if u.finished() {
		return s.use(ctx, u.Active)
	}
	if u.State == "requested" {
		if err := s.use(ctx, u.Active); err != nil {
			return err
		}
	}
	status, err := relayUpgradeCall(ctx, s.client, s.node, "status", u.Task, u.Target)
	if err != nil {
		return nil // Keep the journal and retry when the center is reachable.
	}
	if status.CandidateSHA != u.Target {
		return errors.New("center upgrade does not match the local candidate SHA-256")
	}
	switch status.State {
	case "canceled":
		if u.State != "requested" {
			return errors.New("center canceled a claimed relay upgrade")
		}
		u.State = "canceled"
		return u.save(s.dir)
	case "succeeded", "rolled_back":
		if (status.State == "succeeded" && u.State != "switching") ||
			(status.State == "rolled_back" && u.State != "rolling_back") || status.OriginalSHA != u.Active {
			return errors.New("conflicting terminal relay upgrade outcome")
		}
		if status.State == "succeeded" {
			u.Active = u.Target
		}
		u.State = status.State
		if err := u.save(s.dir); err != nil {
			return err
		}
		return s.use(ctx, u.Active)
	case "pending":
		if u.State != "requested" || !status.Ready {
			return nil
		}
		active, err := relayprotocol.Probe(ctx, s.endpoint, s.key.Public().(ed25519.PublicKey), s.node.Config.Build)
		if err != nil || active != 0 {
			return nil
		}
		status, err = relayUpgradeCall(ctx, s.client, s.node, "claim", u.Task, u.Active)
		if err != nil {
			return nil
		}
	case "claimed":
	default:
		return errors.New("unknown center relay upgrade phase")
	}
	if status.State != "claimed" || status.OriginalSHA != u.Active || status.CandidateSHA != u.Target {
		return errors.New("relay claim does not match the journal")
	}
	if u.State == "requested" {
		active, err := relayprotocol.Probe(ctx, s.endpoint, s.key.Public().(ed25519.PublicKey), s.node.Config.Build)
		if err != nil || active != 0 {
			return nil
		}
		u.State = "switching"
		if err := u.save(s.dir); err != nil {
			return err
		}
	}
	digest, outcome := u.Target, "succeeded"
	if u.State == "rolling_back" {
		digest, outcome = u.Active, "rolled_back"
	}
	err = s.use(ctx, digest)
	if err == nil {
		err = s.waitReady(ctx)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if u.State == "rolling_back" {
			return fmt.Errorf("original relay failed health verification; upgrade remains held: %w", err)
		}
		u.State = "rolling_back"
		if err := u.save(s.dir); err != nil {
			return err
		}
		s.stop()
		return nil
	}
	if err := reportRelayHealth(ctx, s.client, filepath.Dir(s.dir), s.endpoint, s.node, s.key); err != nil {
		return nil
	}
	_, _ = relayUpgradeCall(ctx, s.client, s.node, outcome, u.Task, digest)
	// The next step reconciles the persisted center response, including a lost
	// response after commit, before permitting another local operation.
	return nil
}
