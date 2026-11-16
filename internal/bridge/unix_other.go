//go:build !linux

package bridge

import "fmt"

func unixAddress(path string) (string, func(), error) {
	// Darwin's sockaddr_un has 104 bytes including the terminating NUL.
	if len(path) > 103 {
		return "", nil, fmt.Errorf("Unix socket path is too long: %s; shorten INTERCEPTOR_DATA_DIR or use --bridge-transport spool", path)
	}
	return path, func() {}, nil
}
