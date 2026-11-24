package relayprotocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"
)

const probePrefix = "DeskRealm/relay/v1/probe\x00"
const probeBuildBytes = 64
const ProbeTimeout = 2 * time.Second

var ErrProbeProtocol = errors.New("invalid signed relay probe response")

// Probe measures the published TCP endpoint with a fresh, domain-separated
// challenge. The build and session count are covered by the endpoint signature.
func Probe(ctx context.Context, endpoint string, key ed25519.PublicKey, build string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	var challenge [len(probePrefix) + 32]byte
	copy(challenge[:], probePrefix)
	if _, err := rand.Read(challenge[len(probePrefix):]); err != nil {
		return 0, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, err
	}
	if _, err := conn.Write(challenge[:]); err != nil {
		return 0, err
	}
	var reply [probeBuildBytes + 4 + ed25519.SignatureSize]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return 0, probeReadError(err)
	}
	data, signature := reply[:probeBuildBytes+4], reply[probeBuildBytes+4:]
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, append(challenge[:], data...), signature) ||
		string(bytes.TrimRight(data[:probeBuildBytes], "\x00")) != build {
		return 0, ErrProbeProtocol
	}
	sessions := binary.BigEndian.Uint32(data[probeBuildBytes:])
	if sessions > 100000 {
		return 0, ErrProbeProtocol
	}
	return int(sessions), nil
}
