package windowssetup

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"deskrealm/internal/windowsdelivery"
)

const profileName = "deskrealm-public.json"
const iconName = "deskrealm-brand.ico"

var (
	errArchive = errors.New("invalid DeskRealm installer archive")
	devName    = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[1-9¹²³]|lpt[1-9¹²³])$`)
)

// Install extracts a verified executable's archive into a private per-user installation root.
// The caller verifies Authenticode against the same open file before calling it.
func Install(image *os.File, arch, directory string) (string, error) {
	info, err := image.Stat()
	if err != nil {
		return "", err
	}
	p, err := readPE(image, info.Size())
	if err != nil || p.certSize == 0 || p.machine != machineFor(arch) || machineFor(arch) == 0 {
		return "", errImage
	}
	member, err := readPayload(image, p.certOffset, p)
	if err != nil {
		return "", err
	}
	return installBundle(io.NewSectionReader(image, member.offset, member.size), member.size, member.digest, directory)
}

func installBundle(bundle io.ReaderAt, size int64, digest [32]byte, directory string) (string, error) {
	outer, err := zip.NewReader(bundle, size)
	if err != nil || len(outer.File) != 3 {
		return "", errArchive
	}
	names := []string{"client.zip", profileName, iconName}
	for i, entry := range outer.File {
		if entry.Name != names[i] || !entry.Mode().IsRegular() || entry.Method != zip.Store ||
			entry.Flags&1 != 0 || entry.UncompressedSize64 == 0 || entry.UncompressedSize64 > uint64(size) {
			return "", errArchive
		}
	}
	profile, err := readEntry(outer.File[1], 64<<10)
	if err != nil {
		return "", err
	}
	if _, err := windowsdelivery.ParsePublicProfile(profile); err != nil {
		return "", errArchive
	}
	if outer.File[2].UncompressedSize64 > 1<<20 {
		return "", errArchive
	}
	client := outer.File[0]
	offset, err := client.DataOffset()
	if err != nil || offset < 0 || offset > size || client.UncompressedSize64 > uint64(size-offset) ||
		client.CompressedSize64 != client.UncompressedSize64 {
		return "", errArchive
	}
	files, err := zip.NewReader(io.NewSectionReader(bundle, offset, int64(client.UncompressedSize64)), int64(client.UncompressedSize64))
	if err != nil || checkEntries(files.File) != nil {
		return "", errArchive
	}
	entries := append(files.File, outer.File[1:]...)
	return installTree(directory, hex.EncodeToString(digest[:]), entries)
}

func readEntry(entry *zip.File, limit uint64) ([]byte, error) {
	if entry.UncompressedSize64 > limit {
		return nil, errArchive
	}
	data, err := entry.Open()
	if err != nil {
		return nil, errArchive
	}
	defer data.Close()
	return io.ReadAll(io.LimitReader(data, int64(limit)+1))
}

func checkEntries(entries []*zip.File) error {
	if len(entries) == 0 || len(entries) > 10_000 {
		return errArchive
	}
	names := map[string]string{}
	isFile := map[string]bool{}
	explicit := map[string]bool{}
	var unpacked uint64
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name, "/")
		for _, component := range strings.Split(name, "/") {
			if component == "" || component == "." || component == ".." || strings.HasSuffix(component, ".") ||
				strings.HasSuffix(component, " ") || strings.ContainsAny(component, "\\:<>\"|?*") ||
				strings.ContainsFunc(component, func(r rune) bool { return r < 32 }) ||
				devName.MatchString(strings.TrimRight(strings.SplitN(component, ".", 2)[0], " ")) {
				return errArchive
			}
		}
		folded := strings.ToLower(name)
		if explicit[folded] || (entry.Mode().IsRegular() && names[folded] != "") ||
			folded == profileName || folded == iconName ||
			(!entry.Mode().IsDir() && !entry.Mode().IsRegular()) || entry.Flags&1 != 0 ||
			entry.UncompressedSize64 > 3*uint64(maxPayload)-unpacked {
			return errArchive
		}
		unpacked += entry.UncompressedSize64
		explicit[folded] = true
		for prefix := name; prefix != "."; prefix = path.Dir(prefix) {
			key := strings.ToLower(prefix)
			if prior, exists := names[key]; exists && (prior != prefix || isFile[key]) {
				return errArchive
			}
			names[key] = prefix
		}
		if entry.Mode().IsRegular() {
			isFile[folded] = true
		}
	}
	for _, required := range []string{"DeskRealm.exe", "rustdesk.exe", "librustdesk.dll"} {
		key := strings.ToLower(required)
		if !isFile[key] || names[key] != required {
			return errArchive
		}
	}
	return nil
}

func installTree(directory, digest string, entries []*zip.File) (string, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	dest := filepath.Join(directory, digest)
	if info, err := os.Lstat(dest); err == nil {
		if !info.IsDir() {
			return "", errArchive
		}
		return dest, validateTree(dest, entries)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	stage, err := os.MkdirTemp(directory, ".deskrealm-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	root, err := os.OpenRoot(stage)
	if err != nil {
		return "", err
	}
	err = extractTree(root, entries)
	closeErr := root.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := os.Rename(stage, dest); err != nil {
		fi, statErr := os.Lstat(dest)
		if statErr != nil || !fi.IsDir() {
			return "", err
		}
		return dest, validateTree(dest, entries)
	}
	return dest, nil
}

func extractTree(root *os.Root, entries []*zip.File) error {
	for _, entry := range entries {
		name := filepath.FromSlash(strings.TrimSuffix(entry.Name, "/"))
		if entry.Mode().IsDir() {
			if err := root.MkdirAll(name, 0700); err != nil {
				return err
			}
			continue
		}
		if err := root.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return err
		}
		out, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		copyErr := copyEntry(out, entry)
		closeErr := out.Close()
		if copyErr != nil || closeErr != nil {
			return errors.Join(copyErr, closeErr)
		}
	}
	return nil
}

func copyEntry(out io.Writer, entry *zip.File) error {
	in, err := entry.Open()
	if err != nil {
		return errArchive
	}
	defer in.Close()
	if n, err := io.Copy(out, in); err != nil || uint64(n) != entry.UncompressedSize64 {
		return fmt.Errorf("unable to unpack %q: %w", entry.Name, errArchive)
	}
	return nil
}

func validateTree(directory string, entries []*zip.File) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	expected := make(map[string]bool)
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name, "/")
		for prefix := name; prefix != "."; prefix = path.Dir(prefix) {
			expected[prefix] = true
		}
		info, err := root.Lstat(name)
		if err != nil {
			return errArchive
		}
		if entry.Mode().IsDir() {
			if !info.IsDir() {
				return errArchive
			}
			continue
		}
		if !info.Mode().IsRegular() || uint64(info.Size()) != entry.UncompressedSize64 {
			return errArchive
		}
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		actual, want := sha256.New(), sha256.New()
		n, actualErr := io.Copy(actual, io.LimitReader(file, info.Size()+1))
		closeErr := file.Close()
		if actualErr != nil || closeErr != nil || n != info.Size() || copyEntry(want, entry) != nil ||
			!bytes.Equal(want.Sum(nil), actual.Sum(nil)) {
			return errArchive
		}
	}
	return filepath.WalkDir(directory, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, name)
		if err != nil || (relative != "." && !expected[filepath.ToSlash(relative)]) ||
			(!entry.IsDir() && !entry.Type().IsRegular()) {
			return errArchive
		}
		return nil
	})
}
