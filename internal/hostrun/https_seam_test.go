package hostrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/httpsseam"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
)

func TestSeamRestoreRejectsChangedHostAuthority(t *testing.T) {
	root := t.TempDir()
	blueprint := &environment.Blueprint{Network: environment.NetworkSimulation{HTTPSSeams: []environment.HTTPSSeam{{ID: "tracing", Hostname: "trace.example"}}}}
	load := func(origin string) *httpsseam.Prepared {
		t.Helper()
		path := filepath.Join(root, "config.yaml")
		err := os.WriteFile(path, []byte("seams:\n  - id: tracing\n    origin: "+origin+"\n    auth: {kind: none}\n"), 0600)
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := httpsseam.Load(context.Background(), path, blueprint.Network.HTTPSSeams, nil)
		if err != nil {
			t.Fatal(err)
		}
		return prepared
	}
	original := load("https://public.example")
	metadata := session.Metadata{ID: "session-original"}
	paths := session.PathsFor(root, metadata.ID)
	if err := bindHTTPSSeams(original, blueprint, root, metadata, paths); err != nil {
		t.Fatal(err)
	}
	restored := session.Metadata{ID: "session-restored", ParentSessionID: metadata.ID}
	restoredPaths := session.PathsFor(root, restored.ID)
	if err := bindHTTPSSeams(original, blueprint, root, restored, restoredPaths); err != nil {
		t.Fatal(err)
	}
	if err := bindHTTPSSeams(load("https://private.example"), blueprint, root, restored, restoredPaths); err == nil {
		t.Fatal("restore changed remote authority")
	}
	if err := bindHTTPSSeams(nil, blueprint, root, restored, restoredPaths); err == nil {
		t.Fatal("missing bindings accepted")
	}
}
