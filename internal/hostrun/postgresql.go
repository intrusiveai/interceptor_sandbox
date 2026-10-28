package hostrun

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

// Database oracle SQL and management configuration must remain inaccessible to
// the application even when the author's checkout has world-readable modes.
// Copy only sealed assets, preserve their digest-covered modes, and protect the
// mounted directory itself with the existing supervisor identity.
func privateDatabaseEnvironment(source, sessionRoot string, b *environment.Blueprint) (string, error) {
	needed := false
	for _, service := range b.Services {
		needed = needed || service.Kind == "postgresql"
	}
	if !needed {
		return source, nil
	}
	lock, _, err := environment.Verify(source)
	if err != nil {
		return "", err
	}
	destination := filepath.Join(sessionRoot, "database-environment")
	if err = os.Mkdir(destination, 0700); err != nil {
		return "", err
	}
	files := append([]environment.Asset{{Path: environment.BlueprintFile, Type: "file", Mode: 0600}, {Path: environment.LockFile, Type: "file", Mode: 0600}}, lock.Assets...)
	for _, asset := range files {
		path := filepath.Join(destination, filepath.FromSlash(asset.Path))
		if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return "", err
		}
		switch asset.Type {
		case "file":
			content, x := os.ReadFile(filepath.Join(source, filepath.FromSlash(asset.Path)))
			if x != nil {
				return "", x
			}
			if err = os.WriteFile(path, content, os.FileMode(asset.Mode)); err != nil {
				return "", err
			}
			if err = os.Chmod(path, os.FileMode(asset.Mode)); err != nil {
				return "", err
			}
		case "directory":
			if err = os.MkdirAll(path, os.FileMode(asset.Mode)); err != nil {
				return "", err
			}
			if err = os.Chmod(path, os.FileMode(asset.Mode)); err != nil {
				return "", err
			}
		case "symlink":
			if err = os.Symlink(filepath.FromSlash(asset.Target), path); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("unsupported sealed asset type %s", asset.Type)
		}
	}
	copied, _, err := environment.Verify(destination)
	if err != nil {
		return "", err
	}
	if copied.EnvironmentDigest != lock.EnvironmentDigest {
		return "", fmt.Errorf("private database environment differs from sealed source")
	}
	return destination, nil
}
