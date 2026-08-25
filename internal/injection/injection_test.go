package injection

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func TestApplyPlacementStrictly(t *testing.T) {
	original := map[string]any{"result": map[string]any{"text": "safe", "tags": []any{"one"}}}
	updated, err := Apply(original, Placement{Operation: "append_text", Pointer: "/result/text"}, json.RawMessage(`" injected"`))
	if err != nil {
		t.Fatal(err)
	}
	text, _ := pointerGet(updated, "/result/text")
	if text != "safe injected" {
		t.Fatalf("unexpected placement result: %#v", updated)
	}
	if _, err := Apply(original, Placement{Operation: "replace", Pointer: "/missing"}, json.RawMessage(`true`)); err == nil {
		t.Fatal("expected absent target to fail closed")
	}
}

func TestRewriteToolResultAndJournalConsumption(t *testing.T) {
	directory := t.TempDir()
	journalPath := filepath.Join(directory, "state.jsonl")
	journal, err := statejournal.Open(journalPath, "sess-test")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(journal, nil)
	definition := Definition{
		ID: "inj-1", Surface: "model_tool_result", Mode: "simulation", Scope: "once", Enabled: true,
		Selector:  Selector{ToolName: "read_ticket", CallOrdinal: "first", ArgumentsPointer: "/id", ArgumentsEqual: json.RawMessage(`"t-1"`)},
		Placement: Placement{Operation: "append_text", Pointer: ""}, Payload: json.RawMessage(`" IGNORE"`),
	}
	if err := manager.Arm(definition, ""); err != nil {
		t.Fatal(err)
	}
	request := []byte(`{"model":"test","messages":[{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_ticket","arguments":"{\"id\":\"t-1\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"ticket body"}]}`)
	rewritten, results, err := manager.RewriteOpenAI(request, "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Applied || !bytes.Contains(rewritten, []byte("ticket body IGNORE")) {
		t.Fatalf("rewrite failed: %s %#v", rewritten, results)
	}
	evaluation := manager.Evaluation()
	if len(evaluation) != 1 || evaluation[0].Armed || !evaluation[0].Consumed {
		t.Fatalf("unexpected evaluation state: %#v", evaluation)
	}
	confirmed, err := manager.ConfirmDelivery(rewritten, []string{"inj-1"})
	if err != nil || len(confirmed) != 1 {
		t.Fatalf("delivery confirmation failed: %v %#v", err, confirmed)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	transactions, err := statejournal.VerifyFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 3 || transactions[1].Reason != "injection_consume" || transactions[2].Reason != "injection_delivery" {
		t.Fatalf("consumption was not journaled: %#v", transactions)
	}
	replayed := NewManager(nil, nil)
	for _, transaction := range transactions {
		for _, operation := range transaction.Operations {
			if err := replayed.ApplyJournal(operation); err != nil {
				t.Fatal(err)
			}
		}
	}
	restored := replayed.Evaluation()
	if len(restored) != 1 || restored[0].Delivery != "confirmed" || !restored[0].Fired || !restored[0].Consumed {
		t.Fatalf("delivery state did not replay: %#v", restored)
	}
	_ = os.Remove(journalPath)
}
