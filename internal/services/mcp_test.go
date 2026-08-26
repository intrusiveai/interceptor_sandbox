package services

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func TestMCPHandlerProtocolFixturesAndCoverage(t *testing.T) {
	directory := t.TempDir()
	toolsPath := writeMCPTools(t, directory)
	seedPath := writeMCPSeed(t, directory)
	journal, err := statejournal.Open(filepath.Join(directory, "state.jsonl"), "sess-mcp")
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := recording.Open(filepath.Join(directory, "events.jsonl"), "sess-mcp")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := LoadMCP(toolsPath, seedPath, "/mcp", "knowledge-mcp", journal, recorder)
	if err != nil {
		t.Fatal(err)
	}

	initialize := callMCP(t, handler, `{"jsonrpc":"2.0","id":"init-1","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if initialize.Code != http.StatusOK || !strings.Contains(initialize.Body.String(), `"id":"init-1"`) || !strings.Contains(initialize.Body.String(), `"tools"`) {
		t.Fatalf("unexpected initialize response: %d %s", initialize.Code, initialize.Body.String())
	}

	list := callMCP(t, handler, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"search_documents"`) || !strings.Contains(list.Body.String(), `"patch_ticket"`) {
		t.Fatalf("unexpected tools/list response: %d %s", list.Code, list.Body.String())
	}

	fixture := callMCP(t, handler, `{"jsonrpc":"2.0","id":37,"method":"tools/call","params":{"name":"search_documents","arguments":{"query":"retention","extra":"allowed"}}}`)
	if fixture.Code != http.StatusOK || !strings.Contains(fixture.Body.String(), `"id":37`) || !strings.Contains(fixture.Body.String(), `"Retention is seven years"`) {
		t.Fatalf("unexpected fixture call response: %d %s", fixture.Code, fixture.Body.String())
	}

	gap := callMCP(t, handler, `{"jsonrpc":"2.0","id":38,"method":"tools/call","params":{"name":"search_documents","arguments":{"query":"unknown"}}}`)
	if gap.Code != http.StatusOK || !strings.Contains(gap.Body.String(), `"isError":true`) {
		t.Fatalf("unexpected fixture gap response: %d %s", gap.Code, gap.Body.String())
	}

	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if get.Code != http.StatusMethodNotAllowed {
		t.Fatalf("MCP GET should fail closed without streaming support: %d", get.Code)
	}

	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	events, err := recording.VerifyFile(filepath.Join(directory, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var sawCall, sawGap bool
	for _, event := range events {
		sawCall = sawCall || event.Kind == "mcp_tool_call"
		sawGap = sawGap || event.Kind == "coverage_gap"
	}
	if !sawCall || !sawGap {
		t.Fatalf("MCP evidence did not contain call and coverage gap: %#v", events)
	}
}

func TestMCPStatefulToolsJournalInjectAndReplay(t *testing.T) {
	directory := t.TempDir()
	toolsPath := writeMCPTools(t, directory)
	seedPath := writeMCPSeed(t, directory)
	journalPath := filepath.Join(directory, "state.jsonl")
	journal, err := statejournal.Open(journalPath, "sess-state")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := LoadMCP(toolsPath, seedPath, "/mcp", "knowledge-mcp", journal, nil)
	if err != nil {
		t.Fatal(err)
	}

	patch := callMCP(t, handler, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"patch_ticket","arguments":{"id":"t-1","status":"closed"}}}`)
	if patch.Code != http.StatusOK || !strings.Contains(patch.Body.String(), `closed`) {
		t.Fatalf("unexpected patch result: %d %s", patch.Code, patch.Body.String())
	}

	injected := statejournal.Operation{
		Kind: "service_put", Service: "knowledge-mcp", Collection: "tickets", Key: "t-1",
		Value: json.RawMessage(`{"id":"t-1","status":"injected"}`),
	}
	if _, err := journal.Append("injection_arm", []statejournal.Operation{injected}); err != nil {
		t.Fatal(err)
	}
	if err := handler.Apply(injected); err != nil {
		t.Fatal(err)
	}
	get := callMCP(t, handler, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_ticket","arguments":{"id":"t-1"}}}`)
	if !strings.Contains(get.Body.String(), `injected`) {
		t.Fatalf("MCP result did not expose injected managed state: %s", get.Body.String())
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	transactions, err := statejournal.VerifyFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 2 || transactions[0].Operations[0].Kind != "service_put" {
		t.Fatalf("unexpected MCP state journal: %#v", transactions)
	}

	restored, err := LoadMCP(toolsPath, seedPath, "/mcp", "knowledge-mcp", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, transaction := range transactions {
		for _, operation := range transaction.Operations {
			if err := restored.Apply(operation); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !bytes.Contains(restored.Snapshot()["tickets"]["t-1"], []byte(`injected`)) {
		t.Fatalf("replayed MCP state does not match snapshot source: %s", restored.Snapshot()["tickets"]["t-1"])
	}
}

func TestMCPDefinitionValidationRejectsAmbiguousFixtures(t *testing.T) {
	definition := MCPDefinition{Tools: []MCPTool{{
		Name: "search", Description: "Search", InputSchema: map[string]any{"type": "object"},
		Behavior: MCPBehavior{Kind: "fixture", Fixtures: []MCPFixtureRule{
			{ID: "broad", Priority: 1, Match: map[string]any{}, Result: valueMCPResult("a")},
			{ID: "narrow", Priority: 1, Match: map[string]any{"query": "x"}, Result: valueMCPResult("b")},
		}},
	}}}
	if err := ValidateMCPDefinition(&definition); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous MCP fixtures to fail, got %v", err)
	}
}

func callMCP(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Accept", "application/json, text/event-stream")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func writeMCPTools(t *testing.T, directory string) string {
	t.Helper()
	path := filepath.Join(directory, "mcp-tools.yaml")
	content := `tools:
  - name: search_documents
    description: Search the synthetic knowledge base
    input_schema:
      type: object
      properties:
        query: {type: string}
      required: [query]
    behavior:
      kind: fixture
      fixtures:
        - id: retention
          priority: 10
          match: {query: retention}
          result:
            content:
              - {type: text, text: "Retention is seven years"}
            structured_content: {answer: "Retention is seven years"}
  - name: get_ticket
    description: Read a synthetic ticket
    input_schema:
      type: object
      properties:
        id: {type: string}
      required: [id]
      additionalProperties: false
    behavior:
      kind: stateful
      action: get
      collection: tickets
      key_argument: id
  - name: patch_ticket
    description: Change a synthetic ticket
    input_schema:
      type: object
      properties:
        id: {type: string}
        status: {type: string}
      required: [id, status]
      additionalProperties: false
    behavior:
      kind: stateful
      action: patch
      collection: tickets
      key_argument: id
      patch_fields: [status]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeMCPSeed(t *testing.T, directory string) string {
	t.Helper()
	path := filepath.Join(directory, "mcp-seed.json")
	if err := os.WriteFile(path, []byte(`{"tickets":{"t-1":{"id":"t-1","status":"open"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
