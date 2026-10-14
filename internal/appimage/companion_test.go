package appimage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo"
)

func TestPackagedLinuxCompanion(t *testing.T) {
	// Real ELF/Go executables for both supported architectures, never executed
	// cross-architecture. Docker qualification separately exercises the CLI.
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			directory := t.TempDir()
			source := filepath.Join(directory, "main.go")
			if err := os.WriteFile(source, []byte("package main\nfunc main() {}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("go", "build", "-trimpath", "-o", filepath.Join(directory, companionName), source)
			command.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("build companion fixture: %v: %s", err, output)
			}
			companion, err := os.ReadFile(filepath.Join(directory, companionName))
			if err != nil {
				t.Fatal(err)
			}
			host := []byte("host CLI fixture")
			metadata := map[string]any{"version": "0.1.0", "source_commit": strings.Repeat("a", 40), "platform": "darwin/" + arch,
				"binary_sha256": hexDigest(host), "linux_companion": map[string]any{"filename": companionName, "platform": "linux/" + arch, "sha256": hexDigest(companion)}}
			reset := func() {
				t.Helper()
				for name, data := range map[string][]byte{"interceptor": host, companionName: companion} {
					if err := os.WriteFile(filepath.Join(directory, name), data, 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(filepath.Join(directory, name), 0755); err != nil {
						t.Fatal(err)
					}
				}
				writeMetadata(t, directory, metadata)
			}
			reset()
			executable := filepath.Join(directory, "interceptor")
			link := filepath.Join(t.TempDir(), "interceptor")
			if err := os.Symlink(executable, link); err != nil {
				t.Fatal(err)
			}
			t.Chdir(t.TempDir())
			destination := filepath.Join(t.TempDir(), "copied")
			if err := copyLinuxBinary(link, destination, "darwin", arch, "0.1.0", strings.Repeat("a", 40)); err != nil {
				t.Fatal(err)
			}
			copied, _ := os.ReadFile(destination)
			if string(copied) != string(companion) {
				t.Fatal("did not copy the packaged Linux bytes")
			}
			for _, test := range []struct {
				name            string
				change          func()
				version, source string
			}{
				{"missing companion", func() { _ = os.Remove(filepath.Join(directory, companionName)) }, "0.1.0", strings.Repeat("a", 40)},
				{"missing metadata", func() { _ = os.Remove(filepath.Join(directory, "release.json")) }, "0.1.0", strings.Repeat("a", 40)},
				{"invalid metadata", func() { _ = os.WriteFile(filepath.Join(directory, "release.json"), []byte("not JSON"), 0600) }, "0.1.0", strings.Repeat("a", 40)},
				{"changed companion", func() { _ = os.WriteFile(filepath.Join(directory, companionName), []byte("wrong executable"), 0755) }, "0.1.0", strings.Repeat("a", 40)},
				{"changed host", func() { _ = os.WriteFile(executable, []byte("another CLI"), 0755) }, "0.1.0", strings.Repeat("a", 40)},
				{"not executable", func() { _ = os.Chmod(filepath.Join(directory, companionName), 0644) }, "0.1.0", strings.Repeat("a", 40)},
				{"different version", func() {}, "0.2.0", strings.Repeat("a", 40)},
				{"different source", func() {}, "0.1.0", strings.Repeat("b", 40)},
			} {
				t.Run(test.name, func(t *testing.T) {
					reset()
					test.change()
					err := copyLinuxBinary(executable, destination, "darwin", arch, test.version, test.source)
					if err == nil || !strings.Contains(err.Error(), "re-extract the complete") {
						t.Fatalf("expected actionable error, got %v", err)
					}
				})
			}
			reset()
			// Even a rehashed wrong-architecture or non-ELF file is rejected.
			for _, invalid := range [][]byte{[]byte("not ELF"), append([]byte(nil), companion...)} {
				if len(invalid) > 20 {
					invalid[18], invalid[19] = 0, 0
				}
				_ = os.WriteFile(filepath.Join(directory, companionName), invalid, 0755)
				metadata["linux_companion"].(map[string]any)["sha256"] = hexDigest(invalid)
				writeMetadata(t, directory, metadata)
				if _, err := packagedLinuxBinary(executable, arch, "0.1.0", strings.Repeat("a", 40)); err == nil {
					t.Fatal("accepted invalid ELF")
				}
			}
		})
	}
}

func TestLinuxCopiesRunningExecutableWithoutMetadata(t *testing.T) {
	directory := t.TempDir()
	executable, destination := filepath.Join(directory, "interceptor"), filepath.Join(directory, "copy")
	data := []byte("existing Linux CLI")
	if err := os.WriteFile(executable, data, 0755); err != nil {
		t.Fatal(err)
	}
	if err := copyLinuxBinary(executable, destination, "linux", "amd64", "0.1.0", ""); err != nil {
		t.Fatal(err)
	}
	actual, _ := os.ReadFile(destination)
	if string(actual) != string(data) {
		t.Fatal("Linux self-copy changed bytes")
	}
	if err := copyLinuxBinary(executable+"-missing", destination, "linux", "amd64", "0.1.0", ""); err == nil {
		t.Fatal("missing Linux executable accepted")
	}
}

func TestReleasedBuildNeverFallsBackToGo(t *testing.T) {
	oldVersion, oldSource := buildinfo.Version, buildinfo.SourceCommit
	t.Cleanup(func() { buildinfo.Version, buildinfo.SourceCommit = oldVersion, oldSource })
	buildinfo.Version, buildinfo.SourceCommit = "0.1.0", strings.Repeat("a", 40)
	t.Setenv("PATH", t.TempDir())
	// On Darwin the test executable has no release.json and must fail without
	// searching the checkout; on Linux it must keep working without Go.
	err := buildLinuxBinary(context.Background(), filepath.Join(t.TempDir(), "interceptor"))
	if runtime.GOOS == "darwin" && (err == nil || !strings.Contains(err.Error(), "re-extract the complete")) {
		t.Fatalf("expected release error without fallback: %v", err)
	}
	if runtime.GOOS == "linux" && err != nil {
		t.Fatal(err)
	}
}

func hexDigest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func writeMetadata(t *testing.T, directory string, metadata map[string]any) {
	t.Helper()
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "release.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}
}
