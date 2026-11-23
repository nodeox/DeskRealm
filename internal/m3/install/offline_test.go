package install

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func fixtureBundle(t *testing.T) (config, offlineManifest, ed25519.PrivateKey) {
	t.Helper()
	c := preparedConfig(t)
	bundle := t.TempDir()
	c.Offline = offlineConfig{filepath.Join(bundle, "manifest.json"), filepath.Join(bundle, "manifest.sig"), filepath.Join(t.TempDir(), "trusted.pem")}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, c.Offline.PublicKey, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	var m offlineManifest
	m.Version, m.Platform = 1, "linux/arm64"
	archive := []byte("test archive bytes")
	m.Archive.File, m.Archive.Bytes, m.Archive.SHA256 = "images.tar", int64(len(archive)), sha256Hex(archive)
	writeFixture(t, filepath.Join(bundle, m.Archive.File), archive)
	m.Images.Center = offlineImage{c.Images.Center, "sha256:" + strings.Repeat("1", 64)}
	m.Images.PostgreSQL = offlineImage{c.Images.PostgreSQL, "sha256:" + strings.Repeat("2", 64)}
	m.Images.Rendezvous = offlineImage{c.Images.Rendezvous, "sha256:" + strings.Repeat("3", 64)}
	signFixture(t, c, m, private)
	return c, m, private
}

func writeFixture(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func signFixture(t *testing.T, c config, m offlineManifest, private ed25519.PrivateKey) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, c.Offline.Manifest, data)
	writeFixture(t, c.Offline.Signature, ed25519.Sign(private, data))
}

func TestOfflineManifestBindsSignatureReferencesAndArchivePath(t *testing.T) {
	c, m, key := fixtureBundle(t)
	if _, err := readOfflineBundle(c); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, c.Offline.Signature, make([]byte, ed25519.SignatureSize))
	if _, err := readOfflineBundle(c); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("changed signature accepted: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*offlineManifest)
		want string
	}{
		{"other source", func(m *offlineManifest) { m.Images.Center.Reference = m.Images.PostgreSQL.Reference }, "image references"},
		{"traversal", func(m *offlineManifest) { m.Archive.File = "../images.tar" }, "archive basename"},
		{"wrong platform", func(m *offlineManifest) { m.Platform = "windows/arm64" }, "supported platform"},
		{"mutable image", func(m *offlineManifest) { m.Images.Center.ID = "center:latest" }, "SHA-256 image IDs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := m
			tc.edit(&changed)
			signFixture(t, c, changed, key)
			if _, err := readOfflineBundle(c); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invalid signed manifest accepted: %v", err)
			}
		})
	}
}

func TestOfflineImportVerifiesBytesAndLoadedImageIdentity(t *testing.T) {
	c, m, key := fixtureBundle(t)
	prepared, err := prepareDeployment(c)
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.ReadFile(prepared.ComposeFile)
	if err != nil || !strings.Contains(string(base), m.Images.Center.ID) || strings.Contains(string(base), c.Images.Center) {
		t.Fatal("Compose did not pin the signed local image ID")
	}
	awaited := []string{m.Images.Center.ID, m.Images.PostgreSQL.ID, m.Images.Rendezvous.ID}
	var imports int
	badPlatform := false
	run := func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) == 5 && args[0] == "image" && args[1] == "load":
			imports++
			staged, err := os.ReadFile(args[4])
			if err != nil || sha256Hex(staged) != m.Archive.SHA256 {
				t.Fatal("Docker did not receive the verified archive")
			}
			return nil, nil
		case len(args) == 5 && args[0] == "image" && args[1] == "inspect" && slices.Contains(awaited, args[4]):
			if imports == 0 {
				return nil, errors.New("not loaded")
			}
			platform := m.Platform
			if badPlatform {
				platform = "linux/amd64"
			}
			return []byte(args[4] + " " + platform + "\n"), nil
		default:
			t.Fatalf("unexpected Docker command: %v", args)
			return nil, nil
		}
	}
	ctx := context.Background()
	archive := filepath.Join(filepath.Dir(c.Offline.Manifest), m.Archive.File)
	if err := ensureOfflineImages(ctx, c, prepared.Offline, "linux/amd64", false, run); err == nil || imports != 0 {
		t.Fatal("wrong engine platform reached Docker")
	}
	// A different validly signed manifest cannot replace the prepared bundle.
	changed := m
	changed.Archive.File = "other.tar"
	signFixture(t, c, changed, key)
	if err := ensureOfflineImages(ctx, c, prepared.Offline, m.Platform, false, run); err == nil || imports != 0 {
		t.Fatal("different signed manifest reached Docker")
	}
	signFixture(t, c, m, key)
	writeFixture(t, archive, []byte("test archive byte!"))
	if err := ensureOfflineImages(ctx, c, prepared.Offline, m.Platform, false, run); err == nil || imports != 0 {
		t.Fatal("changed archive reached Docker")
	}
	writeFixture(t, archive, []byte("test archive bytes"))
	badPlatform = true
	if err := ensureOfflineImages(ctx, c, prepared.Offline, m.Platform, false, run); err == nil || imports != 1 {
		t.Fatal("wrong loaded platform was not rejected")
	}
	badPlatform = false
	if err := ensureOfflineImages(ctx, c, prepared.Offline, m.Platform, false, run); err != nil || imports != 2 {
		t.Fatalf("verified archive was not accepted: %v", err)
	}
	if err := os.Remove(c.Offline.Manifest); err != nil {
		t.Fatal(err)
	}
	if err := ensureOfflineImages(ctx, c, prepared.Offline, m.Platform, true, run); err != nil || imports != 2 {
		t.Fatalf("active deployment failed to reuse pinned cached images: %v", err)
	}
}
