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

func TestRewriteNativeResponsesAndBedrockToolResults(t *testing.T) {
	journal, err := statejournal.Open(filepath.Join(t.TempDir(), "state.jsonl"), "sess-test")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	manager := NewManager(journal, nil)
	if err := manager.Arm(Definition{
		ID: "inj-native", Surface: "model_tool_result", Mode: "simulation", Scope: "standing", Enabled: true,
		Selector:  Selector{ToolName: "lookup", CallOrdinal: "first", ArgumentsPointer: "/q", ArgumentsEqual: json.RawMessage(`"old"`)},
		Placement: Placement{Operation: "append_text"}, Payload: json.RawMessage(`" injected"`),
	}, ""); err != nil {
		t.Fatal(err)
	}

	responses := []byte(`{"model":"gpt-test","input":[{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{\"q\":\"old\"}"},{"type":"function_call_output","call_id":"call-1","output":"result"}]}`)
	rewrittenResponses, results, err := manager.RewriteOpenAIResponses(responses, "turn-responses")
	if err != nil || len(results) != 1 || !results[0].Applied || !bytes.Contains(rewrittenResponses, []byte(`"output":"result injected"`)) {
		t.Fatalf("Responses injection failed: err=%v results=%#v body=%s", err, results, rewrittenResponses)
	}

	bedrock := []byte(`{"messages":[{"role":"assistant","content":[{"toolUse":{"toolUseId":"tool-1","name":"lookup","input":{"q":"old"}}}]},{"role":"user","content":[{"toolResult":{"toolUseId":"tool-1","content":[{"text":"result"}]}}]}]}`)
	rewrittenBedrock, results, err := manager.RewriteBedrockConverse(bedrock, "turn-bedrock")
	if err != nil || len(results) != 1 || !results[0].Applied || !bytes.Contains(rewrittenBedrock, []byte(`"text":"result injected"`)) {
		t.Fatalf("Bedrock injection failed: err=%v results=%#v body=%s", err, results, rewrittenBedrock)
	}
}

func TestResponseInjectionOrderIsStableByID(t *testing.T) {
	journal, err := statejournal.Open(filepath.Join(t.TempDir(), "state.jsonl"), "sess-test")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	manager := NewManager(journal, nil)
	for _, item := range []struct{ id, payload string }{{"b", `"B"`}, {"a", `"A"`}} {
		if err := manager.Arm(Definition{ID: item.id, Surface: "service_response", Mode: "simulation", Scope: "standing", Enabled: true, Selector: Selector{Service: "tickets", CallOrdinal: "every"}, Placement: Placement{Operation: "append_text"}, Payload: json.RawMessage(item.payload)}, ""); err != nil {
			t.Fatal(err)
		}
	}
	updated, results, err := manager.RewriteResponse("service_response", "tickets", "", ResponseContext{Method: "GET", Path: "/tickets"}, "safe")
	if err != nil || updated != "safeAB" || len(results) != 2 || results[0].InjectionID != "a" {
		t.Fatalf("response injection was not deterministic: value=%v results=%#v err=%v", updated, results, err)
	}
}

func TestResponseInjectionUsesPerDefinitionRouteOrdinalAndRequestPredicates(t *testing.T) {
	journal, err := statejournal.Open(filepath.Join(t.TempDir(), "state.jsonl"), "sess-test")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	manager := NewManager(journal, nil)
	definition := Definition{
		ID: "targeted", Surface: "service_response", Mode: "simulation", Scope: "once", Enabled: true,
		Selector:  Selector{Service: "c2", Method: "POST", Path: "/poll", CallOrdinal: "first", QueryEqual: map[string]string{"agent": "one"}, RequestBodyPointer: "/state", RequestBodyEqual: json.RawMessage(`"ready"`)},
		Placement: Placement{Operation: "replace"}, Payload: json.RawMessage(`"task"`),
	}
	if err := manager.Arm(definition, ""); err != nil {
		t.Fatal(err)
	}
	unchanged, results, err := manager.RewriteResponse("service_response", "c2", "", ResponseContext{Method: "GET", Path: "/health"}, "safe")
	if err != nil || unchanged != "safe" || len(results) != 0 {
		t.Fatalf("unrelated route affected ordinal: value=%v results=%#v err=%v", unchanged, results, err)
	}
	updated, results, err := manager.RewriteResponse("service_response", "c2", "", ResponseContext{Method: "POST", Path: "/poll", Query: map[string][]string{"agent": {"one"}}, Body: map[string]any{"state": "ready"}}, "safe")
	if err != nil || updated != "task" || len(results) != 1 || !results[0].Applied {
		t.Fatalf("targeted response injection failed: value=%v results=%#v err=%v", updated, results, err)
	}
}

func TestResponseInjectionOrdinalProgressReplays(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "state.jsonl")
	journal, err := statejournal.Open(journalPath, "sess-test")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(journal, nil)
	if err := manager.Arm(Definition{ID: "second", Surface: "service_response", Mode: "simulation", Scope: "standing", Enabled: true, Selector: Selector{Service: "c2", Method: "GET", Path: "/poll", CallOrdinal: "2"}, Placement: Placement{Operation: "replace"}, Payload: json.RawMessage(`"task"`)}, ""); err != nil {
		t.Fatal(err)
	}
	request := ResponseContext{Method: "GET", Path: "/poll"}
	first, results, err := manager.RewriteResponse("service_response", "c2", "", request, "idle")
	if err != nil || first != "idle" || len(results) != 0 {
		t.Fatalf("first response unexpectedly matched: value=%v results=%#v err=%v", first, results, err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	transactions, err := statejournal.VerifyFile(journalPath)
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
			if err := restored.ApplyJournal(operation); err != nil {
				t.Fatal(err)
			}
		}
	}
	second, results, err := restored.RewriteResponse("service_response", "c2", "", request, "idle")
	if err != nil || second != "task" || len(results) != 1 || !results[0].Applied {
		t.Fatalf("restored ordinal did not advance to its persisted match: value=%v results=%#v err=%v", second, results, err)
	}
}
