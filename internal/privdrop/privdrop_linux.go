//go:build linux

// Package privdrop gives /data's link folders to an unprivileged user and
// switches the process to it.
package privdrop

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/fsutil"
)

// Drop runs as root only. It creates dataDir/{identity,secrets,state} with
// mode 0700, chowns them and everything inside to uid:gid, then drops all
// groups and switches to gid and uid. If the process is not root (a dev
// run) it does nothing.
func Drop(dataDir string, uid, gid int) (bool, error) {
	if os.Geteuid() != 0 {
		return false, nil
	}
	// The unprivileged user must be able to pass through dataDir (it does
	// not need to list or write it). Supervisor creates /data owned by root.
	if err := os.MkdirAll(dataDir, 0o711); err != nil {
		return false, err
	}
	st, err := os.Stat(dataDir)
	if err != nil {
		return false, err
	}
	if st.Mode().Perm()&0o001 == 0 {
		if err := os.Chmod(dataDir, st.Mode().Perm()|0o711); err != nil {
			return false, fmt.Errorf("making %s passable: %w", dataDir, err)
		}
	}
	for _, d := range []string{"identity", "secrets", "state"} {
		dir := filepath.Join(dataDir, d)
		if err := fsutil.EnsureDir(dir); err != nil {
			return false, err
		}
		err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.Type()&fs.ModeSymlink != 0 {
				return os.Lchown(p, uid, gid)
			}
			return os.Chown(p, uid, gid)
		})
		if err != nil {
			return false, fmt.Errorf("chown %s: %w", d, err)
		}
	}
	if err := syscall.Setgroups([]int{}); err != nil {
		return false, fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return false, fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return false, fmt.Errorf("setuid: %w", err)
	}
	if os.Getuid() != uid || os.Geteuid() != uid || os.Getgid() != gid || os.Getegid() != gid {
		return false, errors.New("privilege drop did not take effect")
	}
	if syscall.Setuid(0) == nil {
		return false, errors.New("privilege drop is reversible")
	}
	return true, nil
}
