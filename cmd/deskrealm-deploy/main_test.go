package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeploymentToolPublishesOnlyVerifiedOutputs(t *testing.T) {
	const release = "4b8443b4-41cf-47bb-adda-7ff02f4ade70"
	const group = "a14a87d5-3986-44e5-be62-7d84be63c3d9"
	const deployment = "8e04be9d-e574-4767-bd01-bb817fbf9178"
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	credential := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	publicKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	install := "test signed installer"
	sum := sha256.Sum256([]byte(install))
	base := "/v1/deployment/windows-releases/" + release
	var rolloutOnly atomic.Bool
	receipt := enrollmentReceipt{Deployment: deployment, Group: group, Release: release, Platform: "windows-arm64",
		State: "issued", Credential: credential, Expires: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Cookie") != "" {
			t.Error("incorrect authentication")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET " + base:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": release, "rollout_only": rolloutOnly.Load(), "installer_publication": map[string]any{
				"installers": map[string]any{
					"arm64": map[string]any{"file": "DeskRealm-Setup-arm64.exe", "bytes": len(install), "sha256": hex.EncodeToString(sum[:])},
					"x64":   map[string]any{"file": "DeskRealm-Setup-x64.exe", "bytes": len(install), "sha256": hex.EncodeToString(sum[:])},
				},
			}})
		case "GET " + base + "/installers/arm64":
			_, _ = io.WriteString(w, install)
		case "GET " + base + "/rollouts/" + deployment + "/" + group:
			_, _ = io.WriteString(w, install)
		case "GET " + base + "/installers/x64":
			_, _ = io.WriteString(w, "changed")
		case "POST " + base + "/deployments":
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["public_key"] != publicKey ||
				body["rustdesk_id"] != "windows01" || body["deployment_id"] != deployment ||
				body["group_id"] != group || body["arch"] != "arm64" || body["deployment_file"] != nil {
				t.Error("request did not match authorized device")
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(receipt)
		case "GET " + base + "/deployments/" + deployment:
			_ = json.NewEncoder(w).Encode(map[string]any{"release_id": release, "deployment_id": deployment,
				"state": "consumed", "registration_state": "pending", "credential": credential})
		case "POST " + base + "/deployments/" + deployment + "/revoke":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := deploymentClient{http: server.Client(), base: server.URL + base, token: token}
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	targetJSON, err := json.Marshal(map[string]string{"arch": "arm64", "rustdesk_id": "windows01", "public_key": publicKey,
		"deployment_file": filepath.Join(dir, "unexpected-output")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, targetJSON, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	o := options{release: release, group: group, deployment: deployment, arch: "arm64", target: target,
		output: filepath.Join(dir, "setup.exe"), purpose: "test", lifetime: 20}
	for range 2 {
		if err := c.execute("fetch", o, &out); err != nil {
			t.Fatal(err)
		}
	}
	if raw, err := os.ReadFile(o.output); err != nil || string(raw) != install {
		t.Fatalf("wrong installer: %v", err)
	}
	o.arch, o.output = "x64", filepath.Join(dir, "changed.exe")
	if err := c.execute("fetch", o, &out); err == nil {
		t.Fatal("wrote a changed installer")
	}
	if _, err := os.Stat(o.output); !os.IsNotExist(err) {
		t.Fatalf("left unverified installer: %v", err)
	}
	rolloutOnly.Store(true)
	o.arch, o.output = "arm64", filepath.Join(dir, "batch.exe")
	if err := c.execute("fetch", o, &out); err == nil {
		t.Fatal("used an ordinary download for a batch-only release")
	}
	o.rollout, o.device = deployment, group
	if err := c.execute("fetch", o, &out); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(o.output); err != nil || string(raw) != install {
		t.Fatalf("wrong batch installer: %v", err)
	}
	rolloutOnly.Store(false)
	o.output = filepath.Join(dir, "deployment.json")
	for range 2 {
		if err := c.execute("issue", o, &out); err != nil {
			t.Fatal(err)
		}
	}
	if raw, err := os.ReadFile(o.output); err != nil || !bytes.Contains(raw, []byte(credential)) {
		t.Fatalf("credential absent from protected output: %v", err)
	}
	if err := c.execute("status", o, &out); err != nil {
		t.Fatal(err)
	}
	if err := c.execute("revoke", o, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), credential) || strings.Contains(out.String(), token) ||
		!strings.Contains(out.String(), "pending") {
		t.Fatal("unexpected public deployment output")
	}
}
