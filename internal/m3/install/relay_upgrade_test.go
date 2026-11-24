package install

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelayUpgradeRefusesAlteredFileAndOccupiedPort(t *testing.T) {
	source := filepath.Join(t.TempDir(), "hbbr")
	if err := os.WriteFile(source, []byte("approved relay"), 0700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := stageRelayBinary(dir, source, strings.Repeat("a", 64)); err == nil {
		t.Fatal("accepted a file that differs from its authorized digest")
	}
	digest, err := stageRelayBinary(dir, source, "")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	s := relaySupervisor{dir: dir, endpoint: listener.Addr().String(), start: func(string) (*relayProcess, error) {
		t.Fatal("attempted start with a listener already occupying the relay port")
		return nil, nil
	}}
	if err := s.use(t.Context(), digest); err == nil || !strings.Contains(err.Error(), "occupied") {
		t.Fatalf("expected a port conflict, got %v", err)
	}
}
