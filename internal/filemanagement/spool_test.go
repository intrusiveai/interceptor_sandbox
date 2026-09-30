package filemanagement

import (
	"context"
	"errors"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/testdocker"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSpoolAccountingAndFailures(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "http-spool")
	b := filepath.Join(root, "model-spool")
	for _, p := range []string{filepath.Join(a, "unexpected", "nested"), b} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for p, content := range map[string]string{filepath.Join(a, "unexpected", "nested", "temp"): "12345", filepath.Join(b, "ack"): "67890"} {
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	roots := []string{a, b}
	if size, err := ScanSpools(context.Background(), roots, 10); err != nil || size != 10 {
		t.Fatalf("exact limit: %d %v", size, err)
	}
	if err := CheckSpools(context.Background(), roots, 9); err == nil {
		t.Fatal("aggregate overflow accepted")
	} else {
		var f *SpoolFault
		if !errors.As(err, &f) || f.Reason != SpoolSizeLimit || f.Bytes != 10 {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(a, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := CheckSpools(context.Background(), roots, 100); err == nil {
		t.Fatal("symlink accepted")
	} else {
		var f *SpoolFault
		if !errors.As(err, &f) || f.Reason != SpoolInspectionFailure {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ScanSpools(ctx, roots, 100); err == nil {
		t.Fatal("canceled traversal continued")
	}
}

func TestSpoolWatchDetectsGrowth(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	faults := make(chan error, 1)
	go WatchSpools(ctx, []string{root}, 512, func(err error) { faults <- err })
	if err := os.WriteFile(filepath.Join(root, "runaway.tmp"), make([]byte, 513), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-faults:
		var fault *SpoolFault
		if !errors.As(err, &fault) || fault.Reason != SpoolSizeLimit {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("quota watcher did not terminate")
	}
}

func TestCleanupRequiresExitAndStoppedWriters(t *testing.T) {
	docker, dockerRoot := testdocker.New(t)
	root := t.TempDir()
	m := session.Metadata{SchemaVersion: 1, ID: "sess-1-000000000000000000000000", ContainerID: "interceptor-sess-1-000000000000000000000000", Phase: session.Error}
	m.ContainerID = testdocker.Retired(t, root, m.ID)
	paths := session.PathsFor(root, m.ID)
	if err := session.Save(paths.Metadata, m); err != nil {
		t.Fatal(err)
	}
	spool := SpoolRoots(paths)[0]
	if err := os.MkdirAll(spool, 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "keep")
	_ = os.WriteFile(sentinel, []byte("evidence"), 0600)
	_ = os.Symlink(outside, filepath.Join(spool, "link"))
	writer, err := Lock(root, "writers-"+m.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := CleanupSpools(context.Background(), root, m, docker); err == nil {
		t.Fatal("deleted while writer active")
	}
	writer.Close()
	for _, suffix := range []string{".present", ".running"} {
		_ = os.WriteFile(filepath.Join(dockerRoot, m.ContainerID+suffix), nil, 0600)
	}
	if err := CleanupSpools(context.Background(), root, m, docker); err == nil {
		t.Fatal("trusted stale error phase over live Docker")
	}
	_ = os.Remove(filepath.Join(dockerRoot, m.ContainerID+".running"))
	if err := CleanupSpools(context.Background(), root, m, docker); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spool); !os.IsNotExist(err) {
		t.Fatal("spool survived")
	}
	for _, p := range []string{sentinel, paths.Metadata} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSpoolLimitConfiguration(t *testing.T) {
	if DefaultSpoolMaxBytes != 536870912 {
		t.Fatal("default changed")
	}
	for _, value := range []string{"", "0", "-1", "512MB", "9223372036854775808"} {
		if _, err := ParseSpoolLimit(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if got, err := ParseSpoolLimit("1048576"); err != nil || got != 1048576 {
		t.Fatal(got, err)
	}
}

func TestCleanupDoesNotFollowReplacedSocketsDirectory(t *testing.T) {
	docker, _ := testdocker.New(t)
	root := t.TempDir()
	m := session.Metadata{SchemaVersion: 1, ID: "sess-1-000000000000000000000000", Phase: session.Stopped}
	m.ContainerID = testdocker.Retired(t, root, m.ID)
	paths := session.PathsFor(root, m.ID)
	if err := session.Save(paths.Metadata, m); err != nil {
		t.Fatal(err)
	}
	victim := t.TempDir()
	if err := os.MkdirAll(filepath.Join(victim, "http-spool"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, paths.Sockets); err != nil {
		t.Fatal(err)
	}
	if err := CleanupSpools(context.Background(), root, m, docker); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(victim, "http-spool")); err != nil {
		t.Fatal("followed sockets symlink", err)
	}
}
