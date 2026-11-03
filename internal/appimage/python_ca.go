package appimage

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

//go:embed python_ca.py
var pythonCAInstaller string

// InstallPythonCA runs only inside the wrapper build. A second, fresh interpreter
// proves that Certifi resolves to the same persistent bundle after installation.
func InstallPythonCA(caPath, mountsJSON string, interpreters []string) error {
	var mounts []string
	if err := json.Unmarshal([]byte(mountsJSON), &mounts); err != nil {
		return fmt.Errorf("invalid Python trust mount list: %w", err)
	}
	for _, interpreter := range interpreters {
		if !filepath.IsAbs(interpreter) {
			return fmt.Errorf("Python trust interpreter must be absolute: %q", interpreter)
		}
		if err := installPythonCA(interpreter, caPath, mountsJSON); err != nil {
			return fmt.Errorf("install Python CA for %s: %w; ensure this image interpreter has a persistent, writable Certifi bundle, then rebuild", interpreter, err)
		}
	}
	return nil
}

func installPythonCA(interpreter, caPath, mountsJSON string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var installed string
	for _, mode := range []string{"install", "verify"} {
		command := exec.CommandContext(ctx, interpreter, "-I", "-B", "-c", pythonCAInstaller, mode, caPath, mountsJSON)
		command.Dir = "/"
		command.WaitDelay = time.Second
		var output, diagnostics caOutput
		command.Stdout, command.Stderr = &output, &diagnostics
		if err := command.Run(); err != nil {
			return fmt.Errorf("%s: %w: %s %s", mode, err, strings.TrimSpace(output.String()), strings.TrimSpace(diagnostics.String()))
		}
		bundle := strings.TrimSpace(output.String())
		if !filepath.IsAbs(bundle) || strings.ContainsAny(bundle, "\r\n") || (mode == "verify" && installed != bundle) {
			return fmt.Errorf("Certifi did not resolve to a stable absolute bundle path: %q", bundle)
		}
		installed = bundle
	}
	return nil
}

// Bound diagnostics from an image-supplied interpreter without blocking it.
type caOutput struct{ bytes.Buffer }

func (b *caOutput) Write(data []byte) (int, error) {
	n := len(data)
	if remaining := 16384 - b.Len(); remaining > 0 {
		b.Buffer.Write(data[:min(len(data), remaining)])
	}
	return n, nil
}

func pythonCAMounts(blueprint *environment.Blueprint, volumes []string) []string {
	roots := []string{"/tmp", "/proc", "/sys", "/dev", "/run", "/opt/interceptor/environment", "/etc/hosts", "/etc/hostname", "/etc/resolv.conf"}
	roots = append(roots, volumes...)
	roots = append(roots, blueprint.Application.Persistence.MutableRoots...)
	for _, seed := range blueprint.Filesystem.Seed {
		roots = append(roots, seed.Target)
	}
	return roots
}
