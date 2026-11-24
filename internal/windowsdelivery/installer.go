package windowsdelivery

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path"
)

const (
	installerStatementPrefix = "DeskRealm/windows-installer-attestation/v1\x00"
	maxInstaller             = maxPackage + (80 << 20)
)

type InstallerEvidence struct {
	Schema       int                `json:"schema"`
	Scope        string             `json:"scope"`
	ReleaseSHA   string             `json:"release_sha256"`
	SourceCommit string             `json:"source_commit"`
	SignerSHA    string             `json:"signer_sha256"`
	Installers   map[string]Package `json:"installers"`
}

type InstallerVerifier struct {
	stage     string
	key       ed25519.PublicKey
	signerSHA string
}

func (v *Verifier) ConfigureInstallers(keyPath, signerSHA string) (*InstallerVerifier, error) {
	if keyPath == "" && signerSHA == "" {
		return nil, nil
	}
	if v == nil || keyPath == "" || !ValidDigest(signerSHA) {
		return nil, errors.New("Windows installer publishing requires ZIP verification, an attestation key and a code signer SHA-256")
	}
	key, err := readEd25519PublicKey(keyPath)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(key, v.key) || base64.StdEncoding.EncodeToString(key) == v.center.RendezvousKey ||
		base64.RawURLEncoding.EncodeToString(key) == v.center.SigningKey {
		return nil, errors.New("Windows installer attestations require a separate verification key")
	}
	return &InstallerVerifier{stage: v.directory, key: key, signerSHA: signerSHA}, nil
}

func (v *InstallerVerifier) KeySHA() string { return digest(v.key) }

func validInstaller(arch string, pkg Package) bool {
	return (arch == "x64" || arch == "arm64") && pkg.File == "DeskRealm-Setup-"+arch+".exe" &&
		ValidDigest(pkg.SHA) && pkg.Size > 0 && pkg.Size <= maxInstaller
}

func (v *InstallerVerifier) readStatement(root *os.Root, id, release string) (InstallerEvidence, []byte, []byte, error) {
	var evidence InstallerEvidence
	raw, err := readSmall(root, "installer-verification.json", maxManifest)
	if err != nil {
		return evidence, nil, nil, err
	}
	if digest(raw) != id || strictJSON(raw, &evidence) != nil || evidence.Schema != 1 ||
		evidence.Scope != "authenticode-development" || evidence.ReleaseSHA != release ||
		evidence.SignerSHA != v.signerSHA || !commitPattern.MatchString(evidence.SourceCommit) ||
		len(evidence.Installers) != 2 || !validInstaller("x64", evidence.Installers["x64"]) ||
		!validInstaller("arm64", evidence.Installers["arm64"]) {
		return evidence, nil, nil, ErrManifest
	}
	sig, err := readSmall(root, "installer-verification.sig", ed25519.SignatureSize)
	if err != nil {
		return evidence, nil, nil, err
	}
	if !ed25519.Verify(v.key, append([]byte(installerStatementPrefix), raw...), sig) {
		return evidence, nil, nil, ErrSignature
	}
	return evidence, raw, sig, nil
}

// PreserveInstallers copies the signed statement and its exact binaries to a
// separate snapshot. The attestation signer vouches for external Authenticode verification.
func (s *Releases) PreserveInstallers(ctx context.Context, v *InstallerVerifier, id, release string) (InstallerEvidence, error) {
	if !ValidDigest(id) || !ValidDigest(release) {
		return InstallerEvidence{}, ErrManifest
	}
	stage, err := os.OpenRoot(v.stage)
	if err != nil {
		return InstallerEvidence{}, ErrUnavailable
	}
	defer stage.Close()
	source, err := stage.OpenRoot(path.Join("installers", id))
	if err != nil {
		return InstallerEvidence{}, ErrUnavailable
	}
	defer source.Close()
	evidence, raw, sig, err := v.readStatement(source, id, release)
	if err != nil {
		return InstallerEvidence{}, err
	}
	root, err := os.OpenRoot(s.directory)
	if err != nil {
		return InstallerEvidence{}, ErrUnavailable
	}
	defer root.Close()
	name := "installers-" + id
	check := func() error {
		snapshot, err := root.OpenRoot(name)
		if err != nil {
			return err
		}
		defer snapshot.Close()
		if _, _, _, err := v.readStatement(snapshot, id, release); err != nil {
			return err
		}
		return checkPackages(ctx, snapshot, evidence.Installers)
	}
	if err := check(); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return InstallerEvidence{}, err
		}
		return evidence, syncDirectory(root)
	}
	temp := ".installers-" + rand.Text()
	if err := root.Mkdir(temp, 0700); err != nil {
		return InstallerEvidence{}, ErrUnavailable
	}
	defer root.RemoveAll(temp)
	dest, err := root.OpenRoot(temp)
	if err != nil {
		return InstallerEvidence{}, ErrUnavailable
	}
	defer dest.Close()
	for _, pkg := range evidence.Installers {
		if err := copyVerifiedFile(ctx, source, dest, pkg.File, pkg); err != nil {
			return InstallerEvidence{}, err
		}
	}
	for file, contents := range map[string][]byte{"installer-verification.json": raw, "installer-verification.sig": sig} {
		f, err := dest.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
		if err != nil {
			return InstallerEvidence{}, ErrUnavailable
		}
		_, writeErr := f.Write(contents)
		if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
			return InstallerEvidence{}, ErrUnavailable
		}
	}
	if err := syncDirectory(dest); err != nil {
		return InstallerEvidence{}, err
	}
	if err := root.Rename(temp, name); err != nil {
		if err := check(); err != nil {
			return InstallerEvidence{}, err
		}
	}
	return evidence, syncDirectory(root)
}

func (s *Releases) OpenInstaller(ctx context.Context, id, arch string, pkg Package) (*os.File, error) {
	if !ValidDigest(id) || !validInstaller(arch, pkg) {
		return nil, ErrPackage
	}
	root, err := os.OpenRoot(s.directory)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer root.Close()
	snapshot, err := root.OpenRoot("installers-" + id)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer snapshot.Close()
	return verifiedArchive(ctx, snapshot, pkg)
}
