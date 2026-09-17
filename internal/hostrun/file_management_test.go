package hostrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/appimage"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/filemanagement"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/testdocker"
)

func TestQuotaTerminatesDuringUnresponsiveBootstrap(t *testing.T) {
	root := t.TempDir()
	docker, dockerRoot := testdocker.New(t)
	m := session.Metadata{SchemaVersion: 1, ID: "sess-1-000000000000000000000000", Phase: session.Creating, BridgeTransport: "spool", SpoolMaxBytes: 512, Limits: session.Limits{MaxBlobBytes: 1024}}
	paths, events, journal, err := session.Initialize(root, m, "")
	if err != nil {
		t.Fatal(err)
	}
	events.Close()
	journal.Close()
	if _, err := blobstore.Open(paths.Blobs, 1024); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	failed := make(chan error, 1)
	result := make(chan error, 1)
	go func() {
		_, err := runPrepared(ctx, Config{Docker: docker, BridgeTransport: "spool", InternalOnly: true, ProviderURL: "http://127.0.0.1:9", EnvironmentDirectory: root, OnFault: func(_ string, err error) { failed <- err }}, root, &environment.Blueprint{}, appimage.Lock{WrapperImage: "test:local"}, m, paths, nil)
		result <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dockerRoot, "interceptor-"+m.ID+".running")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Docker never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(paths.Sockets, "http-spool", "runaway.tmp"), make([]byte, 513), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		var f *filemanagement.SpoolFault
		if !errors.As(err, &f) || f.Reason != filemanagement.SpoolSizeLimit {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("quota fault missing")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("fault reported success")
		}
	case <-ctx.Done():
		t.Fatal("termination blocked by bootstrap")
	}
	calls, _ := os.ReadFile(filepath.Join(dockerRoot, "calls"))
	if !strings.Contains(string(calls), "kill --signal KILL interceptor-"+m.ID) {
		t.Fatal("did not kill Docker", string(calls))
	}
	for _, spool := range filemanagement.SpoolRoots(paths) {
		if _, err := os.Stat(spool); !os.IsNotExist(err) {
			t.Fatal("spool survived", err)
		}
	}
	loaded, _, err := session.Load(root, m.ID)
	if err != nil || loaded.Phase != session.Error || !strings.Contains(loaded.TerminationReason, filemanagement.SpoolSizeLimit) {
		t.Fatal(loaded, err)
	}
	for _, p := range []string{paths.Events, paths.Journal, filepath.Join(paths.Root, "transport-failure.json")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal("evidence missing", err)
		}
	}
}
