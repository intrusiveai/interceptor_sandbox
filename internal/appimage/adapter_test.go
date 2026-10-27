package appimage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

func TestStageAdapterChecksCopiedBytes(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets/adapter"), 0755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "assets/adapter/main.py")
	if err := os.WriteFile(entry, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	sealed, err := environment.AdapterFiles(root, "assets/adapter")
	if err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	if err := stageAdapter(root, stage, "assets/adapter", sealed); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(stage, "adapter/main.py"))
	if err != nil || string(data) != "original" {
		t.Fatalf("staged code: %s %v", data, err)
	}
	info, err := os.Stat(filepath.Join(stage, "adapter/main.py"))
	if err != nil || info.Mode().Perm() != 0444 {
		t.Fatalf("unreadable/writable adapter: %v", err)
	}
	if err := os.WriteFile(entry, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := stageAdapter(root, t.TempDir(), "assets/adapter", sealed); err == nil {
		t.Fatal("changed bytes accepted")
	}
	if err := stageAdapter(root, t.TempDir(), "assets/adapter", nil); err == nil {
		t.Fatal("unsealed code accepted")
	}
}

func TestAdapterDestinationRejectsCollisionAndSymlink(t *testing.T) {
	// Resolve macOS /var -> /private/var before checking container-style paths.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, environment.AdapterDirectory)
	if err := CheckAdapterDestination(destination); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0755); err != nil {
		t.Fatal(err)
	}
	if err := CheckAdapterDestination(destination); err == nil {
		t.Fatal("collision accepted")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckAdapterDestination(filepath.Join(link, "child", environment.AdapterDirectory)); err == nil {
		t.Fatal("symlink parent accepted")
	}
}

func TestAdapterRejectsImageVolumes(t *testing.T) {
	for _, volume := range []string{"/", "/app", "/app/.interceptor-adapter", "/app/.interceptor-adapter/helper"} {
		if err := checkAdapterVolumes("/app", []string{volume}); err == nil {
			t.Errorf("accepted image volume %s", volume)
		}
	}
	if err := checkAdapterVolumes("/app", []string{"/state", "/application"}); err != nil {
		t.Fatal(err)
	}
}
