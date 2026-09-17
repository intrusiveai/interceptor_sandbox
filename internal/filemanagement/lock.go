// Package filemanagement owns host spool cleanup and administrative exclusion.
package filemanagement

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock is process-scoped by the OS, so crashes release it without stale PID files.
// Lock files live outside guest mounts and are never unlinked during normal use.
func Lock(root, name string, exclusive bool) (*os.File, error) {
	directory := filepath.Join(root, "locks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(directory, name+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("file management busy (%s); stop the owning service or wait for its operation to finish: %w", name, err)
	}
	return f, nil
}
