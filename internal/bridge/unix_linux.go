package bridge

import (
	"fmt"
	"os"
	"path/filepath"
)

func unixAddress(path string) (string, func(), error) {
	const maximum = 107 // sockaddr_un.sun_path includes a terminating NUL.
	if len(path) <= maximum {
		return path, func() {}, nil
	}
	// /proc/self/fd resolves the opened parent directory without changing the
	// process working directory or moving sockets outside the private session.
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return "", nil, fmt.Errorf("open Unix socket directory: %w", err)
	}
	address := fmt.Sprintf("/proc/self/fd/%d/%s", directory.Fd(), filepath.Base(path))
	if len(address) > maximum {
		directory.Close()
		return "", nil, fmt.Errorf("Unix socket filename is too long: %s; shorten the filename or use --bridge-transport spool", path)
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/self/fd/%d", directory.Fd())); err != nil {
		directory.Close()
		return "", nil, fmt.Errorf("long Unix socket paths require /proc/self/fd; use --bridge-transport spool: %w", err)
	}
	return address, func() { _ = directory.Close() }, nil
}
