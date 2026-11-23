package install

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureConfig(t *testing.T) config {
	t.Helper()
	digest := "@sha256:" + strings.Repeat("a", 64)
	return config{
		Version: 1, Directory: filepath.Join(t.TempDir(), "center data"),
		PublicOrigin: "https://Console.Example.Test:443/", IDServer: "ID.Example.Test:21116",
		HTTPS: httpsConfig{Mode: httpsExternal},
		Images: imageConfig{
			Center:     "registry.example.test:5443/platform/deskrealm:stable" + digest,
			PostgreSQL: "postgres:18.7-bookworm" + digest,
			Rendezvous: "registry.example.test/platform/hbbs" + digest,
		},
	}
}

func TestInstallConfigDefaultsAndCanonicalOrigins(t *testing.T) {
	input := fixtureConfig(t)
	actual, err := validateConfig(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if actual.PublicOrigin != "https://console.example.test" || actual.IDServer != "id.example.test:21116" ||
		actual.Ports != (portConfig{API: 8080, RendezvousCheck: 21115, Rendezvous: 21116}) {
		t.Fatalf("unexpected normalized config: %+v", actual)
	}
	input.PublicOrigin = "https://[2001:DB8::2]:08443"
	input.IDServer = "[2001:DB8::4]:21116"
	actual, err = validateConfig(input, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if actual.PublicOrigin != "https://[2001:db8::2]:8443" || actual.IDServer != "[2001:db8::4]:21116" {
		t.Fatalf("unexpected IPv6 normalization: %+v", actual)
	}
	if _, err := os.Lstat(input.Directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configuration validation must not create the deployment directory, got %v", err)
	}
}

func TestInstallConfigRejectsUnsafeValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*config)
		want string
	}{
		{"version", func(c *config) { c.Version = 2 }, "version"},
		{"root", func(c *config) { c.Directory = "/" }, "filesystem root"},
		{"interpolation", func(c *config) { c.Directory += "$ADDED" }, "dollar signs"},
		{"directory symlink", func(c *config) { c.Directory += "-link" }, "cannot be a symlink"},
		{"insecure", func(c *config) { c.PublicOrigin = "http://example.test" }, "must use HTTPS"},
		{"path", func(c *config) { c.PublicOrigin += "admin" }, "URL origin"},
		{"port", func(c *config) { c.PublicOrigin = "https://example.test:65536" }, "invalid port"},
		{"bad endpoint", func(c *config) { c.IDServer = "id.example.test" }, "with port"},
		{"unpinned", func(c *config) { c.Images.Center = "deskrealm:latest" }, "pinned"},
		{"image injection", func(c *config) { c.Images.Center = "a/../x" + c.Images.Center }, "pinned"},
		{"proxy key", func(c *config) { c.HTTPS.PrivateKey = "/key" }, "reverse proxy"},
		{"duplicate port", func(c *config) { c.Ports.API = 21115 }, "must be distinct"},
		{"invalid local bind", func(c *config) { c.Ports.BindHost = "0.0.0.0" }, "unicast IPv4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := fixtureConfig(t)
			if err := os.Symlink(filepath.Dir(value.Directory), value.Directory+"-link"); err != nil {
				t.Fatal(err)
			}
			tc.edit(&value)
			_, err := validateConfig(value, time.Now())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestInstallConfigFileRejectsUnknownAndExtraInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "center.json")
	c := fixtureConfig(t)
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		string(data) + " {}",
		strings.TrimSuffix(string(data), "}") + ",\"private_token\":\"never expose me\"}",
	} {
		if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path, time.Now()); err == nil || strings.Contains(err.Error(), "never expose me") {
			t.Fatalf("expected rejection without private configuration values, got %v", err)
		}
	}
}

func TestInstallPreflightChecksTLSFiles(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"console.example.test"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &leaf, &leaf, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	c := fixtureConfig(t)
	c.HTTPS = httpsConfig{Mode: httpsProvided, Certificate: filepath.Join(directory, "cert.pem"), PrivateKey: filepath.Join(directory, "key.pem")}
	if err := os.WriteFile(c.HTTPS.Certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.HTTPS.PrivateKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	actual, err := validateConfig(c, now)
	if err != nil || actual.Ports.API != 443 {
		t.Fatalf("provided HTTPS test got %d, %v", actual.Ports.API, err)
	}
	if err := checkCertificate(c.HTTPS, "wrong.example.test", now); err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("expected a hostname error, got %v", err)
	}
	if err := checkCertificate(c.HTTPS, "console.example.test", leaf.NotAfter); err == nil || !strings.Contains(err.Error(), "not currently valid") {
		t.Fatalf("expected expiry rejection, got %v", err)
	}
	if err := os.Chmod(c.HTTPS.PrivateKey, 0644); err != nil {
		t.Fatal(err)
	}
	if err := checkCertificate(c.HTTPS, "console.example.test", now); err == nil || !strings.Contains(err.Error(), "restricted private key") {
		t.Fatalf("expected permission rejection, got %v", err)
	}
}

func TestInstallPreflightRejectsRemoteDockerAndOccupiedPorts(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://localhost:2375")
	t.Setenv("DOCKER_CONTEXT", "")
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "context":
			return []byte("unix:///var/run/docker.sock\n"), nil
		case "version":
			return []byte("linux/arm64\n"), nil
		case "compose":
			return []byte("5.5.0\n"), nil
		default:
			t.Fatalf("unexpected Docker operation %v", args)
			return nil, nil
		}
	}
	ctx := context.Background()
	if err := localDockerEndpoint(ctx, runner); err == nil || !strings.Contains(err.Error(), "Unix socket") {
		t.Fatalf("expected a remote Docker rejection, got %v", err)
	}
	t.Setenv("DOCKER_CONTEXT", "local")
	if err := localDockerEndpoint(ctx, runner); err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	c := fixtureConfig(t)
	c.Ports.API = uint16(tcp.Addr().(*net.TCPAddr).Port)
	if _, err := check(ctx, c, runner); err == nil || !strings.Contains(err.Error(), "api port tcp") {
		t.Fatalf("expected API listener conflict, got %v", err)
	}
	if err := checkPort(ctx, portBinding{Protocol: "udp", Address: udp.LocalAddr().String()}); err == nil {
		t.Fatal("expected occupied UDP port to fail")
	}
}
