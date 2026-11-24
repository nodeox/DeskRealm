package windowsdelivery

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPythonInstallerEvidence(t *testing.T) {
	input := os.Getenv("DESKREALM_TEST_INSTALLER_INTEROP")
	if input == "" {
		t.Skip("invoked by the Authenticode installer test")
	}
	var config struct {
		Stage          string `json:"stage"`
		ZipKey         string `json:"zip_key"`
		AttestationKey string `json:"attestation_key"`
		CertificateSHA string `json:"certificate_sha"`
		EvidenceSHA    string `json:"evidence_sha"`
		ReleaseSHA     string `json:"release_sha"`
		Center         Center `json:"center"`
	}
	if err := json.Unmarshal([]byte(input), &config); err != nil {
		t.Fatal(err)
	}
	zipVerifier, err := Configure(config.Stage, config.ZipKey, config.Center)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := zipVerifier.ConfigureInstallers(config.AttestationKey, config.CertificateSHA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zipVerifier.ConfigureInstallers(config.ZipKey, config.CertificateSHA); err == nil {
		t.Fatal("ZIP key also authorized as installer attestation key")
	}
	releases, err := zipVerifier.ConfigureReleases(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := releases.PreserveInstallers(t.Context(), verifier, config.EvidenceSHA, config.ReleaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	for arch, pkg := range evidence.Installers {
		f, err := releases.OpenInstaller(t.Context(), config.EvidenceSHA, arch, pkg)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
}
