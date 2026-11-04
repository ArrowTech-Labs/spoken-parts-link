// Package fsutil has small file helpers: atomic private writes and wipes.
package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// PrivateFile is the mode for every file the link writes under /data.
const PrivateFile fs.FileMode = 0o600

// PrivateDir is the mode for every directory the link creates under /data.
const PrivateDir fs.FileMode = 0o700

// EnsureDir creates dir (and parents) with mode 0700 and tightens the mode if
// the directory already exists.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, PrivateDir); err != nil {
		return err
	}
	return os.Chmod(dir, PrivateDir)
}

// WriteFileAtomic writes data to path with mode 0600. It writes a temporary
// file in the same directory, syncs it, then renames it over path, so a crash
// leaves either the old or the new content and never a partial file.
func WriteFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := EnsureDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(PrivateFile); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// Wipe overwrites a file with zeros, syncs it and removes it. A missing file
// is not an error. On flash storage the overwrite is best effort only; the
// real protection is that secrets on disk are encrypted or worthless alone.
func Wipe(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return os.Remove(path)
	}
	if st, serr := f.Stat(); serr == nil && st.Mode().IsRegular() {
		zeros := make([]byte, st.Size())
		_, _ = f.WriteAt(zeros, 0)
		_ = f.Sync()
	}
	_ = f.Close()
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
