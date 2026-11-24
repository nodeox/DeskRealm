package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"deskrealm/internal/windowsdelivery"
)

const (
	profileName     = "deskrealm-public.json"
	deploymentName  = "deployment.json"
	seedName        = "device.seed"
	rustdeskIDName  = "rustdesk-id"
	receiptName     = "registration.json"
	maxDocumentSize = 64 << 10
)

var (
	rustdeskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{2,31}$`)
	uuidPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type registration struct {
	Device       string `json:"device_id"`
	Organization string `json:"organization_id"`
	State        string `json:"registration_state"`
}

func (r registration) valid() bool {
	return uuidPattern.MatchString(r.Device) && uuidPattern.MatchString(r.Organization) &&
		(r.State == "pending" || r.State == "approved")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "DeskRealm:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if runtime.GOOS != "windows" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return errors.New("the launcher requires Windows x64 or ARM64")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	install := filepath.Dir(executable)
	profileFile, err := os.OpenRoot(install)
	if err != nil {
		return err
	}
	defer profileFile.Close()
	raw, err := readDocument(profileFile, profileName)
	if err != nil {
		return err
	}
	profile, err := windowsdelivery.ParsePublicProfile(raw)
	if err != nil {
		return err
	}
	local := os.Getenv("LOCALAPPDATA")
	if !filepath.IsAbs(local) {
		return errors.New("LOCALAPPDATA is unavailable")
	}
	bucket := sha256.Sum256([]byte(profile.Center.Origin + "\n" + profile.Center.SigningKey))
	stateDir := filepath.Join(local, "DeskRealm", "deployments", hex.EncodeToString(bucket[:]))
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	state, err := os.OpenRoot(stateDir)
	if err != nil {
		return err
	}
	defer state.Close()
	if len(args) == 2 && args[0] == "prepare" {
		key, err := prepare(state, args[1])
		if err != nil {
			return err
		}
		arch := "x64"
		if runtime.GOARCH == "arm64" {
			arch = "arm64"
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"arch": arch, "rustdesk_id": args[1],
			"public_key":      base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
			"deployment_file": filepath.Join(stateDir, deploymentName),
		})
	}
	if len(args) != 0 && (len(args) != 1 || args[0] != "start") {
		return errors.New("usage: DeskRealm.exe [prepare RUSTDESK_ID | start]")
	}
	id, key, err := loadDevice(state)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	registered, err := bootstrap(context.Background(), client, profile.Center, state, "windows-"+runtime.GOARCH, id, key)
	if err != nil {
		return err
	}
	desktop := exec.Command(filepath.Join(install, "rustdesk.exe"))
	desktop.Dir = install
	desktop.Env = desktopEnvironment(desktop.Environ(), profile.Center, registered, id, filepath.Join(stateDir, seedName))
	if bundle := installedBundle(local, install); bundle != "" {
		desktop.Env = append(desktop.Env, "DESKREALM_MANAGED_BUNDLE_SHA256="+bundle)
	}
	return desktop.Run()
}

func installedBundle(local, install string) string {
	want := filepath.Join(local, "Programs", "DeskRealm", "releases")
	name := filepath.Base(install)
	if strings.EqualFold(filepath.Dir(install), want) && windowsdelivery.ValidDigest(name) {
		return name
	}
	return ""
}

func prepare(state *os.Root, id string) (ed25519.PrivateKey, error) {
	if !rustdeskIDPattern.MatchString(id) {
		return nil, errors.New("invalid RustDesk ID")
	}
	if err := saveDocument(state, rustdeskIDName, []byte(id)); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	stored, err := readDocument(state, rustdeskIDName)
	if err != nil {
		return nil, err
	}
	if string(stored) != id {
		return nil, errors.New("this installation already has a different RustDesk ID")
	}
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	if err := saveDocument(state, seedName, []byte(base64.RawURLEncoding.EncodeToString(key.Seed()))); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	_, key, err = loadDevice(state)
	return key, err
}

func loadDevice(state *os.Root) (string, ed25519.PrivateKey, error) {
	id, err := readDocument(state, rustdeskIDName)
	if err != nil || !rustdeskIDPattern.Match(id) {
		return "", nil, errors.New("prepare the local device identity first")
	}
	raw, err := readDocument(state, seedName)
	if err != nil {
		return "", nil, errors.New("local device key is unavailable")
	}
	seed, err := base64.RawURLEncoding.Strict().DecodeString(string(raw))
	if err != nil || len(seed) != ed25519.SeedSize || base64.RawURLEncoding.EncodeToString(seed) != string(raw) {
		return "", nil, errors.New("invalid local device key")
	}
	return string(id), ed25519.NewKeyFromSeed(seed), nil
}

func readDocument(root *os.Root, name string) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxDocumentSize {
		return nil, fmt.Errorf("invalid document: %s", name)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxDocumentSize+1))
	if err != nil || len(raw) == 0 || len(raw) > maxDocumentSize {
		return nil, fmt.Errorf("unable to read document: %s", name)
	}
	return raw, nil
}

// Publish a complete file without replacing another process's saved state.
func saveDocument(root *os.Root, name string, raw []byte) error {
	file, err := os.CreateTemp(root.Name(), ".deskrealm-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return root.Link(filepath.Base(file.Name()), name)
}

func desktopEnvironment(in []string, center windowsdelivery.Center, r registration, id, seed string) []string {
	values := map[string]string{
		"DESKREALM_MANAGED_MODE": "1", "DESKREALM_MANAGED_CENTER": center.Origin,
		"DESKREALM_MANAGED_CENTER_KID": center.SigningKeyID, "DESKREALM_MANAGED_CENTER_PUBLIC_KEY": center.SigningKey,
		"DESKREALM_MANAGED_RENDEZVOUS_ADDRESS": center.IDServer, "DESKREALM_MANAGED_RUSTDESK_ID": id,
		"DESKREALM_MANAGED_DEVICE_ID": r.Device, "DESKREALM_MANAGED_ORGANIZATION_ID": r.Organization,
		"DESKREALM_MANAGED_DEVICE_SEED_FILE": seed, "DESKREALM_MANAGED_CA_FILE": "",
		"DESKREALM_MANAGED_RELAY_ID": "", "DESKREALM_MANAGED_RELAY_ADDRESS": "",
		"DESKREALM_MANAGED_BUNDLE_SHA256": "",
	}
	if relay := center.DefaultRelay; relay != nil {
		values["DESKREALM_MANAGED_RELAY_ID"] = relay.ID
		values["DESKREALM_MANAGED_RELAY_ADDRESS"] = relay.Address
	}
	out := make([]string, 0, len(in)+len(values))
	for _, entry := range in {
		name, _, _ := strings.Cut(entry, "=")
		if _, managed := values[strings.ToUpper(name)]; !managed {
			out = append(out, entry)
		}
	}
	for name, value := range values {
		if value != "" {
			out = append(out, name+"="+value)
		}
	}
	return out
}
