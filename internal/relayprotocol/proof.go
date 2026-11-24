// Package relayprotocol defines the signed messages exchanged with relay nodes.
package relayprotocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"

	"filippo.io/edwards25519"
)

type Configuration struct {
	NodeID   string `json:"node_id"`
	Endpoint string `json:"endpoint"`
	Site     string `json:"site"`
	Region   string `json:"region"`
	Build    string `json:"build"`
	Version  int64  `json:"config_version"`
	Capacity int    `json:"capacity"`
}

type Enrollment struct {
	Configuration
	Challenge string `json:"challenge"`
	ExpiresMS int64  `json:"expires_ms"`
	PublicKey string `json:"public_key"`
}

type Health struct {
	NodeID         string `json:"node_id"`
	ConfigVersion  int64  `json:"config_version"`
	Sequence       int64  `json:"sequence"`
	ObservedMS     int64  `json:"observed_ms"`
	Build          string `json:"build"`
	ProtocolReady  bool   `json:"protocol_ready"`
	ActiveSessions int    `json:"active_sessions"`
}

type ConfigRequest struct {
	NodeID     string `json:"node_id"`
	Nonce      string `json:"nonce"`
	ObservedMS int64  `json:"observed_ms"`
}

type UpgradeRequest struct {
	NodeID        string `json:"node_id"`
	TaskID        string `json:"task_id"`
	Nonce         string `json:"nonce"`
	ObservedMS    int64  `json:"observed_ms"`
	Action        string `json:"action"`
	ConfigVersion int64  `json:"config_version"`
	SHA256        string `json:"sha256"`
}

type Upgrade struct {
	TaskID       string `json:"task_id"`
	CandidateSHA string `json:"candidate_sha256"`
	OriginalSHA  string `json:"original_sha256"`
	State        string `json:"state"`
	Ready        bool   `json:"ready"`
}

func (r UpgradeRequest) Frame() []byte { return frame("upgrade", r) }

func (e Enrollment) Frame() []byte    { return frame("enrollment", e) }
func (h Health) Frame() []byte        { return frame("health", h) }
func (r ConfigRequest) Frame() []byte { return frame("configuration", r) }

func frame(purpose string, value any) []byte {
	// Only the fixed structs above are passed here. Their scalar fields have no custom encoders.
	data, _ := json.Marshal(value)
	return append([]byte("DeskRealm/relay/v1/"+purpose+"\x00"), data...)
}

func PublicKey(text string) ed25519.PublicKey {
	raw := decode(text, ed25519.PublicKeySize)
	point, err := new(edwards25519.Point).SetBytes(raw)
	if err != nil || !bytes.Equal(point.Bytes(), raw) ||
		new(edwards25519.Point).MultByCofactor(point).Equal(edwards25519.NewIdentityPoint()) == 1 {
		return nil
	}
	return raw
}

func Verify(key ed25519.PublicKey, message []byte, text string) bool {
	return len(key) == ed25519.PublicKeySize && ed25519.Verify(key, message, decode(text, ed25519.SignatureSize))
}

func decode(text string, size int) []byte {
	if len(text) != base64.RawURLEncoding.EncodedLen(size) {
		return nil
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(text)
	if err != nil || len(raw) != size || base64.RawURLEncoding.EncodeToString(raw) != text {
		return nil
	}
	return raw
}
