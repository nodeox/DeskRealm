package managedproto

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestSharedSigningVectors(t *testing.T) {
	data, err := os.ReadFile("../../managed/tests/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Version int
		Vectors []struct {
			Name      string
			Kind      Kind
			Fields    []string `json:"fields_hex"`
			Frame     string   `json:"frame_hex"`
			Seed      string   `json:"seed_hex"`
			Public    string   `json:"public_key_hex"`
			Signature string   `json:"signature_hex"`
			Reject    bool
		}
	}
	if err := json.Unmarshal(data, &fixtures); err != nil || fixtures.Version != 1 {
		t.Fatalf("wrong fixture format: %v", err)
	}
	passed := make(map[Kind]bool)
	rejected := 0
	for _, vector := range fixtures.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			decode := func(s string) []byte {
				data, err := hex.DecodeString(s)
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			fields := make([][]byte, 0, len(vector.Fields))
			for _, value := range vector.Fields {
				fields = append(fields, decode(value))
			}
			frame, err := Encode(vector.Kind, fields...)
			if vector.Reject {
				if !errors.Is(err, ErrFrame) {
					t.Fatalf("accepted invalid frame: %v", err)
				}
				rejected++
				return
			}
			if err != nil || !bytes.Equal(frame, decode(vector.Frame)) {
				t.Fatalf("canonical frame differs: %x (%v)", frame, err)
			}
			private := ed25519.NewKeyFromSeed(decode(vector.Seed))
			public := private.Public().(ed25519.PublicKey)
			signature := decode(vector.Signature)
			if !bytes.Equal(public, decode(vector.Public)) || !bytes.Equal(ed25519.Sign(private, frame), signature) ||
				!ed25519.Verify(public, frame, signature) {
				t.Fatal("Ed25519 vector differs")
			}
			frame[len(prefix)] ^= 1
			if ed25519.Verify(public, frame, signature) {
				t.Fatal("changed frame kind verified")
			}
			passed[vector.Kind] = true
		})
	}
	if len(passed) != 4 || rejected != 13 {
		t.Fatalf("missing fixture coverage: %v, %d rejected", passed, rejected)
	}
}
