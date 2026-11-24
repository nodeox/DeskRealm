package m1

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"deskrealm/internal/relayprotocol"
)

func relayFixtureListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func relayFixturePublish(t *testing.T) (string, *atomic.Pointer[string]) {
	t.Helper()
	listener := relayFixtureListener(t)
	var backend atomic.Pointer[string]
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				address := backend.Load()
				if address == nil {
					return
				}
				server, err := net.DialTimeout("tcp", *address, time.Second)
				if err != nil {
					return
				}
				defer server.Close()
				deadline := time.Now().Add(3 * time.Second)
				_ = client.SetDeadline(deadline)
				_ = server.SetDeadline(deadline)
				forwarded := make(chan struct{})
				go func() {
					defer close(forwarded)
					_, _ = io.Copy(server, client)
					_ = server.Close()
				}()
				_, _ = io.Copy(client, server)
				_ = client.Close()
				<-forwarded
			}()
		}
	}()
	return listener.Addr().String(), &backend
}

func TestRegionalRelayRuntimePostgres(t *testing.T) {
	binary, hbbr := os.Getenv("DESKREALM_RELAY_TEST_BINARY"), os.Getenv("DESKREALM_TEST_HBBR_RUNNER")
	if binary == "" || hbbr == "" {
		t.Skip("run scripts/deskrealm-m3-test relay-enrollment")
	}
	f := newSessionFixture(t)
	ctx, store, h, admin := f.ctx, f.store, f.h, f.admin
	csrf := csrfToken(admin.Value)
	center := httptest.NewTLSServer(h)
	defer center.Close()
	scratch := t.TempDir()
	ca := filepath.Join(scratch, "center-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: center.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) []byte {
		t.Helper()
		out, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("relay %s: %v\n%s", args[0], err, out)
		}
		return out
	}
	type node struct {
		relayCredential
		state, public, local, port string
		published                  *atomic.Pointer[string]
		listener                   net.Listener
	}
	create := func(name string) node {
		t.Helper()
		endpoint, published := relayFixturePublish(t)
		record := request(h, "POST", "/v1/admin/relays", testOrigin,
			relayRequest{name, "site-" + name, "north", endpoint, "1.1.16", 2}, admin, csrf)
		checkCode(t, record, 201, relayEnrollmentPrefix)
		cred := responseValue[relayCredential](t, record.Body.Bytes())
		state, credential := filepath.Join(scratch, name), filepath.Join(scratch, name+".credential")
		if err := os.WriteFile(credential, []byte(cred.Credential), 0600); err != nil {
			t.Fatal(err)
		}
		command("prepare", "--state", state, "--center", center.URL, "--node", cred.NodeID)
		out := command("enroll", "--state", state, "--ca", ca, "--credential-file", credential)
		if err := os.Remove(credential); err != nil {
			t.Fatal(err)
		}
		view := responseValue[struct {
			PublicKey string `json:"public_key"`
		}](t, out)
		local := relayFixtureListener(t)
		address := local.Addr().String()
		_, port, _ := net.SplitHostPort(address)
		return node{cred, state, view.PublicKey, address, port, published, local}
	}
	start := func(node node) func() {
		t.Helper()
		_ = node.listener.Close()
		var output bytes.Buffer
		processCtx, cancel := context.WithCancel(ctx)
		args := []string{"run", "--state", node.state, "--hbbr", hbbr, "--port", node.port, "--ca", ca}
		cmd := exec.CommandContext(processCtx, binary, args...)
		cmd.Stdout, cmd.Stderr = &output, &output
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 6 * time.Second
		if err := cmd.Start(); err != nil {
			cancel()
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cancel()
			if err := <-exited; err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("relay runtime: %v\n%s", err, &output)
			}
		}
		t.Cleanup(stop)
		return stop
	}
	waitHealth := func(node node, after int64) int64 {
		t.Helper()
		until := time.Now().Add(12 * time.Second)
		for time.Now().Before(until) {
			var sequence int64
			var ready bool
			if err := store.DB.QueryRow(ctx, `SELECT health_sequence,coalesce(protocol_ready AND health_config_version=config_version,false)
				FROM relay_nodes WHERE id=$1`, node.NodeID).Scan(&sequence, &ready); err != nil {
				t.Fatal(err)
			}
			if ready && sequence > after {
				return sequence
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("no fresh signed health from relay", node.NodeID)
		return 0
	}
	checkStates := func(a, b string) {
		t.Helper()
		reply := request(h, "GET", "/v1/admin/relays", "", nil, admin, "")
		checkCode(t, reply, 200, "")
		nodes := responseValue[struct{ Nodes []relayNodeSummary }](t, reply.Body.Bytes()).Nodes
		if len(nodes) != 2 || nodes[0].State != a || nodes[1].State != b {
			t.Fatalf("expected relay states %q and %q, received %+v", a, b, nodes)
		}
	}
	sweep := func() {
		t.Helper()
		taskSQL(t, ctx, store, "UPDATE relay_nodes SET probe_checked_at=NULL")
		if err := store.probeDueRelays(ctx); err != nil {
			t.Fatal(err)
		}
	}
	a, b := create("alpha"), create("beta")
	if a.public == b.public {
		t.Fatal("regional relays share an identity")
	}
	checkStates("awaiting_health", "awaiting_health")
	stopA, _ := start(a), start(b)
	sequence := waitHealth(a, 0)
	waitHealth(b, 0)
	checkStates("awaiting_probe", "awaiting_probe")
	a.published.Store(&b.local)
	b.published.Store(&b.local)
	sweep()
	checkStates("awaiting_probe", "available")
	a.published.Store(&a.local)
	sweep()
	checkStates("available", "available")
	sampleRegionalRelays(t, f, a.NodeID, b.NodeID)
	for _, n := range []node{a, b} {
		checkCode(t, request(h, "GET", tasksPath+"/"+n.TaskID, "", nil, admin, ""), 200, "relay_available")
	}
	a.published.Store(&b.local)
	sweep()
	checkStates("available", "available")
	sweep()
	checkStates("available", "available")
	sweep()
	checkStates("probe_unreachable", "available")
	a.published.Store(&a.local)
	sweep()
	checkStates("available", "available")
	taskSQL(t, ctx, store, "UPDATE relay_nodes SET active_sessions=capacity WHERE id=$1", a.NodeID)
	checkStates("at_capacity", "available")
	stopA()
	taskSQL(t, ctx, store, "UPDATE relay_nodes SET health_at=statement_timestamp()-interval '16 seconds' WHERE id=$1", a.NodeID)
	checkStates("awaiting_health", "available")
	start(a)
	sequence = waitHealth(a, sequence)
	sweep()
	checkStates("available", "available")
	newEndpoint, newPublished := relayFixturePublish(t)
	newPublished.Store(&a.local)
	change := request(h, "POST", "/v1/admin/relays/"+a.NodeID+"/lifecycle", testOrigin,
		relayChange{"change_endpoint", 1, newEndpoint}, admin, csrf)
	checkCode(t, change, 200, "relay_endpoint_changed")
	waitHealth(a, sequence)
	sweep()
	checkStates("available", "available")
	out := command("status", "--state", a.state)
	if !strings.Contains(string(out), a.public) || !strings.Contains(string(out), newEndpoint) {
		t.Fatal("runtime restart or address change lost the relay identity or approved configuration")
	}
	var registrations, admissions int
	err := store.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE action='relay.registered'),
		count(*) FILTER (WHERE action='relay.available') FROM audit_events`).Scan(&registrations, &admissions)
	if err != nil || registrations != 2 || admissions != 2 {
		t.Fatalf("expected one registration and admission audit per node: %d %d %v", registrations, admissions, err)
	}
}

func sampleRegionalRelays(t *testing.T, f sessionFixture, alpha, beta string) {
	t.Helper()
	grant := relayRoundSession(t, f, 591)
	controller, target := f.roundPeers()
	conn := testSessionUUID(592)
	checkCode(t, controller.join(t, f.h, grant, conn), 200, "waiting_for_target")
	response := target.join(t, f.h, grant, conn)
	checkCode(t, response, 200, "collecting")
	round := responseValue[relayRound](t, response.Body.Bytes())
	if len(round.Candidates) != 2 || round.Deadline == nil {
		t.Fatalf("expected both enrolled native nodes: %+v", round)
	}
	ctx, stop := context.WithDeadline(f.ctx, *round.Deadline)
	defer stop()
	type measured struct {
		peer      roundPeer
		candidate relayRoundCandidate
		samples   []relayProbeSample
		err       error
	}
	results := make(chan measured, 4)
	for _, peer := range []roundPeer{controller, target} {
		for _, candidate := range []relayRoundCandidate{roundCandidate(t, round, alpha), roundCandidate(t, round, beta)} {
			go func() {
				result := measured{peer: peer, candidate: candidate, samples: make([]relayProbeSample, 0, 3)}
				for range 3 {
					started := time.Now()
					_, err := relayprotocol.Probe(ctx, candidate.Endpoint, relayprotocol.PublicKey(candidate.PublicKey), candidate.Build)
					if err != nil {
						result.err = err
						break
					}
					result.samples = append(result.samples, measuredRTT(uint32(time.Since(started).Microseconds())))
				}
				results <- result
			}()
		}
	}
	for range cap(results) {
		result := <-results
		if result.err != nil {
			t.Fatalf("could not measure native relay %s from role %d: %v", result.candidate.ID, result.peer.role, result.err)
		}
		checkCode(t, result.peer.report(t, f.h, grant, round, result.candidate, result.samples), 204, "")
	}
	response = controller.get(f.h, grant, conn)
	checkCode(t, response, 200, round.ID)
	observed := responseValue[relayRound](t, response.Body.Bytes())
	for _, candidate := range observed.Candidates {
		if !candidate.Eligible || candidate.Controller.Median == nil || candidate.Target.Median == nil {
			t.Fatalf("missing one of the native RTT results: %+v", candidate)
		}
	}
	f.count(t, 4, "SELECT count(*) FROM session_relay_reports WHERE round_id=$1", round.ID)
}
