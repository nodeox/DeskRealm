// Package windowsdelivery verifies signed Windows development bundles staged by a build operator.
package windowsdelivery

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

const (
	signingPrefix = "DeskRealm/windows-delivery/v1\x00"
	maxManifest   = 64 << 10
	maxPackage    = 1 << 30
)

var (
	ErrUnavailable = errors.New("windows candidate files unavailable")
	ErrManifest    = errors.New("invalid Windows delivery manifest")
	ErrSignature   = errors.New("invalid Windows delivery signature")
	ErrPackage     = errors.New("Windows delivery package digest mismatch")
	digestPattern  = regexp.MustCompile("^[0-9a-f]{64}$")
	commitPattern  = regexp.MustCompile("^[0-9a-f]{40}$")
	idPattern      = regexp.MustCompile("^[a-z0-9][a-z0-9_-]{0,31}$")
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[a-z0-9]+(?:[.-][a-z0-9]+)*)?$`)
)

type Relay struct {
	ID      string `json:"id"`
	Address string `json:"address"`
}

type Center struct {
	Origin        string `json:"public_origin"`
	IDServer      string `json:"id_server"`
	RendezvousKey string `json:"rendezvous_public_key"`
	SigningKeyID  string `json:"signing_key_id"`
	SigningKey    string `json:"signing_public_key"`
	DefaultRelay  *Relay `json:"default_relay,omitempty"`
}

func (c Center) Matches(other Center) bool {
	return c.Origin == other.Origin && c.IDServer == other.IDServer && c.RendezvousKey == other.RendezvousKey &&
		c.SigningKeyID == other.SigningKeyID && c.SigningKey == other.SigningKey &&
		(c.DefaultRelay == nil && other.DefaultRelay == nil ||
			c.DefaultRelay != nil && other.DefaultRelay != nil && *c.DefaultRelay == *other.DefaultRelay)
}

type Brand struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Name     string `json:"name"`
	IconSHA  string `json:"icon_sha256"`
}

type Build struct {
	ManifestSHA string `json:"manifest_sha256"`
	ClientSHA   string `json:"client_sha256"`
}

type Approval struct {
	Schema  int              `json:"schema"`
	Version string           `json:"version"`
	Brand   Brand            `json:"brand"`
	Center  Center           `json:"center"`
	Groups  []string         `json:"group_ids"`
	Source  string           `json:"source_commit"`
	Builds  map[string]Build `json:"builds"`
}

type Package struct {
	File string `json:"file"`
	SHA  string `json:"sha256"`
	Size int64  `json:"bytes"`
}

type Manifest struct {
	Schema   int                `json:"schema"`
	Scope    string             `json:"signature_scope"`
	Approval Approval           `json:"approval"`
	Packages map[string]Package `json:"packages"`
}

type Verifier struct {
	directory string
	key       ed25519.PublicKey
	center    Center
}

func Configure(directory, publicKeyFile string, center Center) (*Verifier, error) {
	if directory == "" && publicKeyFile == "" {
		return nil, nil
	}
	if !filepath.IsAbs(directory) || publicKeyFile == "" || !strings.HasPrefix(center.Origin, "https://") ||
		center.IDServer == "" || center.RendezvousKey == "" || center.SigningKey == "" || !idPattern.MatchString(center.SigningKeyID) {
		return nil, errors.New("Windows delivery requires an absolute staging directory, public verification key and HTTPS center identity")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("Windows delivery staging directory unavailable")
	}
	_ = root.Close()
	keyFile, err := os.Open(publicKeyFile)
	if err != nil {
		return nil, errors.New("Windows delivery public key unavailable")
	}
	defer keyFile.Close()
	raw, err := io.ReadAll(io.LimitReader(keyFile, 1025))
	block, rest := pem.Decode(raw)
	if err != nil || len(raw) > 1024 || block == nil || block.Type != "PUBLIC KEY" || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("Windows delivery requires a PEM Ed25519 public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	key, ok := parsed.(ed25519.PublicKey)
	if err != nil || !ok {
		return nil, errors.New("Windows delivery requires a PEM Ed25519 public key")
	}
	if base64.StdEncoding.EncodeToString(key) == center.RendezvousKey ||
		base64.RawURLEncoding.EncodeToString(key) == center.SigningKey {
		return nil, errors.New("Windows development verification requires a key separate from the center keys")
	}
	return &Verifier{directory: directory, key: key, center: center}, nil
}

func (v *Verifier) KeySHA() string {
	return digest(v.key)
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func ValidDigest(value string) bool { return digestPattern.MatchString(value) }

func validUUID(value string) bool {
	var id pgtype.UUID
	return id.Scan(value) == nil && id.Bytes != [16]byte{} && id.String() == value
}

func (m Manifest) valid() bool {
	a := m.Approval
	if m.Schema != 1 || m.Scope != "development" || a.Schema != 1 || len(a.Version) > 64 || !versionPattern.MatchString(a.Version) ||
		!idPattern.MatchString(a.Brand.ID) || a.Brand.Revision < 1 || a.Brand.Revision > (1<<53)-1 || !ValidDigest(a.Brand.IconSHA) ||
		strings.TrimSpace(a.Brand.Name) != a.Brand.Name || utf8.RuneCountInString(a.Brand.Name) > 64 || a.Brand.Name == "" ||
		strings.ContainsFunc(a.Brand.Name, func(r rune) bool { return unicode.In(r, unicode.C) }) ||
		!commitPattern.MatchString(a.Source) || len(a.Groups) < 1 || len(a.Groups) > 50 || !slices.IsSorted(a.Groups) ||
		len(a.Builds) != 2 || len(m.Packages) != 2 {
		return false
	}
	for i, group := range a.Groups {
		if !validUUID(group) || (i > 0 && group == a.Groups[i-1]) {
			return false
		}
	}
	for _, arch := range []string{"x64", "arm64"} {
		build, artifact := a.Builds[arch], m.Packages[arch]
		if !ValidDigest(build.ManifestSHA) || !ValidDigest(build.ClientSHA) || !ValidDigest(artifact.SHA) ||
			artifact.File != "DeskRealm-"+arch+".zip" || artifact.Size < 1 || artifact.Size > maxPackage {
			return false
		}
	}
	return true
}

func strictJSON(raw []byte, out any) error {
	// Record each object member once, including case-folded aliases accepted by
	// encoding/json when projecting the signed document to structs.
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 12 {
			return ErrManifest
		}
		token, err := dec.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			names := map[string]bool{}
			for dec.More() {
				name, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := name.(string)
				key = strings.ToLower(key)
				if !ok || names[key] {
					return ErrManifest
				}
				names[key] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		case json.Delim('['):
			for dec.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return nil
		}
		_, err = dec.Token()
		return err
	}
	if !utf8.Valid(raw) || walk(0) != nil {
		return ErrManifest
	}
	typed := json.NewDecoder(strings.NewReader(string(raw)))
	typed.DisallowUnknownFields()
	if typed.Decode(out) != nil || typed.Decode(new(any)) != io.EOF {
		return ErrManifest
	}
	return nil
}

func (v *Verifier) candidate(id string) (*os.Root, error) {
	if !ValidDigest(id) {
		return nil, ErrManifest
	}
	root, err := os.OpenRoot(v.directory)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer root.Close()
	candidate, err := root.OpenRoot(id)
	if err != nil {
		return nil, ErrUnavailable
	}
	return candidate, nil
}

func regular(root *os.Root, name string) (*os.File, int64, error) {
	info, err := root.Stat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, ErrUnavailable
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, 0, ErrUnavailable
	}
	fresh, err := file.Stat()
	if err != nil || !fresh.Mode().IsRegular() {
		file.Close()
		return nil, 0, ErrUnavailable
	}
	return file, fresh.Size(), nil
}

func readSmall(root *os.Root, path string, limit int64) ([]byte, error) {
	file, size, err := regular(root, path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if size > limit {
		return nil, ErrManifest
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, ErrUnavailable
	}
	if int64(len(raw)) > limit {
		return nil, ErrManifest
	}
	return raw, nil
}

func (v *Verifier) readManifest(root *os.Root, id string) ([]byte, Manifest, error) {
	var manifest Manifest
	raw, err := readSmall(root, "release.json", maxManifest)
	if err != nil {
		return nil, manifest, err
	}
	if digest(raw) != id || strictJSON(raw, &manifest) != nil || !manifest.valid() {
		return nil, manifest, ErrManifest
	}
	if !manifest.Approval.Center.Matches(v.center) {
		return nil, manifest, fmt.Errorf("%w: center identity differs", ErrManifest)
	}
	return raw, manifest, nil
}

func (v *Verifier) Read(id string) (Manifest, error) {
	root, err := v.candidate(id)
	if err != nil {
		return Manifest{}, err
	}
	defer root.Close()
	_, manifest, err := v.readManifest(root, id)
	return manifest, err
}
