package statejournal

import (
	"encoding/json"
	"os"
	"testing"
)

func TestJournalPersistsAndVerifiesOperations(t *testing.T) {
	path := t.TempDir() + "/state.jsonl"
	writer, err := Open(path, "sess-test")
	if err != nil {
		t.Fatal(err)
	}
	value, _ := json.Marshal(map[string]any{"name": "alice"})
	transaction, err := writer.Append("service mutation", []Operation{{Kind: "service_put", Service: "tickets", Collection: "tickets", Key: "1", Value: value}})
	if err != nil {
		t.Fatal(err)
	}
	if transaction.Seq != 1 {
		t.Fatalf("unexpected sequence %d", transaction.Seq)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	transactions, err := VerifyFile(path)
	if err != nil || len(transactions) != 1 {
		t.Fatalf("verify: %v transactions=%d", err, len(transactions))
	}
	data, _ := os.ReadFile(path)
	data[len(data)/2] ^= 1
	_ = os.WriteFile(path, data, 0o600)
	if _, err := VerifyFile(path); err == nil {
		t.Fatal("expected tampering detection")
	}
}

func TestOperationRejectsPathEscapeAndUnknownKind(t *testing.T) {
	if err := ValidateOperation(Operation{Kind: "file_delete", Path: "/state/../etc/passwd"}); err == nil {
		t.Fatal("expected unclean path rejection")
	}
	if err := ValidateOperation(Operation{Kind: "invented"}); err == nil {
		t.Fatal("expected unknown operation rejection")
	}
}

func TestValidateOperationRejectsFieldsFromAnotherOperation(t *testing.T) {
	if err := ValidateOperation(Operation{Kind: "file_delete", Path: "/state/file", Service: "unexpected"}); err == nil {
		t.Fatal("expected unrelated operation field to be rejected")
	}
	if err := ValidateOperation(Operation{Kind: "injection_put", Value: json.RawMessage(`{"enabled":true}`)}); err == nil {
		t.Fatal("expected injection_put without id to be rejected")
	}
}
