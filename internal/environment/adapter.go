package environment

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const AdapterDirectory = ".interceptor-adapter"

func AdapterPath(workdir string) string { return path.Join(workdir, AdapterDirectory) }

// AdapterFiles validates the source tree without following links. Returned records
// use the same digest format as every other sealed Blueprint asset.
func AdapterFiles(root, source string) ([]Asset, error) {
	clean := filepath.Clean(source)
	if source == "" || filepath.IsAbs(source) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("adapter_asset must name a directory within the Blueprint")
	}
	current := root
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, fmt.Errorf("adapter_asset: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("adapter_asset path must contain only directories, not links: %s", current)
		}
	}
	err := filepath.WalkDir(current, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("adapter_asset does not support symlinks: %s", p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return collectAssets(root, []string{source})
}

func validateAdapter(root string, blueprint *Blueprint) error {
	code := blueprint.Application.Interface.Code
	if _, err := AdapterFiles(root, code.AdapterAsset); err != nil {
		return err
	}
	bundle := filepath.Join(root, code.AdapterAsset)
	var entry string
	switch code.Language {
	case "python":
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`).MatchString(code.Module) {
			return fmt.Errorf("adapter Python module must be a dotted module name")
		}
		parts := strings.Split(code.Module, ".")
		for i := 1; i < len(parts); i++ {
			if info, err := os.Stat(filepath.Join(bundle, filepath.Join(parts[:i]...), "__init__.py")); err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("adapter Python packages require __init__.py")
			}
		}
		entry = filepath.Join(bundle, filepath.Join(parts...)+".py")
		if _, err := os.Stat(entry); os.IsNotExist(err) {
			entry = filepath.Join(bundle, filepath.Join(parts...), "__init__.py")
		}
	case "node":
		clean := path.Clean(strings.TrimPrefix(code.Module, "./"))
		if !strings.HasPrefix(code.Module, "./") || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(code.Module, "\\?#\x00\r\n") || path.IsAbs(clean) {
			return fmt.Errorf("adapter Node module must be an explicit ./entry.js, .mjs or .cjs path inside adapter_asset")
		}
		ext := path.Ext(clean)
		if ext != ".js" && ext != ".mjs" && ext != ".cjs" {
			return fmt.Errorf("unsupported Node adapter entry extension %q", ext)
		}
		entry = filepath.Join(bundle, filepath.FromSlash(clean))
	default:
		return fmt.Errorf("adapter language must be python or node")
	}
	if info, err := os.Stat(entry); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("adapter entry %q must exist as a regular file", code.Module)
	}
	destination := AdapterPath(blueprint.Application.Launch.Workdir)
	// These roots are supplied by the container runtime, not the application image.
	roots := []string{"/tmp", "/proc", "/sys", "/dev", "/run", "/opt/interceptor"}
	roots = append(roots, blueprint.Application.Persistence.MutableRoots...)
	for _, seed := range blueprint.Filesystem.Seed {
		roots = append(roots, seed.Target)
	}
	for _, root := range roots {
		if pathWithin(destination, root) || pathWithin(root, destination) {
			return fmt.Errorf("adapter destination %s conflicts with runtime or filesystem root %s; choose a dedicated application workdir", destination, root)
		}
	}
	return nil
}
