package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func injectionManager(t *testing.T, definition injection.Definition) *injection.Manager {
	t.Helper()
	journal, err := statejournal.Open(filepath.Join(t.TempDir(), "state.jsonl"), "session-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	manager := injection.NewManager(journal, nil)
	if err := manager.Arm(definition, ""); err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestManagedResponseInjectionOccursAfterSuccessfulHandler(t *testing.T) {
	manager := injectionManager(t, injection.Definition{ID: "service-injection", Surface: "service_response", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{Service: "tickets", Path: "/tickets", CallOrdinal: "first"}, Placement: injection.Placement{Operation: "replace", Pointer: "/status"}, Payload: json.RawMessage(`"compromised"`), AttemptID: "attempt-1", PayloadDigest: digest.Bytes([]byte("payload"))})
	real := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "t-1", "status": "safe"})
	})
	handler := WithResponseInjection("tickets", "fixture_http", real, manager, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/tickets", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"status":"compromised"`) {
		t.Fatalf("response was not rewritten: %d %s", response.Code, response.Body.String())
	}
}

func TestNativeMCPResultInjectionDoesNotRewriteProtocolRequest(t *testing.T) {
	manager := injectionManager(t, injection.Definition{ID: "mcp-injection", Surface: "mcp_tool_result", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{Service: "docs", ToolName: "search", CallOrdinal: "first"}, Placement: injection.Placement{Operation: "append_text", Pointer: "/content/0/text"}, Payload: json.RawMessage(`" injected"`)})
	real := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "safe"}}}}
		json.NewEncoder(w).Encode(body)
	})
	handler := WithResponseInjection("docs", "mcp_http", real, manager, nil)
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"q":"x"}}}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), "safe injected") {
		t.Fatalf("native MCP result was not rewritten: %s", response.Body.String())
	}
}

func TestNativeMCPResultInjectionIgnoresMalformedSuccessEnvelope(t *testing.T) {
	manager := injectionManager(t, injection.Definition{ID: "unmatched", Surface: "mcp_tool_result", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{Service: "other", ToolName: "search", CallOrdinal: "first"}, Placement: injection.Placement{Operation: "replace"}, Payload: json.RawMessage(`"x"`)})
	real := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	handler := WithResponseInjection("docs", "mcp_http", real, manager, nil)
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"search","arguments":{}}}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatalf("unexpected malformed-envelope handling: %d %q", response.Code, response.Body.String())
	}
}

func TestDynamicAttackerSinkCapturesRequestAndInjectsResponse(t *testing.T) {
	manager := injectionManager(t, injection.Definition{ID: "c2", Surface: "service_response", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{Service: "c2", Method: "POST", Path: "/poll", CallOrdinal: "first", RequestBodyPointer: "/state", RequestBodyEqual: json.RawMessage(`"ready"`)}, Placement: injection.Placement{Operation: "replace"}, Payload: json.RawMessage(`"execute task"`)})
	sink, err := NewSinkHandler("c2", []string{"POST"}, []string{"/poll"}, SinkResponse{Status: 200, Body: "idle"}, 1024, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := WithResponseInjection("c2", "http_sink", sink, manager, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/poll", strings.NewReader(`{"state":"ready"}`)))
	if response.Code != 200 || response.Body.String() != "execute task" {
		t.Fatalf("dynamic sink response was not injected: %d %q", response.Code, response.Body.String())
	}
	if observations := sink.Observations(); len(observations) != 1 || observations[0].Body != `{"state":"ready"}` {
		t.Fatalf("dynamic sink did not capture its request: %#v", observations)
	}
}
