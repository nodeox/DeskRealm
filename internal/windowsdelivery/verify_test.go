package windowsdelivery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPythonDevelopmentEvidence(t *testing.T) {
	source := os.Getenv("DESKREALM_TEST_WINDOWS_DEVELOPMENT_EVIDENCE")
	if source == "" {
		t.Skip("set DESKREALM_TEST_WINDOWS_DEVELOPMENT_EVIDENCE to a Python signing result")
	}
	raw, err := os.ReadFile(filepath.Join(source, "bundle", "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := strictJSON(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	spool := t.TempDir()
	id := digest(raw)
	dir := filepath.Join(spool, id)
	if err := os.MkdirAll(filepath.Join(dir, "bundle"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][2]string{
		{"bundle/release.json", "release.json"},
		{"release.sig", "release.sig"},
		{"bundle/DeskRealm-x64.zip", "bundle/DeskRealm-x64.zip"},
		{"bundle/DeskRealm-arm64.zip", "bundle/DeskRealm-arm64.zip"},
	} {
		payload, err := os.ReadFile(filepath.Join(source, paths[0]))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, paths[1]), payload, 0600); err != nil {
			t.Fatal(err)
		}
	}
	verifier, err := Configure(spool, filepath.Join(source, "development.public.pem"), manifest.Approval.Center)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifier.Verify(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	releases, err := verifier.ConfigureReleases(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releases.Preserve(context.Background(), verifier, id); err != nil {
		t.Fatal(err)
	}
	for arch, pkg := range verified.Packages {
		file, err := releases.Open(context.Background(), id, arch, pkg)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
	}
	t.Logf("Verified and preserved both native bundles for %s (%s)", verified.Approval.Version, verified.Approval.Source)
}
