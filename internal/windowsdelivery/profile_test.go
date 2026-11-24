package windowsdelivery

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestParsePublicProfile(t *testing.T) {
	valid := PublicProfile{Schema: 1, Version: "1.0.0-dev.1", Center: Center{
		Origin: "https://center.example.test", IDServer: "[2001:db8::1]:21116", SigningKeyID: "center-1",
		RendezvousKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)),
		SigningKey:    base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)),
		DefaultRelay:  &Relay{ID: "regional", Address: "relay.example.test:21117"},
	}}
	parse := func(p PublicProfile) error {
		t.Helper()
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ParsePublicProfile(raw)
		return err
	}
	if err := parse(valid); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
	for _, invalid := range []string{"http://center.example.test", "https://center.example.test:443",
		"https://center.example.test/", "https://center.example.test?", "https://guest@center.example.test"} {
		changed := valid
		changed.Center.Origin = invalid
		if err := parse(changed); err == nil {
			t.Errorf("accepted profile with origin %q", invalid)
		}
	}
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	withCredential := append([]byte(`{"credential":"forbidden",`), raw[1:]...)
	if _, err := ParsePublicProfile(withCredential); err == nil {
		t.Fatal("public profile accepted deployment credentials")
	}
}
