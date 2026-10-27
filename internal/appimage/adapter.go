package appimage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

// stageAdapter copies into a private build context, then verifies the copy rather
// than trusting a source tree that may have changed since environment.Verify.
func stageAdapter(root, contextDir, source string, sealed []environment.Asset) error {
	assets, err := environment.AdapterFiles(root, source)
	if err != nil {
		return err
	}
	stage := filepath.Join(contextDir, "adapter-source")
	for _, asset := range assets {
		out := filepath.Join(stage, filepath.FromSlash(asset.Path))
		switch asset.Type {
		case "directory":
			if err := os.MkdirAll(out, 0755); err != nil {
				return err
			}
		case "file":
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(asset.Path)))
			if err != nil {
				return err
			}
			if err := os.WriteFile(out, data, 0600); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported adapter asset type %q", asset.Type)
		}
	}
	// Restore sealed modes before collection; image permissions are normalized
	// only after the copy has passed the existing asset comparison.
	for i := len(assets) - 1; i >= 0; i-- {
		if err := os.Chmod(filepath.Join(stage, filepath.FromSlash(assets[i].Path)), os.FileMode(assets[i].Mode)); err != nil {
			return err
		}
	}
	copied, err := environment.AdapterFiles(stage, source)
	if err != nil {
		return err
	}
	prefix := filepath.ToSlash(filepath.Clean(source))
	var expected []environment.Asset
	for _, asset := range sealed {
		if asset.Path == prefix || strings.HasPrefix(asset.Path, prefix+"/") {
			expected = append(expected, asset)
		}
	}
	if len(copied) != len(expected) {
		return fmt.Errorf("adapter changed since sealing; reseal and rebuild")
	}
	for i := range copied {
		if copied[i] != expected[i] {
			return fmt.Errorf("adapter %s changed since sealing; reseal and rebuild", copied[i].Path)
		}
	}
	// Parent directories must remain traversable after application privilege drop.
	for _, asset := range copied {
		mode := os.FileMode(0444)
		if asset.Type == "directory" {
			mode = 0755
		}
		if err := os.Chmod(filepath.Join(stage, filepath.FromSlash(asset.Path)), mode); err != nil {
			return err
		}
	}
	return os.Rename(filepath.Join(stage, filepath.FromSlash(source)), filepath.Join(contextDir, "adapter"))
}

// CheckAdapterDestination runs inside the image during construction. It executes
// no target code and refuses links or an existing destination before COPY.
func CheckAdapterDestination(destination string) error {
	if !filepath.IsAbs(destination) || filepath.Base(destination) != environment.AdapterDirectory {
		return fmt.Errorf("invalid adapter destination %q", destination)
	}
	for current := filepath.Clean(destination); current != "/"; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if current == destination {
			return fmt.Errorf("adapter destination already exists: %s; choose another application workdir", destination)
		}
		if !info.IsDir() {
			return fmt.Errorf("adapter destination parent must be a directory, not a link: %s", current)
		}
	}
	return nil
}
