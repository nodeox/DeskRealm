package install

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
)

type offlineConfig struct {
	Manifest  string `json:"manifest"`
	Signature string `json:"signature"`
	PublicKey string `json:"trusted_public_key"`
}

func (o offlineConfig) validate() error {
	if o == (offlineConfig{}) {
		return nil
	}
	for _, path := range []string{o.Manifest, o.Signature, o.PublicKey} {
		if !filepath.IsAbs(path) || strings.IndexFunc(path, unicode.IsControl) >= 0 {
			return errors.New("offline manifest, signature and trusted public key must be absolute file paths")
		}
	}
	return nil
}

type offlineImage struct {
	Reference string `json:"reference"`
	ID        string `json:"image_id"`
}

type offlineManifest struct {
	Version  int    `json:"version"`
	Platform string `json:"platform"`
	Archive  struct {
		File   string `json:"file"`
		SHA256 string `json:"sha256"`
		Bytes  int64  `json:"bytes"`
	} `json:"archive"`
	Images struct {
		Center     offlineImage `json:"center"`
		PostgreSQL offlineImage `json:"postgresql"`
		Rendezvous offlineImage `json:"rendezvous"`
	} `json:"images"`
}

type offlineIdentity struct {
	Platform       string      `json:"platform"`
	ManifestSHA256 string      `json:"manifest_sha256"`
	SignerSHA256   string      `json:"signer_sha256"`
	ImageIDs       imageConfig `json:"image_ids"`
}

func (o offlineIdentity) valid(configured bool) bool {
	if !configured {
		return o == (offlineIdentity{})
	}
	return (o.Platform == "linux/amd64" || o.Platform == "linux/arm64") &&
		digestHex.MatchString(o.ManifestSHA256) && digestHex.MatchString(o.SignerSHA256) &&
		validImageID(o.ImageIDs.Center) && validImageID(o.ImageIDs.PostgreSQL) && validImageID(o.ImageIDs.Rendezvous)
}

func validImageID(value string) bool {
	digest, ok := strings.CutPrefix(value, "sha256:")
	return ok && digestHex.MatchString(digest)
}

func (center publicDeployment) runtimeImages(configured imageConfig) imageConfig {
	if center.Offline.Platform != "" {
		return center.Offline.ImageIDs
	}
	return configured
}

type offlineBundle struct {
	manifest offlineManifest
	identity offlineIdentity
}

func readOfflineBundle(c config) (offlineBundle, error) {
	var bundle offlineBundle
	data, err := readOfflineFile(c.Offline.Manifest, maxConfigSize)
	if err != nil {
		return bundle, errors.New("cannot read offline manifest")
	}
	signature, err := readOfflineFile(c.Offline.Signature, ed25519.SignatureSize)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return bundle, errors.New("offline signature must be a raw Ed25519 signature")
	}
	key, err := readOfflineFile(c.Offline.PublicKey, 8192)
	if err != nil {
		return bundle, errors.New("cannot read trusted offline public key")
	}
	block, rest := pem.Decode(key)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return bundle, errors.New("trusted offline key must be a single Ed25519 public key PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	public, ok := parsed.(ed25519.PublicKey)
	if err != nil || !ok {
		return bundle, errors.New("trusted offline key must be an Ed25519 public key")
	}
	if !ed25519.Verify(public, data, signature) {
		return bundle, errors.New("offline manifest signature does not match the trusted public key")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	m := &bundle.manifest
	if err := decoder.Decode(m); err != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) || m.Version != 1 {
		return bundle, errors.New("invalid offline manifest version or structure")
	}
	if !filepath.IsLocal(m.Archive.File) || filepath.Base(m.Archive.File) != m.Archive.File ||
		m.Archive.File == "." || strings.ContainsRune(m.Archive.File, '\\') ||
		strings.IndexFunc(m.Archive.File, unicode.IsControl) >= 0 ||
		m.Archive.Bytes <= 0 || !digestHex.MatchString(m.Archive.SHA256) {
		return bundle, errors.New("offline manifest must specify an archive basename, byte count and SHA-256")
	}
	references := imageConfig{
		Center: m.Images.Center.Reference, PostgreSQL: m.Images.PostgreSQL.Reference, Rendezvous: m.Images.Rendezvous.Reference,
	}
	if references != c.Images {
		return bundle, errors.New("offline manifest image references differ from installation configuration")
	}
	bundle.identity = offlineIdentity{
		Platform: m.Platform, ManifestSHA256: sha256Hex(data), SignerSHA256: sha256Hex(public),
		ImageIDs: imageConfig{Center: m.Images.Center.ID, PostgreSQL: m.Images.PostgreSQL.ID, Rendezvous: m.Images.Rendezvous.ID},
	}
	if !bundle.identity.valid(true) {
		return bundle, errors.New("offline manifest must specify a supported platform and three SHA-256 image IDs")
	}
	return bundle, nil
}

func readOfflineFile(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("offline file must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("cannot read offline file within its size limit")
	}
	return data, nil
}

func ensureOfflineImages(ctx context.Context, c config, recorded offlineIdentity, platform string, activated bool, run dockerRunner) error {
	if recorded.Platform != platform {
		return fmt.Errorf("offline bundle is for %s, Docker Engine runs %s", recorded.Platform, platform)
	}
	missing := func() string {
		for _, image := range []struct{ role, id string }{
			{"database", recorded.ImageIDs.PostgreSQL}, {"center", recorded.ImageIDs.Center}, {"rendezvous", recorded.ImageIDs.Rendezvous},
		} {
			data, err := run(ctx, "image", "inspect", "--format", "{{.Id}} {{.Os}}/{{.Architecture}}", image.id)
			if err != nil || strings.TrimSpace(string(data)) != image.id+" "+platform {
				return image.role
			}
		}
		return ""
	}
	if activated && missing() == "" {
		return nil
	}
	bundle, err := readOfflineBundle(c)
	if err != nil {
		return err
	}
	if bundle.identity != recorded {
		return errors.New("offline manifest differs from the prepared deployment")
	}
	if err := loadOfflineArchive(ctx, c, bundle, run); err != nil {
		return err
	}
	if role := missing(); role != "" {
		return fmt.Errorf("%s image ID or platform is absent after offline archive import", role)
	}
	return nil
}

type archiveReader struct {
	ctx context.Context
	io.Reader
}

func (r archiveReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func loadOfflineArchive(ctx context.Context, c config, bundle offlineBundle, run dockerRunner) error {
	root, err := os.OpenRoot(filepath.Dir(c.Offline.Manifest))
	if err != nil {
		return errors.New("cannot open offline bundle directory")
	}
	defer root.Close()
	a := bundle.manifest.Archive
	file, err := root.OpenFile(a.File, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("cannot open offline image archive")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != a.Bytes {
		return errors.New("offline image archive type or byte count does not match the signed manifest")
	}
	// Docker reads only the private copy whose complete contents are hashed here.
	staged, err := os.CreateTemp(c.Directory, ".offline-images-")
	if err != nil {
		return fmt.Errorf("create private offline archive staging file: %w", err)
	}
	defer os.Remove(staged.Name())
	hash := sha256.New()
	_, copyErr := io.CopyN(io.MultiWriter(staged, hash), archiveReader{ctx, file}, a.Bytes)
	closeErr := staged.Close()
	if copyErr != nil || closeErr != nil {
		return errors.New("cannot stage the complete offline image archive")
	}
	var trailing [1]byte
	if n, err := file.Read(trailing[:]); n != 0 || !errors.Is(err, io.EOF) || hex.EncodeToString(hash.Sum(nil)) != a.SHA256 {
		return errors.New("offline image archive SHA-256 does not match the signed manifest")
	}
	if _, err := run(ctx, "image", "load", "--quiet", "--input", staged.Name()); err != nil {
		return errors.New("Docker rejected the verified offline image archive")
	}
	return nil
}
