package relayprotocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"

	"deskrealm/internal/managedproto"
)

const sessionProbePrefix = "DeskRealm/relay/v1/rtt-1\x00"
const SessionProbeFrameSize = 199

type SessionProbePermit struct {
	Frame     string `json:"frame"`
	Signature string `json:"signature"`
}

// SessionProbeRTT times the proof/response round trip over an open TCP
// connection. TCP setup and the nonce exchange are outside the measured RTT.
func SessionProbeRTT(ctx context.Context, endpoint string, relayKey ed25519.PublicKey, build string,
	permit SessionProbePermit, deviceKey ed25519.PrivateKey, sample byte) (time.Duration, error) {
	frame := decode(permit.Frame, SessionProbeFrameSize)
	signature := decode(permit.Signature, ed25519.SignatureSize)
	if frame == nil || signature == nil || len(deviceKey) != ed25519.PrivateKeySize || sample >= 3 {
		return 0, ErrProbeProtocol
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
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
	request := append(append([]byte(sessionProbePrefix), frame...), signature...)
	if _, err := conn.Write(request); err != nil {
		return 0, err
	}
	var nonce [32]byte
	if _, err := io.ReadFull(conn, nonce[:]); err != nil {
		return 0, probeReadError(err)
	}
	digest := sha256.Sum256(frame)
	proof, err := managedproto.Encode(managedproto.RelayProbeHolder, digest[:], []byte{sample}, nonce[:])
	if err != nil {
		return 0, err
	}
	signedProof := append([]byte{sample}, ed25519.Sign(deviceKey, proof)...)
	start := time.Now()
	if _, err := conn.Write(signedProof); err != nil {
		return 0, err
	}
	var reply [probeBuildBytes + 4 + ed25519.SignatureSize]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return 0, probeReadError(err)
	}
	elapsed := time.Since(start)
	data, relaySignature := reply[:probeBuildBytes+4], reply[probeBuildBytes+4:]
	message := append(append([]byte(sessionProbePrefix), proof...), data...)
	if len(relayKey) != ed25519.PublicKeySize || !ed25519.Verify(relayKey, message, relaySignature) ||
		string(bytes.TrimRight(data[:probeBuildBytes], "\x00")) != build || binary.BigEndian.Uint32(data[probeBuildBytes:]) > 100000 {
		return 0, ErrProbeProtocol
	}
	return elapsed, nil
}

func probeReadError(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrProbeProtocol
	}
	return err
}
