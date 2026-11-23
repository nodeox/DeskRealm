package install

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

type centerStatus struct {
	publicDeployment
	Phase       string                     `json:"phase"`
	Detail      string                     `json:"detail,omitempty"`
	Initialized *bool                      `json:"initialized,omitempty"`
	Services    map[string]containerStatus `json:"services"`
}

type containerStatus struct {
	State    string `json:"state"`
	Health   string `json:"health,omitempty"`
	ExitCode int    `json:"exit_code"`
}

type composeContainer struct {
	Service  string
	Project  string
	Image    string
	State    string
	Health   string
	ExitCode int
}

func runningServices(ctx context.Context, c config, center publicDeployment, run dockerRunner) (map[string]containerStatus, error) {
	data, err := composeCommand(ctx, run, center, "ps", "--all", "--orphans=false", "--format", "json")
	if err != nil {
		return nil, errors.New("cannot inspect Docker Compose service state")
	}
	var containers []composeContainer
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var record json.RawMessage
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, errors.New("unexpected Docker Compose service state")
		}
		if record[0] == '[' {
			if err := json.Unmarshal(record, &containers); err != nil {
				return nil, errors.New("unexpected Docker Compose service list")
			}
			break
		}
		var row composeContainer
		if err := json.Unmarshal(record, &row); err != nil {
			return nil, errors.New("unexpected Docker Compose service record")
		}
		containers = append(containers, row)
	}
	images := center.runtimeImages(c.Images)
	expected := map[string]string{
		"database": images.PostgreSQL, "migrate": images.Center, "center": images.Center, "rendezvous": images.Rendezvous,
	}
	result := make(map[string]containerStatus, len(containers))
	for _, row := range containers {
		image, exists := expected[row.Service]
		if !exists || row.Project != center.Project {
			return nil, errors.New("Docker Compose returned a container outside the deployment")
		}
		if _, duplicate := result[row.Service]; duplicate || row.Image != image {
			return nil, fmt.Errorf("%s: existing container does not match the configured Compose service and image", row.Service)
		}
		status := containerStatus{State: row.State, Health: row.Health, ExitCode: row.ExitCode}
		switch status.State {
		case "created", "restarting", "running", "removing", "paused", "exited", "dead":
		default:
			status.State = "unknown"
		}
		switch status.Health {
		case "", "starting", "healthy", "unhealthy":
		default:
			status.Health = "unknown"
		}
		result[row.Service] = status
	}
	return result, nil
}

func serviceReady(name string, status containerStatus) bool {
	switch name {
	case "database":
		return status.State == "running" && status.Health == "healthy"
	case "migrate":
		return status.State == "exited" && status.ExitCode == 0
	default:
		return status.State == "running" && (status.Health == "" || status.Health == "healthy")
	}
}

func serviceHealth(services map[string]containerStatus) (step, detail string) {
	for _, name := range []string{"database", "migrate", "center", "rendezvous"} {
		status, found := services[name]
		if !found {
			return name, "container has not been created"
		}
		if !serviceReady(name, status) {
			return name, fmt.Sprintf("container state %s, health %q, exit code %d", status.State, status.Health, status.ExitCode)
		}
	}
	return "", ""
}

func inspectCenter(ctx context.Context, c config, center publicDeployment, run dockerRunner) (centerStatus, error) {
	result := centerStatus{publicDeployment: center}
	recordedCluster, err := readDatabaseRecord(c.Directory, center.ID)
	if err != nil {
		return result, err
	}
	result.Services, err = runningServices(ctx, c, center, run)
	if err != nil {
		return result, err
	}
	if len(result.Services) == 0 {
		result.Phase = "prepared"
		if recordedCluster != "" {
			result.Phase = "stopped"
		}
		result.Detail = "no deployment containers are running or stopped in Compose"
		return result, nil
	}
	if result.Phase, result.Detail = serviceHealth(result.Services); result.Phase != "" {
		return result, nil
	}
	initialized, err := probeHTTPS(ctx, c, center)
	if err != nil {
		result.Phase, result.Detail = "https", err.Error()
		return result, nil
	}
	result.Initialized = &initialized
	if err := probeRendezvous(ctx, c); err != nil {
		result.Phase, result.Detail = "rendezvous", err.Error()
		return result, nil
	}
	if c.DefaultRelay.ID != "" {
		if err := checkDefaultRelay(ctx, c, center, run); err != nil {
			result.Phase, result.Detail = "default-relay", err.Error()
			return result, nil
		}
	}
	result.Phase = "ready"
	return result, nil
}

func waitForCenter(ctx context.Context, c config, center publicDeployment, run dockerRunner) (centerStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		status, err := inspectCenter(ctx, c, center, run)
		if err != nil {
			return status, err
		}
		if status.Phase == "ready" {
			return status, nil
		}
		timer := time.NewTimer(600 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return status, fmt.Errorf("%s: %s", status.Phase, status.Detail)
		case <-timer.C:
		}
	}
}

func probeHost(c config) string {
	if c.Ports.BindHost != "" {
		return c.Ports.BindHost
	}
	return "127.0.0.1"
}

func probeHTTPS(ctx context.Context, c config, center publicDeployment) (bool, error) {
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	if c.HTTPS.CACertificate != "" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			return false, errors.New("cannot load system HTTPS trust roots")
		}
		pem, err := os.ReadFile(c.HTTPS.CACertificate)
		if err != nil || !roots.AppendCertsFromPEM(pem) {
			return false, errors.New("cannot load the configured HTTPS CA")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	if c.HTTPS.Mode == httpsProvided {
		dialer := &net.Dialer{}
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, joinPort(probeHost(c), c.Ports.API))
		}
	}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	var bootstrap struct {
		Initialized *bool `json:"initialized"`
	}
	if err := getHealthJSON(ctx, client, c.PublicOrigin+"/v1/bootstrap/status", &bootstrap); err != nil {
		return false, fmt.Errorf("bootstrap HTTPS check: %w", err)
	}
	if bootstrap.Initialized == nil {
		return false, errors.New("bootstrap HTTPS response is missing initialized state")
	}
	var key struct {
		ID     string `json:"kid"`
		Public string `json:"public_key"`
	}
	if err := getHealthJSON(ctx, client, c.PublicOrigin+"/v1/managed/signing-key", &key); err != nil {
		return false, fmt.Errorf("signing key HTTPS check: %w", err)
	}
	if key.ID != center.SigningKeyID || key.Public != center.SigningKey {
		return false, errors.New("HTTPS endpoint returned a different deployment signing identity")
	}
	return *bootstrap.Initialized, nil
}

func getHealthJSON(ctx context.Context, client *http.Client, address string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return errors.New("invalid HTTPS health URL")
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("received HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(data) > 4096 || json.Unmarshal(data, result) != nil {
		return errors.New("invalid HTTPS health response")
	}
	return nil
}

func probeRendezvous(ctx context.Context, c config) error {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	check, err := dialer.DialContext(ctx, "tcp", joinPort(probeHost(c), c.Ports.RendezvousCheck))
	if err != nil {
		return fmt.Errorf("rendezvous check port: %w", err)
	}
	check.Close()
	connection, err := dialer.DialContext(ctx, "tcp", joinPort(probeHost(c), c.Ports.Rendezvous))
	if err != nil {
		return fmt.Errorf("rendezvous signaling port: %w", err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	// RustDesk frames encode the payload length in the upper bits of a
	// little-endian header. RendezvousMessage field 20 is TestNatRequest.
	if _, err := connection.Write([]byte{12, 0xa2, 0x01, 0}); err != nil {
		return fmt.Errorf("write rendezvous NAT probe: %w", err)
	}
	var header [4]byte
	if _, err := io.ReadFull(connection, header[:1]); err != nil {
		return fmt.Errorf("read rendezvous NAT probe: %w", err)
	}
	if _, err := io.ReadFull(connection, header[1:1+int(header[0]&3)]); err != nil {
		return fmt.Errorf("read rendezvous NAT frame: %w", err)
	}
	size := binary.LittleEndian.Uint32(header[:]) >> 2
	if size < 5 || size > 128 {
		return errors.New("rendezvous sent an invalid NAT frame size")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(connection, payload); err != nil {
		return fmt.Errorf("read rendezvous NAT response: %w", err)
	}
	// RendezvousMessage field 21 is TestNatResponse, with the observed port in field 1.
	if payload[0] != 0xaa || payload[1] != 0x01 || int(payload[2]) != len(payload)-3 || payload[3] != 0x08 {
		return errors.New("rendezvous did not return a TestNatResponse")
	}
	if port, n := binary.Uvarint(payload[4:]); n != len(payload)-4 || port == 0 || port > 65535 {
		return errors.New("rendezvous returned an invalid NAT port")
	}
	return nil
}
