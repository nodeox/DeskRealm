package m0

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode"
)

const (
	center       = "center"
	relay        = "relay"
	manifestFile = "deployment.json"
	composeFile  = "compose.json"
	dataDir      = "data"
	privateFile  = "id_ed25519"
	publicFile   = "id_ed25519.pub"
)

type deployment struct {
	Version        int    `json:"version"`
	Role           string `json:"role"`
	Directory      string `json:"directory"`
	Project        string `json:"project"`
	CenterEndpoint string `json:"center_endpoint,omitempty"`
	RelayEndpoint  string `json:"relay_endpoint,omitempty"`
	PublicKey      string `json:"public_key"`
	Fingerprint    string `json:"public_key_sha256"`
}

var label = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

func endpoint(host, port string) (string, error) {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		addr, err := netip.ParseAddr(host[1 : len(host)-1])
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", errors.New("expected bracketed IPv6 address")
		}
		return net.JoinHostPort(addr.String(), port), nil
	}
	if addr, err := netip.ParseAddr(host); err == nil && addr.Zone() == "" {
		return net.JoinHostPort(addr.String(), port), nil
	}
	name := strings.TrimSuffix(host, ".")
	if len(name) == 0 || len(name) > 253 {
		return "", errors.New("expected a DNS name or IP address without a port")
	}
	for _, part := range strings.Split(name, ".") {
		if !label.MatchString(part) {
			return "", errors.New("expected a DNS name or IP address without a port")
		}
	}
	return net.JoinHostPort(strings.ToLower(host), port), nil
}

func decodeKey(value string, size int) ([]byte, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(key) != size || base64.StdEncoding.EncodeToString(key) != value {
		return nil, errors.New("invalid canonical Base64 key")
	}
	return key, nil
}

func projectName(role, dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return fmt.Sprintf("deskrealm-m0-%s-%x", role, sum[:8])
}

func fingerprint(key []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(key))
}

func deploymentDir(input string) (string, error) {
	if input == "" {
		return "", errors.New("--dir is required")
	}
	abs, err := filepath.Abs(input)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("resolve deployment parent: %w", err)
	}
	if abs == filepath.Dir(abs) {
		return "", errors.New("deployment directory must not be the filesystem root")
	}
	canonical := filepath.Join(parent, filepath.Base(abs))
	if strings.ContainsRune(canonical, '$') || strings.IndexFunc(canonical, unicode.IsControl) >= 0 {
		return "", errors.New("deployment path cannot contain dollar signs or control characters")
	}
	return canonical, nil
}

func lockDeployment(dir string) (*os.File, error) {
	// The persistent sidecar ensures all invocations lock the same inode.
	path := dir + ".lock"
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("open deployment lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		err = errors.New("deployment lock must be a private regular file")
	}
	if err == nil {
		for {
			err = syscall.Flock(fd, syscall.LOCK_EX)
			if !errors.Is(err, syscall.EINTR) {
				break
			}
		}
	}
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("lock deployment: %w", err)
	}
	return file, nil
}

func desiredDeployment(dir, role, centerHost, relayHost, key string) (deployment, error) {
	d := deployment{Version: 1, Role: role, Directory: dir, Project: projectName(role, dir)}
	switch role {
	case center:
		if key != "" {
			return d, errors.New("center initializes its own key")
		}
		var err error
		d.CenterEndpoint, err = endpoint(centerHost, "21116")
		if err != nil {
			return d, fmt.Errorf("--center-host: %w", err)
		}
		d.RelayEndpoint, err = endpoint(relayHost, "21117")
		if err != nil {
			return d, fmt.Errorf("--relay-host: %w", err)
		}
	case relay:
		if centerHost != "" || relayHost != "" {
			return d, errors.New("relay only accepts --center-key and --dir")
		}
		raw, err := decodeKey(key, ed25519.PublicKeySize)
		if err != nil {
			return d, errors.New("--center-key must be the 32-byte center public key in canonical Base64")
		}
		d.PublicKey, d.Fingerprint = key, fingerprint(raw)
	default:
		return d, errors.New("--role must be center or relay")
	}
	return d, nil
}

func initialize(d deployment) (deployment, error) {
	info, err := os.Lstat(d.Directory)
	switch {
	case err == nil:
		if !info.IsDir() {
			return d, errors.New("deployment directory must be a real directory")
		}
		entries, err := os.ReadDir(d.Directory)
		if err != nil {
			return d, err
		}
		if len(entries) != 0 {
			current, err := loadDeployment(d.Directory)
			if err != nil {
				return d, err
			}
			if d.Role == center {
				d.PublicKey, d.Fingerprint = current.PublicKey, current.Fingerprint
			}
			if d != current {
				return d, errors.New("requested configuration differs from the persisted deployment")
			}
			return current, nil
		}
	case !errors.Is(err, os.ErrNotExist):
		return d, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(d.Directory), ".deskrealm-m0-")
	if err != nil {
		return d, err
	}
	defer os.RemoveAll(stage)
	if d.Role == center {
		if err := os.Mkdir(filepath.Join(stage, dataDir), 0700); err != nil {
			return d, err
		}
		for {
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return d, err
			}
			d.PublicKey = base64.StdEncoding.EncodeToString(pub)
			if strings.Contains(d.PublicKey, "/") {
				continue
			}
			d.Fingerprint = fingerprint(pub)
			if err := os.WriteFile(filepath.Join(stage, dataDir, privateFile), []byte(base64.StdEncoding.EncodeToString(priv)), 0600); err != nil {
				return d, err
			}
			if err := os.WriteFile(filepath.Join(stage, dataDir, publicFile), []byte(d.PublicKey), 0600); err != nil {
				return d, err
			}
			break
		}
	}
	manifest, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return d, err
	}
	model, err := composeJSON(d)
	if err != nil {
		return d, err
	}
	for name, body := range map[string][]byte{manifestFile: manifest, composeFile: model} {
		if err := os.WriteFile(filepath.Join(stage, name), body, 0600); err != nil {
			return d, err
		}
	}
	if err := os.Rename(stage, d.Directory); err != nil {
		return d, fmt.Errorf("publish deployment: %w", err)
	}
	return d, nil
}

func readRegular(path string, private bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || (private && info.Mode().Perm()&0077 != 0) {
		return nil, fmt.Errorf("%s must be a regular file with the required permissions", path)
	}
	return os.ReadFile(path)
}

func loadDeployment(dir string) (deployment, error) {
	var d deployment
	info, err := os.Lstat(dir)
	if err != nil {
		return d, fmt.Errorf("open deployment: %w", err)
	}
	if !info.IsDir() {
		return d, errors.New("deployment directory must be a real directory")
	}
	raw, err := readRegular(filepath.Join(dir, manifestFile), false)
	if err != nil {
		return d, fmt.Errorf("restore deployment record: %w", err)
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, errors.New("invalid deployment record")
	}
	if d.Version != 1 || d.Directory != dir || d.Project != projectName(d.Role, dir) {
		return d, errors.New("deployment record version or directory does not match")
	}
	pub, err := decodeKey(d.PublicKey, ed25519.PublicKeySize)
	if err != nil || d.Fingerprint != fingerprint(pub) {
		return d, errors.New("deployment public key or fingerprint does not match")
	}
	switch d.Role {
	case center:
		for address, port := range map[string]string{d.CenterEndpoint: "21116", d.RelayEndpoint: "21117"} {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return d, errors.New("invalid persisted endpoint")
			}
			canonical, err := endpoint(host, port)
			if err != nil || canonical != address {
				return d, errors.New("invalid persisted endpoint")
			}
		}
		if err := validateIdentity(d); err != nil {
			return d, err
		}
	case relay:
		if d.CenterEndpoint != "" || d.RelayEndpoint != "" {
			return d, errors.New("unexpected persisted relay configuration")
		}
	default:
		return d, errors.New("invalid deployment role")
	}
	expected, err := composeJSON(d)
	if err != nil {
		return d, err
	}
	actual, err := readRegular(filepath.Join(dir, composeFile), false)
	if err != nil {
		return d, err
	}
	if !bytes.Equal(actual, expected) {
		return d, errors.New("compose.json differs from the persisted deployment")
	}
	return d, nil
}

func validateIdentity(d deployment) error {
	data := filepath.Join(d.Directory, dataDir)
	info, err := os.Lstat(data)
	if err != nil {
		return fmt.Errorf("restore center data directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("restore private center data directory %s", data)
	}
	if _, err := os.Lstat(filepath.Join(data, ".env")); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("unexpected RustDesk environment override in %s", data)
	}
	privPath := filepath.Join(data, privateFile)
	encoded, err := readRegular(privPath, true)
	if err != nil {
		return fmt.Errorf("restore center private key: %w", err)
	}
	priv, err := decodeKey(string(encoded), ed25519.PrivateKeySize)
	if err != nil {
		return fmt.Errorf("restore invalid center private key at %s", privPath)
	}
	expected := ed25519.NewKeyFromSeed(priv[:ed25519.SeedSize])
	stored, err := readRegular(filepath.Join(data, publicFile), false)
	if err != nil {
		return fmt.Errorf("restore center public key: %w", err)
	}
	derived := base64.StdEncoding.EncodeToString(expected[ed25519.SeedSize:])
	if !bytes.Equal(expected, priv) || string(stored) != derived || d.PublicKey != derived {
		return fmt.Errorf("center identity mismatch in %s; restore the original key pair", data)
	}
	return nil
}
