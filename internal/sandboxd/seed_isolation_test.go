package sandboxd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
)

func TestFileInjectionMutatesOnlyMaterializedSessionCopy(t *testing.T) {
	source := t.TempDir()
	benign := []byte("id,description\n1,benign ticket\n")
	if err := os.WriteFile(filepath.Join(source, "tickets.csv"), benign, 0600); err != nil {
		t.Fatal(err)
	}
	bp := &environment.Blueprint{
		Application: environment.Application{Persistence: environment.Persistence{MutableRoots: []string{"/state"}}},
		Filesystem:  environment.Filesystem{Seed: []environment.SeedEntry{{Source: "tickets.csv", Target: "/state/tickets.csv", ReadOnly: false}}}}
	roots, err := session.Materialize(source, bp, session.Paths{Generations: filepath.Join(t.TempDir(), "generations")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	runtime := roots[0].HostPath
	blobs, err := blobstore.Open(filepath.Join(t.TempDir(), "blobs"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{blueprint: &environment.Blueprint{
		Application: environment.Application{Persistence: environment.Persistence{MutableRoots: []string{runtime}}},
		Filesystem:  environment.Filesystem{InjectionNamespaces: []environment.FileInjectionNamespace{{ID: "records", Root: runtime, MaxFileBytes: 1024}}}},
		blobs: blobs}
	definition := injection.Definition{ID: "file-copy", Surface: "environment_state", Mode: "fidelity", Scope: "standing", Enabled: true,
		Selector:  injection.Selector{FileNamespace: "records", RelativePath: "tickets.csv"},
		Placement: injection.Placement{Operation: "replace"}, Payload: json.RawMessage(`"id,description\n1,injected marker\n"`)}
	_, apply, err := server.fidelityMutation(t.Context(), definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		actual, err := os.ReadFile(filepath.Join(runtime, "tickets.csv"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(actual, []byte("injected marker")) {
			t.Fatal("session mutation did not persist")
		}
	}
	original, err := os.ReadFile(filepath.Join(source, "tickets.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, benign) {
		t.Fatal("Blueprint source records were modified")
	}
	second, err := session.Materialize(source, bp, session.Paths{Generations: filepath.Join(t.TempDir(), "generations")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	pristine, err := os.ReadFile(filepath.Join(second[0].HostPath, "tickets.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pristine, benign) {
		t.Fatal("new session inherited another session's injection")
	}
}
