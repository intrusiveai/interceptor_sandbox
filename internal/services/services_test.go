package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func TestFixtureHandlerMatchesAndFailsClosed(t *testing.T) {
	set := &FixtureSet{Rules: []FixtureRule{{
		ID: "ticket", Priority: 10,
		Match:    FixtureMatch{Method: "POST", Path: "/tickets", JSON: map[string]any{"team": "red"}},
		Response: FixtureResponse{Status: http.StatusCreated, JSON: map[string]any{"id": "t-1"}},
	}}}
	if err := ValidateFixtureSet(set); err != nil {
		t.Fatal(err)
	}
	handler := NewFixtureHandler("tickets", set, t.TempDir(), nil)

	request := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"team":"red","extra":true}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Body.String() != `{"id":"t-1"}` {
		t.Fatalf("unexpected fixture response: %d %s", response.Code, response.Body.String())
	}

	gap := httptest.NewRecorder()
	handler.ServeHTTP(gap, httptest.NewRequest(http.MethodGet, "/unknown?b=2&a=1", nil))
	if gap.Code != http.StatusNotImplemented {
		t.Fatalf("coverage gap returned %d", gap.Code)
	}
	coverage := handler.Coverage()
	if coverage.Total != 2 || coverage.Matched != 1 || coverage.Gaps["GET /unknown?a=1&b=2"] != 1 {
		t.Fatalf("unexpected coverage: %#v", coverage)
	}
}

func TestFixtureValidationRejectsOverlappingEqualPriorityRules(t *testing.T) {
	set := &FixtureSet{Rules: []FixtureRule{
		{ID: "broad", Priority: 1, Match: FixtureMatch{Method: "GET", Path: "/items"}, Response: FixtureResponse{Status: 200}},
		{ID: "narrow", Priority: 1, Match: FixtureMatch{Method: "GET", Path: "/items", Query: map[string]string{"team": "red"}}, Response: FixtureResponse{Status: 200}},
	}}
	if err := ValidateFixtureSet(set); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
	set.Rules[1].Priority = 2
	if err := ValidateFixtureSet(set); err != nil {
		t.Fatalf("explicit priority should resolve overlap: %v", err)
	}
}

func TestAttackerSinkCapturesBoundedRequest(t *testing.T) {
	handler, err := NewSinkHandler("exfil", []string{"POST"}, []string{"/collect"}, SinkResponse{Status: 200, JSON: map[string]any{"ok": true}}, 16, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/collect?source=agent", strings.NewReader("secret"))
	request.Header.Set("X-Test", "value")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("sink returned %d: %s", response.Code, response.Body.String())
	}
	observations := handler.Observations()
	if len(observations) != 1 || observations[0].Body != "secret" || observations[0].Query["source"][0] != "agent" {
		t.Fatalf("unexpected observations: %#v", observations)
	}
	overflow := httptest.NewRecorder()
	handler.ServeHTTP(overflow, httptest.NewRequest(http.MethodPost, "/collect", strings.NewReader("this body is much too long")))
	if overflow.Code != http.StatusRequestEntityTooLarge || len(handler.Observations()) != 1 {
		t.Fatalf("overflow should not be captured: %d %#v", overflow.Code, handler.Observations())
	}
}

func TestStatefulHandlerJournalsMutations(t *testing.T) {
	directory := t.TempDir()
	seed := filepath.Join(directory, "seed.json")
	routes := filepath.Join(directory, "routes.yaml")
	if err := os.WriteFile(seed, []byte(`{"tickets":{"t-1":{"id":"t-1","status":"open"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(routes, []byte(`routes:
  - id: list
    method: GET
    path: /tickets
    action: list
    collection: tickets
  - id: create
    method: POST
    path: /tickets
    action: create
    collection: tickets
    key_field: id
  - id: patch
    method: PATCH
    path: /tickets/{id}
    action: patch
    collection: tickets
    patch_fields: [status]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(directory, "state.jsonl")
	journal, err := statejournal.Open(journalPath, "sess-test")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := LoadStateful(seed, routes, "tickets", journal, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"id":"t-2","status":"new"}`)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", response.Code, response.Body.String())
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	transactions, err := statejournal.VerifyFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 1 || transactions[0].Operations[0].Kind != "service_put" {
		t.Fatalf("unexpected journal: %#v", transactions)
	}
	state := handler.Snapshot()
	var created map[string]any
	if err := json.Unmarshal(state["tickets"]["t-2"], &created); err != nil || created["status"] != "new" {
		t.Fatalf("unexpected state: %#v (%v)", created, err)
	}
}

func TestStatefulHandlerValidatesInjectedCreationAgainstSealedRoute(t *testing.T) {
	directory := t.TempDir()
	seed := filepath.Join(directory, "seed.json")
	routes := filepath.Join(directory, "routes.yaml")
	if err := os.WriteFile(seed, []byte(`{"tickets":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(routes, []byte("routes:\n  - id: create\n    method: POST\n    path: /tickets\n    action: create\n    collection: tickets\n    key_field: id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler, err := LoadStateful(seed, routes, "tickets", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	valid := statejournal.Operation{Kind: "service_put", Service: "tickets", Collection: "tickets", Key: "t-2", Value: json.RawMessage(`{"id":"t-2","body":"injected"}`)}
	if !handler.SupportsCreate("tickets") || handler.ValidateCreate(valid) != nil {
		t.Fatal("sealed create contract rejected a valid injected resource")
	}
	invalid := valid
	invalid.Key = "different"
	if err := handler.ValidateCreate(invalid); err == nil {
		t.Fatal("injected resource with mismatched key was accepted")
	}
}
