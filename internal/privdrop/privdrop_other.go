//go:build !linux

// Package privdrop gives /data's link folders to an unprivileged user and
// switches the process to it. Outside Linux it only refuses to run as root.
package privdrop

import (
	"errors"
	"os"
)

// Drop does nothing for a non root process and refuses root.
func Drop(dataDir string, uid, gid int) (bool, error) {
	if os.Geteuid() == 0 {
		return false, errors.New("running as root is only supported on Linux")
	}
	return false, nil
}
