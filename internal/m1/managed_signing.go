package m1

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
)

type ManagedSigningKey struct {
	id  string
	key ed25519.PrivateKey
}

var signingKeyID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// LoadManagedSigningKey reads a deployed Ed25519 seed in unpadded base64url.
// An empty configuration keeps managed issuance disabled.
func LoadManagedSigningKey(path, id string) (*ManagedSigningKey, error) {
	if path == "" && id == "" {
		return nil, nil
	}
	if path == "" || !signingKeyID.MatchString(id) {
		return nil, errors.New("managed signing key file and valid key ID are required")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("managed signing key file unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("managed signing key file unavailable")
	}
	defer file.Close()
	value, err := io.ReadAll(io.LimitReader(file, 46))
	if err != nil {
		return nil, errors.New("managed signing key file unreadable")
	}
	encoded, hadNewline := strings.CutSuffix(string(value), "\n")
	if hadNewline {
		encoded = strings.TrimSuffix(encoded, "\r")
	}
	if !validToken(encoded) {
		return nil, errors.New("managed signing key file must contain one base64url Ed25519 seed")
	}
	seed, _ := base64.RawURLEncoding.Strict().DecodeString(encoded)
	return &ManagedSigningKey{id: id, key: ed25519.NewKeyFromSeed(seed)}, nil
}
