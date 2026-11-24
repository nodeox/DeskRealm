package windowsdelivery

import (
	"archive/zip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path"
)

func (v *Verifier) Verify(ctx context.Context, id string) (Manifest, error) {
	root, err := v.candidate(id)
	if err != nil {
		return Manifest{}, err
	}
	defer root.Close()
	raw, manifest, err := v.readManifest(root, id)
	if err != nil {
		return Manifest{}, err
	}
	signature, err := readSmall(root, "release.sig", ed25519.SignatureSize)
	if err != nil {
		if err == ErrManifest {
			return Manifest{}, ErrSignature
		}
		return Manifest{}, err
	}
	if !ed25519.Verify(v.key, append([]byte(signingPrefix), raw...), signature) {
		return Manifest{}, ErrSignature
	}
	for _, arch := range []string{"arm64", "x64"} {
		if err := checkArchive(ctx, root, arch, manifest); err != nil {
			return Manifest{}, err
		}
	}
	return manifest, nil
}

type contextReader struct {
	ctx context.Context
	src io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.src.Read(p)
}

func checkDigest(ctx context.Context, reader io.Reader, size int64, expected string) error {
	digest := sha256.New()
	read, err := io.Copy(digest, io.LimitReader(contextReader{ctx, reader}, size+1))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || read != size || hex.EncodeToString(digest.Sum(nil)) != expected {
		return ErrPackage
	}
	return nil
}

func checkArchive(ctx context.Context, root *os.Root, arch string, manifest Manifest) error {
	record := manifest.Packages[arch]
	file, size, err := regular(root, path.Join("bundle", record.File))
	if err != nil {
		return err
	}
	defer file.Close()
	if size != record.Size {
		return ErrPackage
	}
	if err := checkDigest(ctx, file, size, record.SHA); err != nil {
		return err
	}
	archive, err := zip.NewReader(file, size)
	if err != nil || len(archive.File) != 3 {
		return ErrPackage
	}
	approval := manifest.Approval
	expected := []struct {
		name string
		max  uint64
		sha  string
	}{
		{"client.zip", maxPackage, approval.Builds[arch].ClientSHA},
		{"deskrealm-public.json", maxManifest, ""},
		{"deskrealm-brand.ico", 1 << 20, approval.Brand.IconSHA},
	}
	for i, entry := range archive.File {
		want := expected[i]
		if entry.Name != want.name || !entry.Mode().IsRegular() || entry.Method != zip.Store ||
			entry.Flags&1 != 0 || entry.UncompressedSize64 == 0 || entry.UncompressedSize64 > want.max {
			return ErrPackage
		}
		if err := checkMember(ctx, entry, approval, want.sha); err != nil {
			return err
		}
	}
	return nil
}

func checkMember(ctx context.Context, entry *zip.File, approval Approval, sha string) error {
	member, err := entry.Open()
	if err != nil {
		return ErrPackage
	}
	defer member.Close()
	if sha != "" {
		return checkDigest(ctx, member, int64(entry.UncompressedSize64), sha)
	}
	var profile struct {
		Schema  int    `json:"schema"`
		Version string `json:"version"`
		Brand   Brand  `json:"brand"`
		Center  Center `json:"center"`
	}
	raw, err := io.ReadAll(io.LimitReader(contextReader{ctx, member}, maxManifest+1))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || len(raw) > maxManifest || strictJSON(raw, &profile) != nil ||
		profile.Schema != 1 || profile.Version != approval.Version || profile.Brand != approval.Brand ||
		!profile.Center.Matches(approval.Center) {
		return ErrPackage
	}
	return nil
}
