// Command deskrealm-deploy uses a delegated API token to prepare managed Windows deployments.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"deskrealm/internal/windowsdelivery"
)

const maxJSON = 64 << 10

var (
	uuid       = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	rustDeskID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{2,31}$`)
)

type options struct {
	center, tokenFile, release, group, deployment, arch, target, output, purpose string
	lifetime                                                                     int
}

type deploymentClient struct {
	http        *http.Client
	base, token string
}

type deviceTarget struct {
	Arch       string `json:"arch"`
	RustDeskID string `json:"rustdesk_id"`
	PublicKey  string `json:"public_key"`
}

type enrollmentReceipt struct {
	Deployment string    `json:"deployment_id"`
	Group      string    `json:"group_id"`
	Release    string    `json:"release_id"`
	Platform   string    `json:"platform"`
	State      string    `json:"state"`
	Credential string    `json:"credential"`
	Expires    time.Time `json:"expires_at"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "deskrealm-deploy:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("expected fetch, issue, status or revoke")
	}
	var o options
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.StringVar(&o.center, "center", "", "HTTPS center origin")
	flags.StringVar(&o.tokenFile, "token-file", "", "file containing a scoped Windows deployment API token")
	flags.StringVar(&o.release, "release", "", "published release UUID")
	switch args[0] {
	case "fetch":
		flags.StringVar(&o.arch, "arch", "", "x64 or arm64")
		flags.StringVar(&o.output, "output", "", "installer destination")
	case "issue":
		flags.StringVar(&o.group, "group", "", "authorized device group UUID")
		flags.StringVar(&o.target, "target", "", "public device file from DeskRealm.exe prepare")
		flags.StringVar(&o.output, "output", "", "private output file for the device")
		flags.StringVar(&o.purpose, "purpose", "", "deployment purpose")
		flags.IntVar(&o.lifetime, "minutes", 30, "credential lifetime in minutes")
		fallthrough
	case "status", "revoke":
		flags.StringVar(&o.deployment, "deployment", "", "deployment UUID retained by the organization")
	default:
		return errors.New("expected fetch, issue, status or revoke")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || !uuid.MatchString(o.release) ||
		(args[0] != "fetch" && !uuid.MatchString(o.deployment)) {
		return errors.New("release and deployment must be UUIDs")
	}
	origin, err := url.Parse(o.center)
	if err != nil || origin.Scheme != "https" || origin.Hostname() == "" ||
		origin.User != nil || o.center != "https://"+origin.Host {
		return errors.New("center must be an HTTPS origin")
	}
	raw, err := readInput(o.tokenFile, true)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(string(raw))
	if !canonicalKey(token, base64.RawURLEncoding) {
		return errors.New("invalid Windows deployment API token file")
	}
	client := deploymentClient{http: &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}, base: o.center + "/v1/deployment/windows-releases/" + o.release, token: token}
	return client.execute(args[0], o, out)
}

func canonicalKey(value string, codec *base64.Encoding) bool {
	raw, err := codec.Strict().DecodeString(value)
	return err == nil && len(raw) == 32 && codec.EncodeToString(raw) == value
}

func (c deploymentClient) request(method, path string, body io.Reader, expected ...int) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("unable to contact the deployment API")
	}
	for _, status := range expected {
		if res.StatusCode == status {
			return res, nil
		}
	}
	_ = res.Body.Close()
	return nil, fmt.Errorf("deployment API returned HTTP %d", res.StatusCode)
}

func (c deploymentClient) execute(action string, o options, out io.Writer) error {
	switch action {
	case "fetch":
		return c.fetch(o)
	case "issue":
		return c.issue(o, out)
	case "status":
		res, err := c.request("GET", "/deployments/"+o.deployment, nil, 200)
		if err != nil {
			return err
		}
		raw, err := readResponse(res)
		if err != nil {
			return err
		}
		var result struct {
			Deployment   string `json:"deployment_id"`
			Release      string `json:"release_id"`
			State        string `json:"state"`
			Device       string `json:"device_id,omitempty"`
			Registration string `json:"registration_state,omitempty"`
			Expected     *int64 `json:"expected_policy_version"`
			Applied      *int64 `json:"applied_policy_version"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Release != o.release || result.Deployment != o.deployment {
			return errors.New("invalid deployment status")
		}
		return json.NewEncoder(out).Encode(result)
	case "revoke":
		res, err := c.request("POST", "/deployments/"+o.deployment+"/revoke", strings.NewReader("{}"), 204)
		if err != nil {
			return err
		}
		return res.Body.Close()
	default:
		return errors.New("invalid deployment action")
	}
}

func (c deploymentClient) issue(o options, out io.Writer) error {
	raw, err := readInput(o.target, false)
	if err != nil {
		return err
	}
	var target deviceTarget
	if json.Unmarshal(raw, &target) != nil || platform(target.Arch) == "" || !rustDeskID.MatchString(target.RustDeskID) ||
		!canonicalKey(target.PublicKey, base64.StdEncoding) || !uuid.MatchString(o.group) ||
		strings.TrimSpace(o.purpose) == "" || o.lifetime < 1 || o.lifetime > 1440 || o.output == "" {
		return errors.New("invalid public device identity or deployment parameters")
	}
	payload, err := json.Marshal(struct {
		deviceTarget
		Deployment string `json:"deployment_id"`
		Group      string `json:"group_id"`
		Purpose    string `json:"purpose"`
		Minutes    int    `json:"lifetime_minutes"`
	}{target, o.deployment, o.group, o.purpose, o.lifetime})
	if err != nil {
		return err
	}
	res, err := c.request("POST", "/deployments", strings.NewReader(string(payload)), 200, 201)
	if err != nil {
		return err
	}
	raw, err = readResponse(res)
	if err != nil {
		return err
	}
	var receipt enrollmentReceipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.Deployment != o.deployment || receipt.Release != o.release ||
		receipt.Group != o.group || receipt.Platform != platform(target.Arch) || receipt.Expires.IsZero() {
		return errors.New("invalid credential issuance response")
	}
	if receipt.State != "issued" || !canonicalKey(receipt.Credential, base64.RawURLEncoding) {
		return errors.New("deployment credential is no longer available")
	}
	if err := publishFile(o.output, strings.NewReader(string(raw)), int64(len(raw)), sha256.Sum256(raw)); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"deployment_id": receipt.Deployment, "expires_at": receipt.Expires})
}

func platform(arch string) string {
	switch arch {
	case "x64":
		return "windows-amd64"
	case "arm64":
		return "windows-arm64"
	}
	return ""
}

func (c deploymentClient) fetch(o options) error {
	if platform(o.arch) == "" || o.output == "" {
		return errors.New("installer architecture and output are required")
	}
	res, err := c.request("GET", "", nil, 200)
	if err != nil {
		return err
	}
	raw, err := readResponse(res)
	if err != nil {
		return err
	}
	var release struct {
		ID        string                             `json:"id"`
		Installer *windowsdelivery.InstallerEvidence `json:"installer_publication"`
	}
	if json.Unmarshal(raw, &release) != nil || release.ID != o.release || release.Installer == nil {
		return errors.New("signed installer is unavailable")
	}
	pkg := release.Installer.Installers[o.arch]
	if pkg.File != "DeskRealm-Setup-"+o.arch+".exe" || !windowsdelivery.ValidDigest(pkg.SHA) ||
		pkg.Size <= 0 || pkg.Size > (1<<30)+(80<<20) {
		return errors.New("invalid installer metadata")
	}
	expected, err := hex.DecodeString(pkg.SHA)
	if err != nil {
		return err
	}
	file, err := c.request("GET", "/installers/"+o.arch, nil, 200)
	if err != nil {
		return err
	}
	defer file.Body.Close()
	return publishFile(o.output, file.Body, pkg.Size, [32]byte(expected))
}

func readInput(path string, private bool) ([]byte, error) {
	if path == "" {
		return nil, errors.New("input file is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || (private && runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("input file type or permissions are invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readBounded(file)
}

func readResponse(res *http.Response) ([]byte, error) {
	defer res.Body.Close()
	return readBounded(res.Body)
}

func readBounded(in io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(in, maxJSON+1))
	if err != nil || len(raw) == 0 || len(raw) > maxJSON {
		return nil, errors.New("invalid or oversized deployment document")
	}
	return raw, nil
}

func publishFile(path string, src io.Reader, size int64, expected [32]byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".deskrealm-deploy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	check := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, check), io.LimitReader(src, size+1))
	flushErr := tmp.Sync()
	closeErr := tmp.Close()
	if err := errors.Join(copyErr, flushErr, closeErr); err != nil {
		return err
	}
	if n != size || [32]byte(check.Sum(nil)) != expected {
		return errors.New("deployment output digest differs")
	}
	if err := os.Link(tmp.Name(), path); !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return errors.New("deployment output already exists with different contents or permissions")
	}
	existing, err := os.Open(path)
	if err != nil {
		return err
	}
	defer existing.Close()
	check.Reset()
	n, err = io.Copy(check, io.LimitReader(existing, size+1))
	if err != nil || n != size || [32]byte(check.Sum(nil)) != expected {
		return errors.New("deployment output already exists with different contents")
	}
	return nil
}
