package appimage

import (
	"bytes"
	_ "embed"
	"fmt"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/postgresql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed postgresql.Dockerfile
var postgresqlBuild string

//go:embed postgresql-go.LICENSE
var postgresClientLicense []byte

//go:embed postgresql-GPL3.LICENSE
var gccGPL []byte

//go:embed postgresql-GCC-runtime.LICENSE
var gccRuntime []byte

//go:embed postgresql-musl.LICENSE
var muslLicense []byte

func databaseBuild(b *environment.Blueprint, directory string) (string, string, error) {
	for _, s := range b.Services {
		if s.Kind == "postgresql" {
			for name, content := range map[string][]byte{"musl.LICENSE": muslLicense, "GPL3.LICENSE": gccGPL, "GCC-runtime.LICENSE": gccRuntime} {
				if err := os.WriteFile(filepath.Join(directory, name), content, 0644); err != nil {
					return "", "", err
				}
			}
			return postgresqlBuild + "\n", fmt.Sprintf("COPY --from=interceptor-postgresql %s %s\n", postgresql.Prefix, postgresql.Prefix), nil
		}
	}
	return "", "", nil
}
func databaseVersions(b *environment.Blueprint) string {
	for _, s := range b.Services {
		if s.Kind == "postgresql" {
			return strings.Join([]string{postgresql.Version, postgresql.VectorVersion}, "/")
		}
	}
	return ""
}

// EnsurePostgreSQLUser gives initdb a name for the existing supervisor UID.
// PostgreSQL requires getpwuid to succeed even with an explicit database role.
// Existing image accounts are preserved; this does not change the app identity.
func EnsurePostgreSQLUser(value string) error {
	if err := ensurePostgreSQLAccount(value); err != nil {
		return err
	}
	uid, _ := strconv.Atoi(value)
	// macOS bind mounts can project the accessing UID as their owner. A native
	// Linux ancestor, outside those mounts, enforces the supervisor boundary.
	base := "/run/interceptor-private"
	if err := os.Mkdir(base, 0700); err != nil {
		return fmt.Errorf("create private database mount ancestor: %w", err)
	}
	if err := os.Chown(base, uid, uid); err != nil {
		return err
	}
	for _, path := range []string{"/run/interceptor-session", "/run/interceptor-blobs", "/run/interceptor-tls", "/opt/interceptor/environment"} {
		if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
			return fmt.Errorf("database wrapper private path already exists: %s", path)
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
func ensurePostgreSQLAccount(value string) error {
	uid, err := strconv.Atoi(value)
	if err != nil || uid <= 0 || uid == 20000 {
		return fmt.Errorf("invalid PostgreSQL supervisor UID")
	}
	data, err := os.ReadFile("/etc/passwd")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Split(line, ":")
		if len(parts) > 2 && parts[2] == value {
			return nil
		}
	}
	if err = os.MkdirAll("/etc", 0755); err != nil {
		return err
	}
	name := "interceptor_pg_" + value
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, name+":") {
			return fmt.Errorf("PostgreSQL supervisor account name already exists")
		}
	}
	data = append(bytes.TrimRight(data, "\n"), []byte(fmt.Sprintf("\n%s:x:%d:%d:Interceptor database:/nonexistent:/bin/false\n", name, uid, uid))...)
	return os.WriteFile("/etc/passwd", data, 0644)
}
