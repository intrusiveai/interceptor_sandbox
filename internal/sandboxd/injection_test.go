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
	"github.com/intrusive-ai/interceptor-sandbox/internal/services"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func TestFidelityInjectionCreatesOnlyBlueprintAuthorizedTargets(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox")
	if err := os.Mkdir(inbox, 0o750); err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(root, "seed.json")
	routes := filepath.Join(root, "routes.yaml")
	if err := os.WriteFile(seed, []byte(`{"tickets":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(routes, []byte("routes:\n  - id: create\n    method: POST\n    path: /tickets\n    action: create\n    collection: tickets\n    key_field: id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := statejournal.Open(filepath.Join(root, "state.jsonl"), "session-test")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	store, err := blobstore.Open(filepath.Join(root, "blobs"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stateful, err := services.LoadStateful(seed, routes, "tickets", journal, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		blueprint: &environment.Blueprint{
			Application: environment.Application{Persistence: environment.Persistence{MutableRoots: []string{root}}},
			Filesystem:  environment.Filesystem{InjectionNamespaces: []environment.FileInjectionNamespace{{ID: "inbox", Root: inbox, AllowCreate: true, MaxFiles: 2, MaxFileBytes: 1024}}},
			Services:    []environment.Service{{ID: "tickets", Kind: "stateful_http", Injections: environment.ServiceInjectionPolicy{CreateCollections: []environment.ResourceCreationPolicy{{Collection: "tickets", MaxResources: 2}}}}},
		},
		journal: journal, blobs: store, services: map[string]services.ManagedHandler{"tickets": stateful},
	}
	resource := injection.Definition{ID: "resource", Surface: "environment_state", Mode: "fidelity", Scope: "standing", Enabled: true, Selector: injection.Selector{Service: "tickets", Collection: "tickets", Key: "t-2"}, Placement: injection.Placement{Operation: "replace"}, Payload: json.RawMessage(`{"id":"t-2","body":"injected"}`)}
	operations, apply, err := server.fidelityMutation(t.Context(), resource)
	if err != nil || len(operations) != 1 || apply == nil {
		t.Fatalf("authorized resource creation failed: operations=%#v err=%v", operations, err)
	}
	if err := apply(); err != nil || !bytes.Contains(stateful.Snapshot()["tickets"]["t-2"], []byte("injected")) {
		t.Fatalf("authorized resource was not created: %v", err)
	}
	file := injection.Definition{ID: "file", Surface: "environment_state", Mode: "fidelity", Scope: "standing", Enabled: true, Selector: injection.Selector{FileNamespace: "inbox", RelativePath: "message.txt"}, Placement: injection.Placement{Operation: "replace"}, Payload: json.RawMessage(`"injected file"`)}
	operations, apply, err = server.fidelityMutation(t.Context(), file)
	if err != nil || len(operations) != 1 || operations[0].Path != filepath.Join(inbox, "message.txt") {
		t.Fatalf("authorized file creation failed: operations=%#v err=%v", operations, err)
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(inbox, "message.txt"))
	if err != nil || string(content) != "injected file" {
		t.Fatalf("authorized file was not created: %q %v", content, err)
	}
	file.Selector = injection.Selector{Path: filepath.Join(root, "undeclared.txt")}
	if _, _, err := server.fidelityMutation(t.Context(), file); err == nil {
		t.Fatal("file creation outside a named namespace was accepted")
	}
}

func TestInjectionTargetRejectsStaticAttackerResponse(t *testing.T) {
	server := &Server{blueprint: &environment.Blueprint{AttackerServices: []environment.AttackerService{
		{ID: "exfil", Kind: "http_sink", Response: environment.SinkResponse{Mode: "static", Status: 204}},
		{ID: "c2", Kind: "http_sink", Response: environment.SinkResponse{Mode: "dynamic", Status: 200}},
	}}}
	definition := injection.Definition{ID: "response", Surface: "service_response", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{Service: "exfil", Path: "/collect", CallOrdinal: "first"}, Placement: injection.Placement{Operation: "replace"}, Payload: json.RawMessage(`"task"`)}
	if err := server.validateInjectionTarget(definition); err == nil {
		t.Fatal("static attacker response accepted an injection")
	}
	definition.Selector.Service = "c2"
	if err := server.validateInjectionTarget(definition); err != nil {
		t.Fatalf("dynamic attacker response rejected an injection: %v", err)
	}
}
