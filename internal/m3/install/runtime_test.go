package install

import (
	"strings"
	"testing"
)

func TestRuntimePlanPinsSupportedHosts(t *testing.T) {
	if sha256Hex(dockerReleaseKey) != dockerKeySHA256 {
		t.Fatal("APT trust root does not match the installer pin")
	}
	for _, release := range []struct{ id, version, suite string }{
		{"debian", "12", "bookworm"}, {"ubuntu", "24.04", "noble"},
	} {
		for _, arch := range []string{"amd64", "arm64"} {
			raw := []byte("ID=" + release.id + "\nVERSION_ID=\"" + release.version + "\"\nVERSION_CODENAME=" + release.suite)
			plan, err := planRuntime(raw, arch)
			if err != nil || plan.Arch != arch || plan.Source != "https://download.docker.com/linux/"+release.id || len(plan.Packages) != 4 {
				t.Fatalf("unexpected Docker selection for %s/%s: %+v: %v", release.id, arch, plan, err)
			}
			for _, p := range plan.Packages {
				if !strings.HasSuffix(p.Version, "~"+release.id+"."+release.version+"~"+release.suite) {
					t.Fatalf("package escaped the distribution pin: %+v", p)
				}
			}
			if _, err := planRuntime(raw, "386"); err == nil {
				t.Fatal("unsupported architecture was accepted")
			}
		}
	}
	if _, err := planRuntime([]byte("ID=ubuntu\nVERSION_ID=24.04\nVERSION_CODENAME=jammy"), "amd64"); err == nil {
		t.Fatal("mismatched release and suite were accepted")
	}
}
