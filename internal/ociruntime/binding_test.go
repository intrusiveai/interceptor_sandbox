package ociruntime_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/ociruntime"
	"github.com/intrusive-ai/interceptor-sandbox/internal/testdocker"
)

func launch(t *testing.T) (ociruntime.Docker, string, string, string) {
	t.Helper()
	ctx := context.Background()
	docker, fake := testdocker.New(t)
	// Do not depend on the test runner's real Docker environment.
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	docker, err := docker.Pin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	docker, err = docker.PrepareBinding(root, "sess-example")
	if err != nil {
		t.Fatal(err)
	}
	id, err := docker.Create(ctx, ociruntime.RunConfig{Name: "interceptor-example", Image: "test:local"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(ociruntime.BindingPath(root, "sess-example"))
	if err != nil {
		t.Fatal(err)
	}
	var binding ociruntime.Binding
	if err := json.Unmarshal(raw, &binding); err != nil {
		t.Fatal(err)
	}
	if binding.ContainerID != id || binding.Endpoint != "unix:///test/docker.sock" || binding.DaemonID != "test-daemon" {
		t.Fatalf("incomplete binding: %s", raw)
	}
	if _, err := os.Stat(filepath.Join(fake, id+".running")); !os.IsNotExist(err) {
		t.Fatal("create started target before binding persistence")
	}
	if err := docker.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	return docker, root, fake, id
}

func TestRecoveryUsesOriginalDaemonAndIDAfterRenameAndContextChange(t *testing.T) {
	docker, root, fake, id := launch(t)
	// Another process has renamed the original and reused its old name.
	if err := os.WriteFile(filepath.Join(fake, id+".name"), []byte("renamed"), 0600); err != nil {
		t.Fatal(err)
	}
	replacement := strings.Repeat("b", 64)
	for suffix, value := range map[string]string{".name": "interceptor-example", ".session": "another-session", ".present": "", ".running": ""} {
		if err := os.WriteFile(filepath.Join(fake, replacement+suffix), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DOCKER_CONTEXT", "elsewhere")
	t.Setenv("DOCKER_HOST", "tcp://elsewhere:2375")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	recovered, recoveredID, err := (ociruntime.Docker{Binary: docker.Binary}).ForSession(context.Background(), root, "sess-example")
	if err != nil {
		t.Fatal(err)
	}
	if recoveredID != id {
		t.Fatal(recoveredID)
	}
	if err := recovered.EnsureRemoved(context.Background(), recoveredID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fake, id+".present")); !os.IsNotExist(err) {
		t.Fatal("original survived")
	}
	if _, err := os.Stat(filepath.Join(fake, replacement+".running")); err != nil {
		t.Fatal("replacement was touched", err)
	}
	if err := recovered.EnsureRemoved(context.Background(), recoveredID, true); err != nil {
		t.Fatal("confirmed removal not retryable", err)
	}
	calls, _ := os.ReadFile(filepath.Join(fake, "calls"))
	if !strings.Contains(string(calls), "--host unix:///test/docker.sock kill --signal KILL "+id) {
		t.Fatal(string(calls))
	}
}

func TestUncertainBindingNeverSignalsOrConfirmsAbsence(t *testing.T) {
	for _, mode := range []string{"unavailable", "replaced-daemon", "wrong-label", "missing-container", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			docker, root, fake, id := launch(t)
			switch mode {
			case "unavailable":
				os.WriteFile(filepath.Join(fake, "unavailable"), nil, 0600)
			case "replaced-daemon":
				os.WriteFile(filepath.Join(fake, "daemon-id"), []byte("another-daemon"), 0600)
			case "wrong-label":
				os.WriteFile(filepath.Join(fake, id+".session"), []byte("another-session"), 0600)
			case "missing-container":
				os.Remove(filepath.Join(fake, id+".present"))
			case "legacy":
				os.Remove(ociruntime.BindingPath(root, "sess-example"))
			}
			os.WriteFile(filepath.Join(fake, "calls"), nil, 0600)
			bound, gotID, err := docker.ForSession(context.Background(), root, "sess-example")
			if err == nil {
				err = bound.EnsureRemoved(context.Background(), gotID, true)
			}
			if err == nil {
				t.Fatal("uncertain cleanup succeeded")
			}
			calls, _ := os.ReadFile(filepath.Join(fake, "calls"))
			for _, command := range []string{" kill ", " stop ", " rm "} {
				if strings.Contains(string(calls), command) {
					t.Fatalf("unsafe command: %s", calls)
				}
			}
		})
	}
}

func TestPinRejectsRemoteEndpoint(t *testing.T) {
	docker, _ := testdocker.New(t)
	docker.Endpoint = "tcp://127.0.0.1:2375"
	if _, err := docker.Pin(context.Background()); err == nil {
		t.Fatal("accepted TCP Docker endpoint")
	}
}

func TestInterruptedCreateRecoversByLabelBeforeCleanup(t *testing.T) {
	docker, root, _, id := launch(t)
	path := ociruntime.BindingPath(root, "sess-example")
	raw, _ := os.ReadFile(path)
	var b ociruntime.Binding
	json.Unmarshal(raw, &b)
	b.ContainerID = ""
	raw, _ = json.Marshal(b)
	os.WriteFile(path, raw, 0600)
	recovered, got, err := docker.ForSession(context.Background(), root, "sess-example")
	if err != nil || got != id {
		t.Fatal(got, err)
	}
	if err := recovered.EnsureRemoved(context.Background(), got, true); err != nil {
		t.Fatal(err)
	}
}
