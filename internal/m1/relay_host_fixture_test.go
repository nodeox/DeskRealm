package m1

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"deskrealm/internal/relayprotocol"
)

func TestRelayHostEnrollmentPostgres(t *testing.T) {
	binary := os.Getenv("DESKREALM_RELAY_TEST_BINARY")
	if binary == "" {
		t.Skip("run scripts/deskrealm-m3-test relay-host")
	}
	ctx, store, _ := postgres(t)
	h, admin, csrf, _ := adminSession(t, ctx, store)
	var completions, configurationReads atomic.Int32
	server := httptest.NewUnstartedServer(nil)
	origin, err := ParseOrigin("https://"+server.Listener.Addr().String(), false)
	if err != nil {
		t.Fatal(err)
	}
	center := NewHandler(store, origin, nil)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/relays/enrollment/complete":
			sequence := completions.Add(1)
			if sequence == 1 {
				response := httptest.NewRecorder()
				center.ServeHTTP(response, r)
				if response.Code == http.StatusOK {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(response.Code)
				return
			}
		case "/v1/relays/configuration":
			configurationReads.Add(1)
		}
		center.ServeHTTP(w, r)
	})
	server.StartTLS()
	defer server.Close()
	scratch := t.TempDir()
	caFile := filepath.Join(scratch, "trust.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, cert, 0600); err != nil {
		t.Fatal(err)
	}
	type hostStatus struct {
		NodeID    string                      `json:"node_id"`
		Phase     string                      `json:"phase"`
		PublicKey string                      `json:"public_key"`
		Config    relayprotocol.Configuration `json:"configuration"`
	}
	run := func(args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("relay host %s failed: %v\n%s", args[0], err, output)
		}
		return string(output)
	}
	reject := func(reason string, args ...string) {
		t.Helper()
		output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
		if err == nil || !strings.Contains(string(output), reason) {
			t.Fatalf("expected relay command to reject %q: %v\n%s", reason, err, output)
		}
	}
	register := func(name, endpoint string, drop bool) hostStatus {
		t.Helper()
		issue := request(h, "POST", "/v1/admin/relays", testOrigin,
			relayRequest{name, "site-" + name, "north", endpoint, "1.1.16", 8}, admin, csrf)
		checkCode(t, issue, 201, relayEnrollmentPrefix)
		cred := responseValue[relayCredential](t, issue.Body.Bytes())
		state := filepath.Join(scratch, name)
		credential := filepath.Join(scratch, name+".credential")
		if err := os.WriteFile(credential, []byte(cred.Credential+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		args := []string{"--state", state}
		prepare := []string{"prepare", "--state", state, "--center", server.URL, "--node", cred.NodeID}
		first := run(prepare...)
		if run(prepare...) != first {
			t.Fatal("preparing the relay twice changed its identity")
		}
		preflight := responseValue[hostStatus](t, []byte(first))
		if preflight.Phase != "prepared" || relayprotocol.PublicKey(preflight.PublicKey) == nil {
			t.Fatal("invalid prepared relay identity")
		}
		identity := filepath.Join(state, "relay-node.json")
		if info, err := os.Stat(identity); err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("relay identity is not private")
		}
		if drop {
			reject("certificate", append([]string{"enroll"}, args...)...)
			reject("completion: center returned HTTP 503", "enroll", "--state", state, "--ca", caFile, "--credential-file", credential)
			if run("status", "--state", state) != first {
				t.Fatal("failed response changed local enrollment state")
			}
			if err := os.Remove(credential); err != nil {
				t.Fatal(err)
			}
		}
		enroll := []string{"enroll", "--state", state, "--ca", caFile}
		if !drop {
			enroll = append(enroll, "--credential-file", credential)
		}
		enrolled := run(enroll...)
		after := responseValue[hostStatus](t, []byte(enrolled))
		if after.Phase != "enrolled" || after.NodeID != cred.NodeID || after.Config.Endpoint != endpoint ||
			after.PublicKey != preflight.PublicKey || strings.Contains(enrolled, cred.Credential) {
			t.Fatalf("unexpected relay configuration or public identity: %+v", after)
		}
		if run("enroll", "--state", state, "--ca", caFile) != enrolled || run("status", "--state", state) != enrolled {
			t.Fatal("enrollment restart changed the saved node identity")
		}
		reject("already bound", "prepare", "--state", state, "--center", "https://different.example.test", "--node", cred.NodeID)
		if err := os.Remove(identity); err != nil {
			t.Fatal(err)
		}
		reject("restore the original node state", prepare...)
		return after
	}
	a := register("alpha", "relay-alpha.example.test:21117", true)
	b := register("beta", "relay-beta.example.test:21117", false)
	if a.PublicKey == b.PublicKey || completions.Load() != 2 || configurationReads.Load() != 5 {
		t.Fatalf("wrong node identities or enrollment replay count: completions=%d config=%d", completions.Load(), configurationReads.Load())
	}
	var count int
	if err := store.DB.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action='relay.registered'").Scan(&count); err != nil || count != 2 {
		t.Fatalf("unexpected relay registration audits: count=%d err=%v", count, err)
	}
}
