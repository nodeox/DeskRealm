package windowsdelivery

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type Releases struct {
	directory string
}

func (v *Verifier) ConfigureReleases(directory string) (*Releases, error) {
	if directory == "" {
		return nil, nil
	}
	if v == nil || !filepath.IsAbs(directory) {
		return nil, errors.New("Windows releases require an absolute release directory and a configured verifier")
	}
	store, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, errors.New("Windows release directory unavailable")
	}
	stage, err := filepath.EvalSymlinks(v.directory)
	if err != nil {
		return nil, errors.New("Windows staging directory unavailable")
	}
	if !outside(store, stage) || !outside(stage, store) {
		return nil, errors.New("Windows release and candidate directories must be separate")
	}
	info, err := os.Stat(store)
	if err != nil || !info.IsDir() {
		return nil, errors.New("Windows release directory unavailable")
	}
	return &Releases{directory: store}, nil
}

func outside(parent, other string) bool {
	relative, err := filepath.Rel(parent, other)
	return err == nil && (relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}

// Preserve copies exactly the bytes from the verified manifest into a separate,
// center-managed store. It exposes a new snapshot only after both archives pass.
func (s *Releases) Preserve(ctx context.Context, v *Verifier, id string) (Manifest, error) {
	manifest, err := v.Verify(ctx, id)
	if err != nil {
		return Manifest{}, err
	}
	root, err := os.OpenRoot(s.directory)
	if err != nil {
		return Manifest{}, ErrUnavailable
	}
	defer root.Close()
	existing, err := root.OpenRoot(id)
	if err == nil {
		defer existing.Close()
		if err := checkSnapshot(ctx, existing, manifest); err != nil {
			return Manifest{}, err
		}
		return manifest, syncDirectory(root)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, ErrUnavailable
	}
	source, err := v.candidate(id)
	if err != nil {
		return Manifest{}, err
	}
	defer source.Close()
	temp := ".candidate-" + rand.Text()
	if err := root.Mkdir(temp, 0700); err != nil {
		return Manifest{}, ErrUnavailable
	}
	defer root.RemoveAll(temp)
	pending, err := root.OpenRoot(temp)
	if err != nil {
		return Manifest{}, ErrUnavailable
	}
	defer pending.Close()
	for _, pkg := range manifest.Packages {
		if err := copyArchive(ctx, source, pending, pkg); err != nil {
			return Manifest{}, err
		}
	}
	if err := syncDirectory(pending); err != nil {
		return Manifest{}, err
	}
	if err := root.Rename(temp, id); err != nil {
		// Another publisher can win the rename. Its complete snapshot must
		// match the exact same signed digests.
		other, openErr := root.OpenRoot(id)
		if openErr != nil {
			return Manifest{}, ErrUnavailable
		}
		defer other.Close()
		if err := checkSnapshot(ctx, other, manifest); err != nil {
			return Manifest{}, err
		}
	}
	return manifest, syncDirectory(root)
}

func syncDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return ErrUnavailable
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return ErrUnavailable
	}
	return nil
}

func copyArchive(ctx context.Context, source, target *os.Root, pkg Package) error {
	return copyVerifiedFile(ctx, source, target, path.Join("bundle", pkg.File), pkg)
}

func copyVerifiedFile(ctx context.Context, source, target *os.Root, sourceName string, pkg Package) error {
	from, size, err := regular(source, sourceName)
	if err != nil {
		return err
	}
	defer from.Close()
	if size != pkg.Size {
		return ErrPackage
	}
	to, err := target.OpenFile(pkg.File, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return ErrUnavailable
	}
	hashErr := checkDigest(ctx, io.TeeReader(from, to), pkg.Size, pkg.SHA)
	syncErr := to.Sync()
	closeErr := to.Close()
	if hashErr != nil {
		return hashErr
	}
	if syncErr != nil || closeErr != nil {
		return ErrUnavailable
	}
	return nil
}

func checkSnapshot(ctx context.Context, root *os.Root, manifest Manifest) error {
	return checkPackages(ctx, root, manifest.Packages)
}

func checkPackages(ctx context.Context, root *os.Root, packages map[string]Package) error {
	for _, pkg := range packages {
		file, err := verifiedArchive(ctx, root, pkg)
		if err != nil {
			return err
		}
		file.Close()
	}
	return nil
}

func (s *Releases) Open(ctx context.Context, id, arch string, pkg Package) (*os.File, error) {
	if !ValidDigest(id) || (arch != "x64" && arch != "arm64") || pkg.File != "DeskRealm-"+arch+".zip" ||
		!ValidDigest(pkg.SHA) || pkg.Size < 1 || pkg.Size > maxPackage {
		return nil, ErrPackage
	}
	root, err := os.OpenRoot(s.directory)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer root.Close()
	release, err := root.OpenRoot(id)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer release.Close()
	return verifiedArchive(ctx, release, pkg)
}

func verifiedArchive(ctx context.Context, root *os.Root, pkg Package) (*os.File, error) {
	file, size, err := regular(root, pkg.File)
	if err != nil {
		return nil, err
	}
	if size != pkg.Size {
		file.Close()
		return nil, ErrPackage
	}
	if err := checkDigest(ctx, file, pkg.Size, pkg.SHA); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, ErrUnavailable
	}
	return file, nil
}
