package services

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
)

func TestMCPServedRecordInjectionPreservesSeedAndState(t *testing.T) {
	for _, pointer := range []string{"/structuredContent/status", "/structuredContent/absent", "/content/0/text"} {
		t.Run(pointer, func(t *testing.T) {
			dir := t.TempDir()
			toolsPath, seedPath := writeMCPTools(t, dir), writeMCPSeed(t, dir)
			seedBefore, err := os.ReadFile(seedPath)
			if err != nil {
				t.Fatal(err)
			}
			h, err := LoadMCP(toolsPath, seedPath, "/mcp", "tickets", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			stateBefore, _ := json.Marshal(h.Snapshot())
			manager := injectionManager(t, injection.Definition{ID: "served-copy", Surface: "mcp_tool_result", Mode: "simulation", Scope: "once", Enabled: true,
				Selector:  injection.Selector{Service: "tickets", ToolName: "get_ticket", CallOrdinal: "first"},
				Placement: injection.Placement{Operation: "append_text", Pointer: pointer}, Payload: json.RawMessage(`" injected marker"`)})
			wrapped := WithResponseInjection("tickets", "mcp_http", h, manager, nil)
			request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_ticket","arguments":{"id":"t-1"}}}`
			first := callMCP(t, wrapped, request)
			if first.Code != 200 {
				t.Fatal(first.Body.String())
			}
			wantInjected := pointer != "/structuredContent/absent"
			if bytes.Contains(first.Body.Bytes(), []byte("injected marker")) != wantInjected {
				t.Fatalf("unexpected response: %s", first.Body)
			}
			var response struct {
				Result struct {
					Structured map[string]any `json:"structuredContent"`
				}
			}
			if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Result.Structured["id"] != "t-1" {
				t.Fatal("unselected field changed")
			}
			if _, found := response.Result.Structured["absent"]; found {
				t.Fatal("missing selector was fabricated")
			}
			if pointer == "/content/0/text" && response.Result.Structured["status"] != "open" {
				t.Fatal("injection mirrored into unselected structured content")
			}
			second := callMCP(t, wrapped, request)
			if bytes.Contains(second.Body.Bytes(), []byte("injected marker")) {
				t.Fatal("once injection persisted")
			}
			stateAfter, _ := json.Marshal(h.Snapshot())
			seedAfter, err := os.ReadFile(seedPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(stateBefore, stateAfter) || !bytes.Equal(seedBefore, seedAfter) {
				t.Fatal("response injection changed benign source/state")
			}
		})
	}
}

func TestMCPExportDoesNotInferOrLeakRecords(t *testing.T) {
	dir := t.TempDir()
	h, err := LoadMCP(writeMCPTools(t, dir), writeMCPSeed(t, dir), "/mcp", "tickets", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	endpoints, err := h.DeliveryEndpoints()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(endpoints)
	for _, private := range []string{"Retention is seven years", "t-1", "127.0.0.1", "retention"} {
		if bytes.Contains(data, []byte(private)) {
			t.Fatalf("leaked fixture/seed value %q", private)
		}
	}
	for _, endpoint := range endpoints {
		if endpoint.OutputPointer != "/structuredContent" || endpoint.InjectionRoot != "mcp-tool-result" {
			t.Fatal("wrong MCP root")
		}
		if endpoint.Delivery.Output.Basis != "unknown" || endpoint.Delivery.Output.Schema != nil {
			t.Fatal("invented output schema")
		}
	}
	if !SupportsMCPImplementation("generic") || SupportsMCPImplementation("jira") || SupportsMCPImplementation("../../plugin") {
		t.Fatal("unexpected named implementation")
	}
}
