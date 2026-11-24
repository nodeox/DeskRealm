package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"

	"deskrealm/internal/managedproto"
	"deskrealm/internal/windowsdelivery"
)

func TestDeploymentRecoversLostRegistrationReceipt(t *testing.T) {
	state, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	const id = "10731001"
	key, err := prepare(state, id)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := prepare(state, id)
	if err != nil || !bytes.Equal(key, reused) {
		t.Fatalf("prepare did not retain device key: %v", err)
	}
	if _, err := prepare(state, "10731002"); err == nil {
		t.Fatal("prepare replaced the RustDesk ID")
	}
	group := "02b52bb4-c440-41b8-b52f-1ffab12096d5"
	registered := registration{Device: "c99bf5a4-1321-45c1-8dd4-b766db22aabd",
		Organization: "2cf7d2e7-ea4d-4971-b185-6dc9cba23d19", State: "pending"}
	credential := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	proof := challenge{ID: "cc2fd016-05fb-48e6-b937-1692a1af1997",
		Nonce: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))}
	signing := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	writeReceipt := func(platform string) {
		t.Helper()
		data, err := json.Marshal(map[string]string{"credential": credential, "group_id": group,
			"platform": platform, "state": "issued"})
		if err != nil {
			t.Fatal(err)
		}
		if err := saveDocument(state, deploymentName, data); err != nil {
			t.Fatal(err)
		}
	}
	challenges, completes := 0, 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/managed/signing-key" && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]string{"kid": "deskrealm", "public_key": signing})
			return
		}
		var request enrollmentRequest
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&request) != nil ||
			request.Credential != credential || request.Group != group || request.Platform != "windows-arm64" ||
			request.RustDeskID != id || request.PublicKey != base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)) {
			t.Error("unexpected enrollment request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/v1/enrollment/challenge":
			challenges++
			if completes > 0 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(proof)
		case "/v1/enrollment/complete":
			signature, err := base64.StdEncoding.DecodeString(request.Signature)
			expected := managedproto.DeviceEnrollmentProof(proof.ID, proof.Nonce, group, request.Platform, id, request.PublicKey)
			if err != nil || request.Attempt != proof.ID || !ed25519.Verify(key.Public().(ed25519.PublicKey), expected, signature) {
				t.Error("incorrect device proof")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			completes++
			if completes == 1 {
				// The server accepted enrollment but the response did not reach the client in full.
				w.Header().Set("Content-Length", "255")
				_, _ = io.WriteString(w, "{")
				return
			}
			_ = json.NewEncoder(w).Encode(registered)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	center := windowsdelivery.Center{Origin: server.URL, SigningKeyID: "deskrealm", SigningKey: signing}
	writeReceipt("windows-amd64")
	if _, err := bootstrap(t.Context(), server.Client(), center, state, "windows-arm64", id, key); err == nil {
		t.Fatal("accepted a credential for another architecture")
	}
	if err := state.Remove(deploymentName); err != nil {
		t.Fatal(err)
	}
	writeReceipt("windows-arm64")
	wrongCenter := center
	wrongCenter.SigningKeyID = "different"
	if _, err := bootstrap(t.Context(), server.Client(), wrongCenter, state, "windows-arm64", id, key); err == nil {
		t.Fatal("accepted a different center key")
	}
	if _, err := bootstrap(t.Context(), server.Client(), center, state, "windows-arm64", id, key); err == nil {
		t.Fatal("accepted a truncated completion")
	}
	if got, err := bootstrap(t.Context(), server.Client(), center, state, "windows-arm64", id, key); err != nil || got != registered {
		t.Fatalf("could not recover original completion: %+v %v", got, err)
	}
	server.Close()
	if got, err := bootstrap(t.Context(), server.Client(), center, state, "windows-arm64", id, key); err != nil || got != registered {
		t.Fatalf("restart did not retain registration: %+v %v", got, err)
	}
	if _, err := state.Stat(deploymentName); !os.IsNotExist(err) || challenges != 1 || completes != 2 {
		t.Fatalf("unexpected deployment residue or request counts: challenges=%d completes=%d file=%v", challenges, completes, err)
	}
}

func TestDesktopUsesVerifiedProfileSettings(t *testing.T) {
	env := desktopEnvironment([]string{"deskrealm_managed_center=unsafe", "DESKREALM_MANAGED_CA_FILE=untrusted",
		"DESKREALM_MANAGED_RELAY_ADDRESS=wrong", "PATH=example"},
		windowsdelivery.Center{Origin: "https://center.test", IDServer: "center.test:21116",
			DefaultRelay: &windowsdelivery.Relay{ID: "regional", Address: "relay.test:21117"}},
		registration{Device: "device", Organization: "organization"}, "desktop1", "seed")
	for _, want := range []string{"PATH=example", "DESKREALM_MANAGED_CENTER=https://center.test",
		"DESKREALM_MANAGED_RENDEZVOUS_ADDRESS=center.test:21116", "DESKREALM_MANAGED_RELAY_ADDRESS=relay.test:21117",
		"DESKREALM_MANAGED_DEVICE_SEED_FILE=seed", "DESKREALM_MANAGED_ORGANIZATION_ID=organization"} {
		if !slices.Contains(env, want) {
			t.Errorf("missing child setting %q", want)
		}
	}
	for _, unwanted := range []string{"deskrealm_managed_center=unsafe", "DESKREALM_MANAGED_CA_FILE=untrusted",
		"DESKREALM_MANAGED_RELAY_ADDRESS=wrong"} {
		if slices.Contains(env, unwanted) {
			t.Errorf("inherited child setting %q", unwanted)
		}
	}
}
