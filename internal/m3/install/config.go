package install

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"deskrealm/internal/m1"
)

const (
	httpsProvided = "provided"
	httpsExternal = "external"
	maxConfigSize = 128 << 10
)

type imageConfig struct {
	Center     string `json:"center"`
	PostgreSQL string `json:"postgresql"`
	Rendezvous string `json:"rendezvous"`
}

type httpsConfig struct {
	Mode          string `json:"mode"`
	Certificate   string `json:"certificate,omitempty"`
	PrivateKey    string `json:"private_key,omitempty"`
	CACertificate string `json:"ca_certificate,omitempty"`
}

type portConfig struct {
	API             uint16 `json:"api,omitempty"`
	RendezvousCheck uint16 `json:"rendezvous_check,omitempty"`
	Rendezvous      uint16 `json:"rendezvous,omitempty"`
}

type config struct {
	Version      int         `json:"version"`
	Directory    string      `json:"directory"`
	PublicOrigin string      `json:"public_origin"`
	IDServer     string      `json:"id_server"`
	HTTPS        httpsConfig `json:"https"`
	Images       imageConfig `json:"images"`
	Ports        portConfig  `json:"ports"`
}

var (
	hostLabel = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
	imagePart = regexp.MustCompile(`^[a-z0-9]+(?:(?:[.]|__?|[-]+)[a-z0-9]+)*$`)
	imageTag  = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)
	digestHex = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

func loadConfig(path string, now time.Time) (config, error) {
	var value config
	if path == "" {
		return value, errors.New("--config is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return value, fmt.Errorf("read configuration metadata: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxConfigSize {
		return value, errors.New("configuration must be a regular JSON file of at most 128 KiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return value, fmt.Errorf("open configuration: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxConfigSize+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("invalid configuration JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return value, errors.New("configuration must contain exactly one JSON object")
	}
	return validateConfig(value, now)
}

func validateConfig(c config, now time.Time) (config, error) {
	if c.Version != 1 {
		return c, errors.New("configuration version must be 1")
	}
	directory, err := canonicalDirectory(c.Directory)
	if err != nil {
		return c, err
	}
	c.Directory = directory
	origin, err := m1.ParseOrigin(c.PublicOrigin, false)
	if err != nil {
		return c, fmt.Errorf("public_origin: %w", err)
	}
	if !validHost(origin.Hostname()) || strings.HasSuffix(origin.Host, ":") {
		return c, errors.New("public_origin must have a valid hostname or IP address")
	}
	httpsPort := uint16(443)
	if value := origin.Port(); value != "" {
		if httpsPort, err = parsePort(value); err != nil {
			return c, errors.New("public_origin has an invalid port")
		}
	}
	host := canonicalHost(origin.Hostname())
	switch {
	case httpsPort != 443:
		origin.Host = joinPort(host, httpsPort)
	case strings.ContainsRune(host, ':'):
		origin.Host = "[" + host + "]"
	default:
		origin.Host = host
	}
	c.PublicOrigin = origin.String()
	if c.IDServer, err = normalizeEndpoint(c.IDServer); err != nil {
		return c, fmt.Errorf("id_server: %w", err)
	}
	for _, image := range []struct{ field, value string }{
		{"center", c.Images.Center},
		{"postgresql", c.Images.PostgreSQL},
		{"rendezvous", c.Images.Rendezvous},
	} {
		if !validImage(image.value) {
			return c, fmt.Errorf("images.%s must use an image name pinned to a SHA-256 digest", image.field)
		}
	}
	switch c.HTTPS.Mode {
	case httpsExternal:
		if c.HTTPS.Certificate != "" || c.HTTPS.PrivateKey != "" {
			return c, errors.New("external HTTPS uses the reverse proxy's certificate and private key")
		}
		if c.Ports.API == 0 {
			c.Ports.API = 8080
		}
	case httpsProvided:
		if err := checkCertificate(c.HTTPS, origin.Hostname(), now); err != nil {
			return c, err
		}
		if c.Ports.API == 0 {
			c.Ports.API = httpsPort
		} else if c.Ports.API != httpsPort {
			return c, errors.New("provided HTTPS must publish the public_origin port")
		}
	default:
		return c, errors.New("https.mode must be provided or external")
	}
	if c.HTTPS.CACertificate != "" {
		path := c.HTTPS.CACertificate
		info, err := os.Stat(path)
		if !filepath.IsAbs(path) || err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return c, errors.New("https.ca_certificate must be an absolute path to a PEM certificate file")
		}
		data, err := os.ReadFile(path)
		if err != nil || !x509.NewCertPool().AppendCertsFromPEM(data) {
			return c, errors.New("https.ca_certificate does not contain a readable PEM certificate")
		}
	}
	if c.Ports.RendezvousCheck == 0 {
		c.Ports.RendezvousCheck = 21115
	}
	if c.Ports.Rendezvous == 0 {
		c.Ports.Rendezvous = 21116
	}
	if c.Ports.API == c.Ports.RendezvousCheck || c.Ports.API == c.Ports.Rendezvous ||
		c.Ports.RendezvousCheck == c.Ports.Rendezvous {
		return c, errors.New("API, rendezvous check and rendezvous TCP ports must be distinct")
	}
	return c, nil
}

func canonicalDirectory(value string) (string, error) {
	if !filepath.IsAbs(value) {
		return "", errors.New("directory must be an absolute path")
	}
	value = filepath.Clean(value)
	parent := filepath.Dir(value)
	if parent == value {
		return "", errors.New("directory must not be the filesystem root")
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve installation directory parent: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("installation directory parent must be a directory")
	}
	value = filepath.Join(resolved, filepath.Base(value))
	if strings.ContainsRune(value, '$') || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", errors.New("directory cannot contain dollar signs or control characters")
	}
	info, err = os.Lstat(value)
	if err == nil && !info.IsDir() {
		return "", errors.New("installation directory must be a directory and cannot be a symlink")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect installation directory: %w", err)
	}
	return value, nil
}

func parsePort(value string) (uint16, error) {
	port, err := strconv.ParseUint(value, 10, 16)
	if err != nil || port == 0 {
		return 0, errors.New("expected a port from 1 to 65535")
	}
	return uint16(port), nil
}

func validHost(host string) bool {
	if address, err := netip.ParseAddr(host); err == nil {
		return address.Zone() == "" && !address.IsUnspecified() && !address.IsMulticast()
	}
	name := strings.TrimSuffix(host, ".")
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, part := range strings.Split(name, ".") {
		if !hostLabel.MatchString(part) {
			return false
		}
	}
	return true
}

func canonicalHost(host string) string {
	if address, err := netip.ParseAddr(host); err == nil {
		return address.String()
	}
	return strings.ToLower(host)
}

func normalizeEndpoint(value string) (string, error) {
	host, port, err := net.SplitHostPort(value)
	if err != nil || !validHost(host) {
		return "", errors.New("expected a DNS name or IP address with port, such as id.example.test:21116")
	}
	number, err := parsePort(port)
	if err != nil {
		return "", err
	}
	return joinPort(canonicalHost(host), number), nil
}

func validImage(ref string) bool {
	name, digest, ok := strings.Cut(ref, "@sha256:")
	if !ok || !digestHex.MatchString(digest) || len(name) > 255 {
		return false
	}
	parts := strings.Split(name, "/")
	last := len(parts) - 1
	if path, tag, tagged := strings.Cut(parts[last], ":"); tagged {
		if !imageTag.MatchString(tag) {
			return false
		}
		parts[last] = path
	}
	if last > 0 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		registry := parts[0]
		if strings.ContainsRune(registry, ':') {
			if _, err := normalizeEndpoint(registry); err != nil {
				return false
			}
		} else if !validHost(registry) {
			return false
		}
		parts = parts[1:]
	}
	for _, part := range parts {
		if !imagePart.MatchString(part) {
			return false
		}
	}
	return true
}

func checkCertificate(config httpsConfig, name string, now time.Time) error {
	if !filepath.IsAbs(config.Certificate) || !filepath.IsAbs(config.PrivateKey) {
		return errors.New("provided HTTPS requires absolute certificate and private_key paths")
	}
	// Certificate managers may expose the current files through symlinks.
	cert, certErr := os.Stat(config.Certificate)
	key, keyErr := os.Stat(config.PrivateKey)
	if certErr != nil || keyErr != nil || !cert.Mode().IsRegular() || !key.Mode().IsRegular() ||
		cert.Size() > 1<<20 || key.Size() > 1<<20 || key.Mode().Perm()&0037 != 0 {
		return errors.New("TLS certificate and key must be readable regular files with a restricted private key")
	}
	pair, err := tls.LoadX509KeyPair(config.Certificate, config.PrivateKey)
	if err != nil {
		return errors.New("TLS certificate and key must contain a matching PEM pair")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return errors.New("TLS certificate is not currently valid")
	}
	if err := leaf.VerifyHostname(name); err != nil {
		return errors.New("TLS certificate does not match the public_origin hostname")
	}
	return nil
}
