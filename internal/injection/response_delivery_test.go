package injection

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func TestResponseDeliveryCandidatesReplayAndDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.jsonl")
	journal, err := statejournal.Open(path, "sess-test")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	manager := NewManager(journal, nil)
	for _, surface := range []string{"service_response", "mcp_tool_result"} {
		tool := ""
		if surface == "mcp_tool_result" {
			tool = "lookup"
		}
		definition := Definition{ID: surface, Surface: surface, Mode: "simulation", Scope: "once", Enabled: true, Selector: Selector{Service: "docs", ToolName: tool, CallOrdinal: "first"}, Placement: Placement{Operation: "append_text"}, Payload: json.RawMessage(`" marker"`)}
		if err = manager.Arm(definition, ""); err != nil {
			t.Fatal(err)
		}
		if _, results, err := manager.RewriteResponse(surface, "docs", tool, ResponseContext{}, "safe"); err != nil || len(results) != 1 || !results[0].Applied {
			t.Fatal(results, err)
		}
	}
	want := []string{"mcp_tool_result", "service_response"}
	if got := manager.ObservableResponseIDs(); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if err = journal.Close(); err != nil {
		t.Fatal(err)
	}
	transactions, err := statejournal.VerifyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	restoredJournal, err := statejournal.Open(filepath.Join(t.TempDir(), "state.jsonl"), "sess-restored")
	if err != nil {
		t.Fatal(err)
	}
	defer restoredJournal.Close()
	restored := NewManager(restoredJournal, nil)
	for _, transaction := range transactions {
		for _, operation := range transaction.Operations {
			if err = restored.ApplyJournal(operation); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := restored.ObservableResponseIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("consumed response eligibility lost on restore: %v", got)
	}
	if ids, err := restored.ConfirmDelivery([]byte(`{"content":"safe marker"}`), restored.ObservableResponseIDs()); err != nil || !reflect.DeepEqual(ids, want) {
		t.Fatal(ids, err)
	}
	for _, item := range restored.Evaluation() {
		if item.Delivery != "confirmed" || !item.Consumed {
			t.Fatal(item)
		}
	}
	for _, id := range want {
		if err := restored.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	if got := restored.ObservableResponseIDs(); len(got) != 0 {
		t.Fatal("deleted candidates retained", got)
	}
}
