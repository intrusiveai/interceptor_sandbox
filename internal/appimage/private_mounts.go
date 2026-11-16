package appimage

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

func NeedsPrivateMounts(b *environment.Blueprint) bool {
	if len(b.Network.HTTPSSeams) > 0 {
		return true
	}
	for _, service := range b.Services {
		if service.Kind == "postgresql" {
			return true
		}
	}
	return false
}

// EnsurePrivateMounts enforces isolation using the native Linux filesystem,
// even where Docker Desktop projects host bind mounts as the accessing UID.
func EnsurePrivateMounts(value string) error {
	uid, err := strconv.Atoi(value)
	if err != nil || uid <= 0 || uid == 20000 {
		return fmt.Errorf("invalid private-mount supervisor UID")
	}
	// macOS bind mounts can project the accessing UID as their owner. A native
	// Linux ancestor, outside those mounts, enforces the supervisor boundary.
	base := "/run/interceptor-private"
	if err := os.Mkdir(base, 0700); err != nil {
		return fmt.Errorf("create private supervisor mount ancestor: %w", err)
	}
	if err := os.Chown(base, uid, uid); err != nil {
		return err
	}
	for _, path := range []string{"/run/interceptor-session", "/run/interceptor-blobs", "/run/interceptor-tls", "/opt/interceptor/environment"} {
		if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
			return fmt.Errorf("wrapper private path already exists: %s", path)
		}
		destination := filepath.Join(base, filepath.Base(path))
		if err := os.Mkdir(destination, 0755); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.Symlink(destination, path); err != nil {
			return err
		}
	}
	return nil
}
