package install

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type recoveryStage struct {
	work, dest string
	parentLock *os.File
}

func newRecoveryStage(dest string) (*recoveryStage, error) {
	parent := filepath.Dir(dest)
	lock, err := os.Open(parent)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		lock.Close()
		return nil, errors.New("destination already exists or cannot be inspected")
	}
	work, err := os.MkdirTemp(parent, ".deskrealm-recovery-")
	if err != nil {
		lock.Close()
		return nil, err
	}
	stage := &recoveryStage{work: work, dest: dest, parentLock: lock}
	for _, dir := range []string{"secrets", "rendezvous"} {
		if err := os.Mkdir(filepath.Join(work, dir), 0700); err != nil {
			stage.Close()
			return nil, err
		}
	}
	return stage, nil
}

func (p *recoveryStage) Close() {
	_ = os.RemoveAll(p.work)
	_ = p.parentLock.Close()
}

func (p *recoveryStage) Publish() error {
	if err := syscall.Flock(int(p.parentLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another backup or restore is publishing to this destination parent")
	}
	defer syscall.Flock(int(p.parentLock.Fd()), syscall.LOCK_UN)
	for _, subdir := range []string{"secrets", "rendezvous", "."} {
		if err := syncDirectory(filepath.Join(p.work, subdir)); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(p.dest); !errors.Is(err, os.ErrNotExist) {
		return errors.New("destination appeared during backup or restore")
	}
	if err := os.Rename(p.work, p.dest); err != nil {
		return err
	}
	return p.parentLock.Sync()
}

type recoveryReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r recoveryReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// verifyBackupArchive consumes all bytes, even beyond the TAR end marker, so
// the recorded SHA-256 describes the exact file delivered by pg_basebackup.
func verifyBackupArchive(ctx context.Context, file, output string, expected *backupFile) (backupFile, error) {
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() <= 0 ||
		(expected != nil && info.Size() != expected.Size) {
		return backupFile{}, errors.New("PostgreSQL backup archive is missing, exposed, or the wrong size")
	}
	in, err := os.Open(file)
	if err != nil {
		return backupFile{}, err
	}
	defer in.Close()
	hash := sha256.New()
	bounded := &io.LimitedReader{R: recoveryReader{ctx, in}, N: info.Size() + 1}
	stream := io.TeeReader(bounded, hash)
	var root *os.Root
	if output != "" {
		root, err = os.OpenRoot(output)
		if err != nil {
			return backupFile{}, err
		}
		defer root.Close()
	}
	if err := unpackPostgresTar(tar.NewReader(stream), root); err != nil {
		return backupFile{}, err
	}
	if _, err := io.Copy(io.Discard, stream); err != nil || bounded.N != 1 {
		return backupFile{}, errors.New("PostgreSQL backup archive changed while reading")
	}
	record := backupFile{Size: info.Size(), SHA: hex.EncodeToString(hash.Sum(nil))}
	if expected != nil && *expected != record {
		return backupFile{}, errors.New("PostgreSQL backup archive digest differs")
	}
	return record, nil
}

func unpackPostgresTar(archive *tar.Reader, root *os.Root) error {
	seen := make(map[string]byte)
	for {
		entry, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.New("invalid PostgreSQL backup archive")
		}
		name := strings.TrimPrefix(strings.TrimSuffix(entry.Name, "/"), "./")
		if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\\x00") ||
			(entry.Typeflag != tar.TypeDir && entry.Typeflag != tar.TypeReg) {
			return fmt.Errorf("PostgreSQL backup contains an unsupported or unsafe entry %q (type %d)", name, entry.Typeflag)
		}
		if previous := seen[name]; previous != 0 {
			// PostgreSQL can include pg_wal directory headers in both the
			// original snapshot and the appended WAL stream.
			if previous == tar.TypeDir && entry.Typeflag == tar.TypeDir {
				continue
			}
			return fmt.Errorf("PostgreSQL backup duplicates file %q", name)
		}
		for _, forbidden := range []string{"standby.signal", "recovery.signal", "postmaster.pid"} {
			if name == forbidden {
				return errors.New("PostgreSQL backup contains unsupported recovery or tablespace settings")
			}
		}
		if name == "tablespace_map" && (entry.Typeflag != tar.TypeReg || entry.Size != 0) {
			return errors.New("PostgreSQL backup uses external tablespaces")
		}
		seen[name] = entry.Typeflag
		if root == nil {
			continue
		}
		if entry.Typeflag == tar.TypeDir {
			if err := root.MkdirAll(name, 0700); err != nil {
				return err
			}
			continue
		}
		if err := root.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return err
		}
		dst, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		n, err := io.Copy(dst, archive)
		if err := errors.Join(err, dst.Close()); err != nil || n != entry.Size {
			return errors.New("could not extract complete PostgreSQL backup")
		}
	}
	for _, name := range []string{"PG_VERSION", "global/pg_control", "backup_label", "backup_manifest"} {
		if seen[name] != tar.TypeReg {
			return errors.New("PostgreSQL backup is missing required cluster files")
		}
	}
	if seen["pg_wal"] != tar.TypeDir {
		return errors.New("PostgreSQL backup is missing its WAL directory")
	}
	return nil
}
