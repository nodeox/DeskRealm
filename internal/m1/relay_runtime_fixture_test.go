package m1

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
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

	"deskrealm/internal/managedproto"
	"deskrealm/internal/relayprotocol"
	"github.com/jackc/pgx/v5"
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

const regionalTicketPrefix = "DeskRealm/relay/v1/pair1\x00"

// When enabled, reject relay tickets on the first node a fixture pair tries.
// Health and RTT probes still reach both native listeners.
type relayFixturePairGate struct {
	enabled  atomic.Bool
	rejected atomic.Pointer[string]
}

func relayFixturePublish(t *testing.T, gate *relayFixturePairGate) (string, *atomic.Pointer[string]) {
	t.Helper()
	listener := relayFixtureListener(t)
	published := listener.Addr().String()
	var backend atomic.Pointer[string]
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				_ = client.SetDeadline(time.Now().Add(5 * time.Second))
				var data io.Reader = client
				if gate != nil && gate.enabled.Load() {
					reader := bufio.NewReader(client)
					prefix, err := reader.Peek(len(regionalTicketPrefix))
					if err != nil {
						return
					}
					if string(prefix) == regionalTicketPrefix {
						gate.rejected.CompareAndSwap(nil, &published)
						if *gate.rejected.Load() == published {
							return
						}
					}
					data = reader
				}
				address := backend.Load()
				if address == nil {
					return
				}
				server, err := net.DialTimeout("tcp", *address, time.Second)
				if err != nil {
					return
				}
				defer server.Close()
				deadline := time.Now().Add(5 * time.Second)
				_ = client.SetDeadline(deadline)
				_ = server.SetDeadline(deadline)
				forwarded := make(chan struct{})
				go func() {
					defer close(forwarded)
					_, _ = io.Copy(server, data)
					_ = server.Close()
				}()
				_, _ = io.Copy(client, server)
				_ = client.Close()
				<-forwarded
			}()
		}
	}()
	return published, &backend
}

func TestRegionalRelayRuntimePostgres(t *testing.T) {
	binary, hbbr := os.Getenv("DESKREALM_RELAY_TEST_BINARY"), os.Getenv("DESKREALM_TEST_HBBR_RUNNER")
	if binary == "" || hbbr == "" {
		t.Skip("run scripts/deskrealm-m3-test relay-enrollment")
	}
	f := newSessionFixture(t)
	ctx, store, h, admin := f.ctx, f.store, f.h, f.admin
	csrf := csrfToken(admin.Value)
	server := httptest.NewUnstartedServer(nil)
	origin, err := ParseOrigin("https://"+server.Listener.Addr().String(), false)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := ConfigureManagedTransport(strings.TrimPrefix(f.rendezvous, rendezvousPrefix), "", "")
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = NewHandler(store, origin, nil, AuthConfig{ManagedKey: f.signing, Transport: transport})
	tlsDir := os.Getenv("DESKREALM_TEST_RELAY_TLS_DIR")
	if tlsDir != "" {
		cert, err := tls.LoadX509KeyPair(filepath.Join(tlsDir, "server.pem"), filepath.Join(tlsDir, "server.key"))
		if err != nil {
			t.Fatal(err)
		}
		server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	}
	server.StartTLS()
	center := server
	defer center.Close()
	scratch := t.TempDir()
	ca := filepath.Join(scratch, "center-ca.pem")
	trust := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: center.Certificate().Raw})
	if tlsDir != "" {
		trust, err = os.ReadFile(filepath.Join(tlsDir, "ca.pem"))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(ca, trust, 0600); err != nil {
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
	var gate relayFixturePairGate
	create := func(name, region string) node {
		t.Helper()
		endpoint, published := relayFixturePublish(t, &gate)
		record := request(h, "POST", "/v1/admin/relays", testOrigin,
			relayRequest{name, "site-" + name, region, endpoint, "1.1.16", 2}, admin, csrf)
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
		if os.Getenv("DESKREALM_TEST_RELAY_UPGRADE") == "1" {
			args = append(args, "--managed-upgrades")
		}
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
			if err := store.DB.QueryRow(ctx, `SELECT health_sequence,COALESCE(`+relayHealthySQL+`,false)
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
	a, b := create("alpha", "north"), create("beta", "south")
	if a.public == b.public {
		t.Fatal("regional relays share an identity")
	}
	checkStates("awaiting_health", "awaiting_health")
	stopA, stopB := start(a), start(b)
	waitHealth(a, 0)
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
	if runner := os.Getenv("DESKREALM_TEST_RUST_RELAY_RUNNER"); runner != "" {
		sampleRegionalRelaysFromRust(t, f, center.URL, ca, scratch, runner, nativeRelayScenario{})
		if hbbs := os.Getenv("DESKREALM_TEST_HBBS_RUNNER"); hbbs != "" {
			address := nativeRegionalRendezvous(t, f, center.URL, ca, scratch, hbbs)
			scenario := nativeRelayScenario{rendezvous: address, candidates: 2}
			for _, failover := range []bool{false, true} {
				gate.enabled.Store(failover)
				sampleRegionalRelaysFromRust(t, f, center.URL, ca, scratch, runner, scenario)
				f.count(t, 1, `SELECT count(*) FROM managed_session_redemptions d JOIN session_relay_rounds r
				ON r.session_id=d.session_id AND r.connection_id=d.connection_id JOIN session_relay_selections s ON s.round_id=r.id
				WHERE d.path='relay' AND d.relay_id=s.relay_id::text AND s.failed_at IS NULL AND d.lease_sequence=2
				AND d.session_id=(SELECT id FROM managed_sessions ORDER BY issued_at DESC LIMIT 1)`)
				if failover {
					rejected := gate.rejected.Load()
					if rejected == nil {
						t.Fatal("no selected regional connection was rejected by the fixture")
					}
					f.count(t, 1, `SELECT count(*) FROM session_relay_selections s JOIN session_relay_rounds r ON r.id=s.round_id
						JOIN session_relay_candidates c ON c.round_id=s.round_id AND c.relay_id=s.relay_id
						WHERE s.attempt=1 AND s.failed_at IS NOT NULL AND c.endpoint=$1
						AND r.session_id=(SELECT id FROM managed_sessions ORDER BY issued_at DESC LIMIT 1)`, *rejected)
				}
			}
			gate.enabled.Store(false)
			sweep()
			baseline := waitNativeRegionalSession(t, f, "")
			var interrupted nativeRegionalSession
			var stopped node
			scenario.disconnect = func() {
				interrupted = waitNativeRegionalSession(t, f, baseline.session)
				switch interrupted.relay {
				case a.NodeID:
					stopped = a
					stopA()
				case b.NodeID:
					stopped = b
					stopB()
				default:
					t.Fatal("unexpected paired relay", interrupted.relay)
				}
			}
			sampleRegionalRelaysFromRust(t, f, center.URL, ca, scratch, runner, scenario)
			f.count(t, 1, `SELECT count(*) FROM managed_session_stops WHERE session_id=$1 AND reason='connection_lost' AND lease_sequence=2`, interrupted.session)
			controller, _ := f.roundPeers()
			oldGrant := f.readGrant(t, interrupted.session)
			checkCode(t, controller.join(t, h, oldGrant, testSessionUUID(593)), 403, errSessionDenied.Error())
			for range 3 {
				sweep()
			}
			f.count(t, 1, `SELECT count(*) FROM relay_nodes WHERE id=$1 AND probe_failures=3`, stopped.NodeID)
			scenario.disconnect, scenario.candidates = nil, 1
			sampleRegionalRelaysFromRust(t, f, center.URL, ca, scratch, runner, scenario)
			reconnected := waitNativeRegionalSession(t, f, interrupted.session)
			if reconnected.session == interrupted.session || reconnected.grant == interrupted.grant ||
				reconnected.connection == interrupted.connection || reconnected.round == interrupted.round || reconnected.relay == interrupted.relay {
				t.Fatalf("reconnect did not use a fresh grant, round and healthy relay: %+v then %+v", interrupted, reconnected)
			}
			for _, expected := range []struct {
				session nativeRegionalSession
				state   string
			}{{interrupted, "stopped"}, {reconnected, "active"}} {
				detail := request(h, "GET", "/v1/admin/sessions/"+expected.session.session, "", nil, admin, "")
				checkCode(t, detail, 200, expected.session.relay)
				view := responseValue[sessionAdminRow](t, detail.Body.Bytes())
				if view.State != expected.state || view.Connection != expected.session.connection || view.Path != "relay" ||
					view.RelayHistory == nil || len(view.RelayHistory.Rounds) != 1 {
					t.Fatalf("unexpected regional session detail: %+v", view)
				}
				candidates := view.RelayHistory.Rounds[0].Candidates
				if len(candidates) == 0 || !candidates[0].Redeemed || candidates[0].Relay != expected.session.relay ||
					candidates[0].Controller.Median == nil || candidates[0].Target.Median == nil {
					t.Fatalf("session detail lacks actual relay and both measured medians: %+v", candidates)
				}
			}
			var sequence int64
			if err := store.DB.QueryRow(ctx, "SELECT health_sequence FROM relay_nodes WHERE id=$1", stopped.NodeID).Scan(&sequence); err != nil {
				t.Fatal(err)
			}
			if stopped.NodeID == a.NodeID {
				stopA = start(a)
			} else {
				start(b)
			}
			waitHealth(stopped, sequence)
			sweep()
			checkStates("available", "available")
		}
	}
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
	var stoppedSequence int64
	if err := store.DB.QueryRow(ctx, `UPDATE relay_nodes SET health_at=statement_timestamp()-interval '16 seconds'
		WHERE id=$1 RETURNING health_sequence`, a.NodeID).Scan(&stoppedSequence); err != nil {
		t.Fatal(err)
	}
	checkStates("awaiting_health", "available")
	stopA = start(a)
	sequence := waitHealth(a, stoppedSequence)
	sweep()
	checkStates("available", "available")
	newEndpoint, newPublished := relayFixturePublish(t, nil)
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
	err = store.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE action='relay.registered'),
		count(*) FILTER (WHERE action='relay.available') FROM audit_events`).Scan(&registrations, &admissions)
	if err != nil || registrations != 2 || admissions != 2 {
		t.Fatalf("expected one registration and admission audit per node: %d %d %v", registrations, admissions, err)
	}
	if os.Getenv("DESKREALM_TEST_RELAY_UPGRADE") == "1" {
		status := func() (string, string) {
			value := responseValue[struct {
				Active string `json:"active_sha256"`
				State  string `json:"state"`
			}](t, command("upgrade-status", "--state", a.state))
			return value.State, value.Active
		}
		_, original := status()
		replace := func(name string, script []byte, want string) string {
			t.Helper()
			candidate := filepath.Join(scratch, name)
			if err := os.WriteFile(candidate, script, 0700); err != nil {
				t.Fatal(err)
			}
			approved := fmt.Sprintf("%x", sha256.Sum256(script))
			var revision int64
			if err := store.DB.QueryRow(ctx, "SELECT revision FROM relay_nodes WHERE id=$1", a.NodeID).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			url := relayAdminURL + "/" + a.NodeID
			reply := request(h, "POST", url+"/upgrade", testOrigin,
				map[string]any{"action": "start", "revision": revision, "sha256": approved}, admin, csrf)
			checkCode(t, reply, 200, "pending")
			task := responseValue[relayprotocol.Upgrade](t, reply.Body.Bytes()).TaskID
			command("upgrade", "--state", a.state, "--ca", ca, "--task", task, "--hbbr", candidate, "--sha256", approved)
			checkCode(t, request(h, "POST", url+"/lifecycle", testOrigin, relayChange{"resume", revision + 1, ""}, admin, csrf), 409, "")
			until := time.Now().Add(55 * time.Second)
			var phase, active string
			for time.Now().Before(until) {
				phase, active = status()
				if phase == want {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			if phase != want || want == "succeeded" && active != approved || want == "rolled_back" && active != original {
				t.Fatalf("upgrade %s expected %s, got %s and active digest %s", name, want, phase, active)
			}
			checkCode(t, request(h, "GET", tasksPath+"/"+task, "", nil, admin, ""), 200, "relay_upgrade_"+want)
			// The fixture drives the center's probes explicitly. Refresh beta after
			// the upgrade grace period so its last probe has not expired.
			sweep()
			checkStates("draining", "available")
			checkCode(t, request(h, "POST", url+"/lifecycle", testOrigin, relayChange{"resume", revision + 2, ""}, admin, csrf),
				200, "relay_resumed")
			checkStates("available", "available")
			return active
		}
		replace("broken", []byte("#!/bin/sh\nexit 3\n"), "rolled_back")
		base, err := os.ReadFile(hbbr)
		if err != nil {
			t.Fatal(err)
		}
		active := replace("compatible", append(base, []byte("\n# compatible upgrade fixture\n")...), "succeeded")
		if err := store.DB.QueryRow(ctx, "SELECT health_sequence FROM relay_nodes WHERE id=$1", a.NodeID).Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		stopA()
		start(a)
		waitHealth(a, sequence)
		if phase, digest := status(); phase != "succeeded" || digest != active {
			t.Fatal("runtime restart lost the active upgrade")
		}
		out := command("status", "--state", a.state)
		if !strings.Contains(string(out), a.public) || !strings.Contains(string(out), newEndpoint) {
			t.Fatal("upgrade changed the enrolled node identity or endpoint")
		}
		t.Log("Verified compatible digest switch, rollback of a failed process, retained identity, and unaffected second native relay")
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
	controllerResponse := controller.get(f.h, grant, conn)
	checkCode(t, controllerResponse, 200, round.ID)
	controllerRound := responseValue[relayRound](t, controllerResponse.Body.Bytes())
	left := roundCandidate(t, controllerRound, alpha)
	right := roundCandidate(t, round, alpha)
	other := roundCandidate(t, round, beta)
	if left.ProbePermit == nil || right.ProbePermit == nil || other.ProbePermit == nil ||
		left.ProbePermit.Frame == right.ProbePermit.Frame {
		t.Fatal("center did not issue distinct permits for both roles and nodes")
	}
	ctx, stop := context.WithDeadline(f.ctx, *round.Deadline)
	defer stop()
	probe := func(ctx context.Context, destination relayRoundCandidate, permit relayprotocol.SessionProbePermit, p roundPeer, slot byte) (time.Duration, error) {
		return relayprotocol.SessionProbeRTT(ctx, destination.Endpoint, relayprotocol.PublicKey(destination.PublicKey),
			destination.Build, permit, p.key, slot)
	}
	for _, attempt := range []struct {
		label       string
		destination relayRoundCandidate
		permit      relayprotocol.SessionProbePermit
		peer        roundPeer
	}{
		{"wrong node", other, *left.ProbePermit, controller},
		{"wrong role key", left, *left.ProbePermit, target},
		{"wrong center signature", left, relayprotocol.SessionProbePermit{Frame: left.ProbePermit.Frame, Signature: right.ProbePermit.Signature}, controller},
		{"wrong published endpoint", left, alteredProbePermit(t, f, *left.ProbePermit, 7), controller},
		{"wrong configuration revision", left, alteredProbePermit(t, f, *left.ProbePermit, 6), controller},
	} {
		if _, err := probe(ctx, attempt.destination, attempt.permit, attempt.peer, 0); !errors.Is(err, relayprotocol.ErrProbeProtocol) {
			t.Fatalf("native hbbr accepted or failed to reject %s: %v", attempt.label, err)
		}
	}
	type measured struct {
		peer      roundPeer
		candidate relayRoundCandidate
		samples   []relayProbeSample
		err       error
	}
	results := make(chan measured, 4)
	for _, input := range []struct {
		peer  roundPeer
		round relayRound
	}{{controller, controllerRound}, {target, round}} {
		for _, candidate := range input.round.Candidates {
			if candidate.ProbePermit == nil {
				t.Fatal("eligible candidate lacks a probe permit")
			}
			go func() {
				result := measured{peer: input.peer, candidate: candidate, samples: make([]relayProbeSample, 0, 3)}
				for slot := range byte(3) {
					rtt, err := probe(ctx, candidate, *candidate.ProbePermit, input.peer, slot)
					if err != nil {
						result.err = err
						break
					}
					result.samples = append(result.samples, measuredRTT(uint32(rtt.Microseconds())))
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
	if _, err := probe(ctx, left, *left.ProbePermit, controller, 0); !errors.Is(err, relayprotocol.ErrProbeProtocol) {
		t.Fatalf("native hbbr accepted a spent sample: %v", err)
	}
	<-ctx.Done()
	// A fresh signature over a past window tests expiration independently of replay
	// and of clock differences between the fixture host and the relay container.
	expiredRound := round
	opened, deadline := round.Opened.Add(-time.Minute), round.Deadline.Add(-time.Minute)
	expiredRound.Opened, expiredRound.Deadline = &opened, &deadline
	centerAPI := API{managedKey: f.signing}
	unusedExpired, err := centerAPI.relayProbePermit(grant, expiredRound, controller.role, controller.key.Public().(ed25519.PublicKey), left)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe(f.ctx, left, *unusedExpired, controller, 0); !errors.Is(err, relayprotocol.ErrProbeProtocol) {
		t.Fatalf("native hbbr accepted an expired permit: %v", err)
	}
	var closed relayRound
	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		response = controller.get(f.h, grant, conn)
		checkCode(t, response, 200, round.ID)
		closed = responseValue[relayRound](t, response.Body.Bytes())
		if closed.State == "complete" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if closed.State != "complete" {
		t.Fatal("center probe window did not close")
	}
	for _, candidate := range closed.Candidates {
		if candidate.ProbePermit != nil {
			t.Fatal("center issued a permit after the round closed")
		}
	}
	// Confirm a selection using the measurements just taken from both native relays.
	selectionPath := sessionsURL + "/" + grant.ID + "/relay-selection?connection_id=" + conn
	response = nativeRequest(f.h, "GET", selectionPath, controller.bearer, nil)
	checkCode(t, response, 200, "dual_rtt")
	options := responseValue[relaySelectionResult](t, response.Body.Bytes())
	if len(options.Options) != 2 || options.Options[0].CombinedRTT == nil || options.Options[1].CombinedRTT == nil ||
		*options.Options[0].CombinedRTT > *options.Options[1].CombinedRTT {
		t.Fatalf("unexpected ordering of actual relay samples: %+v", options)
	}
	response = controller.selectRelay(t, f.h, grant, round, options.Options[0].Relay, "select")
	checkCode(t, response, 200, "confirmed")
	leftTicket := responseValue[relaySelectionResult](t, response.Body.Bytes()).Ticket
	response = nativeRequest(f.h, "GET", selectionPath, target.bearer, nil)
	checkCode(t, response, 200, options.Options[0].Relay)
	confirmed := responseValue[relaySelectionResult](t, response.Body.Bytes())
	if selected := confirmed.Selected; selected == nil ||
		selected.Relay != options.Options[0].Relay || selected.CombinedRTT == nil {
		t.Fatalf("target did not receive the confirmed selection: %+v", selected)
	}
	testNativeRelayTickets(t, options.Options, leftTicket, confirmed.Ticket, controller.key, target.key)
}

func testNativeRelayTickets(t *testing.T, ranked []relaySelectionOption, left, right *relaySessionTicket,
	controller, target ed25519.PrivateKey) {
	t.Helper()
	if left == nil || right == nil {
		t.Fatal("expected both role tickets for native hbbr")
	}
	type attempt struct {
		conn  net.Conn
		frame []byte
	}
	sendTicket := func(endpoint string, ticket *relaySessionTicket) attempt {
		t.Helper()
		conn, err := net.DialTimeout("tcp", endpoint, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		frame, err := base64.RawURLEncoding.DecodeString(ticket.Frame)
		signature, valid := canonicalSignature(ticket.Signature)
		if err != nil || !valid {
			t.Fatal("invalid ticket fixture")
		}
		if _, err := conn.Write(bytes.Join([][]byte{[]byte(regionalTicketPrefix), frame, signature}, nil)); err != nil {
			t.Fatal(err)
		}
		return attempt{conn, frame}
	}
	prove := func(attempt attempt, key ed25519.PrivateKey) {
		t.Helper()
		var nonce [32]byte
		if _, err := io.ReadFull(attempt.conn, nonce[:]); err != nil {
			t.Fatal(err)
		}
		frameHash := sha256.Sum256(attempt.frame)
		proof, err := managedproto.Encode(managedproto.RelayTicketProof, frameHash[:], nonce[:])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := attempt.conn.Write(ed25519.Sign(key, proof)); err != nil {
			t.Fatal(err)
		}
	}
	rejected := func(conn net.Conn) {
		t.Helper()
		var output [1]byte
		n, err := conn.Read(output[:])
		var timeout net.Error
		if n != 0 || err == nil || (errors.As(err, &timeout) && timeout.Timeout()) {
			t.Fatalf("native relay did not close a rejected ticket: %d bytes, %v", n, err)
		}
		_ = conn.Close()
	}
	rejected(sendTicket(ranked[1].Endpoint, left).conn)
	wrongHolder := sendTicket(ranked[0].Endpoint, left)
	prove(wrongHolder, target)
	rejected(wrongHolder.conn)
	first, second := sendTicket(ranked[0].Endpoint, left), sendTicket(ranked[0].Endpoint, right)
	prove(first, controller)
	prove(second, target)
	var leftReceipt, rightReceipt [ed25519.SignatureSize]byte
	if _, err := io.ReadFull(first.conn, leftReceipt[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(second.conn, rightReceipt[:]); err != nil || rightReceipt != leftReceipt {
		t.Fatalf("roles did not receive the same native pairing receipt: %v", err)
	}
	active, err := relayprotocol.Probe(context.Background(), ranked[0].Endpoint, relayprotocol.PublicKey(left.PublicKey), "1.1.16")
	if err != nil || active != 1 {
		t.Fatalf("native relay did not report its paired session: %d %v", active, err)
	}
	replay := sendTicket(ranked[0].Endpoint, left)
	prove(replay, controller)
	rejected(replay.conn)
	_ = first.conn.Close()
	_ = second.conn.Close()
}

func alteredProbePermit(t *testing.T, f sessionFixture, permit relayprotocol.SessionProbePermit, field int) relayprotocol.SessionProbePermit {
	t.Helper()
	frame, err := base64.RawURLEncoding.DecodeString(permit.Frame)
	if err != nil {
		t.Fatal(err)
	}
	offset := 8
	for i := 0; i < field; i++ {
		offset += 2 + int(binary.BigEndian.Uint16(frame[offset:offset+2]))
	}
	last := offset + 1 + int(binary.BigEndian.Uint16(frame[offset:offset+2]))
	frame[last] ^= 2
	return relayprotocol.SessionProbePermit{
		Frame: base64.RawURLEncoding.EncodeToString(frame), Signature: signRelay(f.signing.key, frame),
	}
}

func nativeRegionalRendezvous(t *testing.T, f sessionFixture, center, ca, scratch, runner string) string {
	t.Helper()
	listener := relayFixtureListener(t)
	address := listener.Addr().String()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()
	log, err := os.Create(filepath.Join(scratch, "hbbs.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	ctx, cancel := context.WithCancel(f.ctx)
	cmd := exec.CommandContext(ctx, runner, port, ca)
	cmd.Env = append(os.Environ(), "DESKREALM_MANAGED_CENTER="+center,
		"DESKREALM_MANAGED_CENTER_KID="+f.signing.id,
		"DESKREALM_MANAGED_CENTER_PUBLIC_KEY="+base64.RawURLEncoding.EncodeToString(f.signing.key.Public().(ed25519.PublicKey)),
		"DESKREALM_MANAGED_RENDEZVOUS_TOKEN="+strings.TrimPrefix(f.rendezvous, rendezvousPrefix))
	cmd.Stdout, cmd.Stderr = log, log
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	for wait := time.Now().Add(8 * time.Second); time.Now().Before(wait); {
		select {
		case err := <-done:
			output, _ := os.ReadFile(log.Name())
			t.Fatalf("regional hbbs startup: %v\n%s", err, output)
		default:
		}
		if conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
			_ = conn.Close()
			return address
		}
		time.Sleep(50 * time.Millisecond)
	}
	output, _ := os.ReadFile(log.Name())
	t.Fatalf("regional hbbs did not begin listening\n%s", output)
	return ""
}

type nativeRegionalSession struct{ session, grant, connection, round, relay string }

func waitNativeRegionalSession(t *testing.T, f sessionFixture, after string) nativeRegionalSession {
	t.Helper()
	var out nativeRegionalSession
	for until := time.Now().Add(10 * time.Second); time.Now().Before(until); {
		err := f.store.DB.QueryRow(f.ctx, `SELECT g.id,g.grant_id,d.connection_id,r.id,d.relay_id
			FROM managed_sessions g JOIN managed_session_redemptions d ON d.session_id=g.id
			JOIN session_relay_rounds r ON r.session_id=g.id AND r.connection_id=d.connection_id
			WHERE d.lease_sequence=2 AND ($1='' OR g.issued_at>(SELECT issued_at FROM managed_sessions WHERE id=NULLIF($1,'')::uuid))
			ORDER BY g.issued_at DESC LIMIT 1`, after).Scan(&out.session, &out.grant, &out.connection, &out.round, &out.relay)
		if err == nil {
			return out
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatal("no renewed regional session")
	return out
}

type nativeRelayScenario struct {
	rendezvous string
	candidates int
	disconnect func()
}

func sampleRegionalRelaysFromRust(t *testing.T, f sessionFixture, center, ca, scratch, runner string, scenario nativeRelayScenario) {
	t.Helper()
	testName := "managed_native_endpoints_report_both_regional_relays"
	if scenario.rendezvous != "" {
		testName = "managed_native_endpoints_forward_on_selected_regional_relay"
	}
	if scenario.candidates == 0 {
		scenario.candidates = 2
	}
	encode := base64.RawURLEncoding.EncodeToString
	peer := func(id, credential string, key ed25519.PrivateKey) map[string]string {
		return map[string]string{"device": id, "credential": credential, "seed": encode(key.Seed())}
	}
	var policy int64
	if err := f.store.DB.QueryRow(f.ctx, "SELECT policy_version FROM managed_sessions ORDER BY issued_at DESC LIMIT 1").Scan(&policy); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{
		"center": center, "kid": f.signing.id, "center_key": encode(f.signing.key.Public().(ed25519.PublicKey)),
		"controller": peer(f.controller, f.userToken, f.controllerKey),
		"target":     peer(f.target, f.targetToken, f.targetKey),
		"rendezvous": scenario.rendezvous, "policy_version": policy, "expect_disconnect": scenario.disconnect != nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(scratch, testName+"-*.json")
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	if _, err := file.Write(input); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	command := exec.CommandContext(ctx, runner, path, ca, testName)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	if scenario.disconnect != nil {
		scenario.disconnect()
	}
	// Check the native harness actually ran its named test.
	if err := <-done; err != nil || !strings.Contains(output.String(), testName+" ... ok") {
		t.Fatalf("native relay endpoint %s failed: %v\n%s", testName, err, &output)
	}
	f.count(t, 6*scenario.candidates, `SELECT count(*) FROM session_relay_reports reports
		JOIN session_relay_rounds r ON r.id=reports.round_id
		CROSS JOIN LATERAL jsonb_array_elements(reports.samples) s
		WHERE r.session_id=(SELECT id FROM managed_sessions ORDER BY issued_at DESC LIMIT 1)
			AND s->>'result'='ok' AND (s->>'rtt_us')::integer BETWEEN 0 AND 2000000`)
}
