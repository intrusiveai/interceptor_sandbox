package recording

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
)

func TestWriterRequiresChannelAndProvenanceAndDetectsTampering(t *testing.T) {
	path := t.TempDir() + "/events.jsonl"
	writer, err := Open(path, "sess-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Append(Draft{Layer: "test", Kind: "invalid", Semantic: map[string]any{}}); err == nil {
		t.Fatal("expected invalid draft rejection")
	}
	if _, err := writer.Append(Draft{Channel: Control, Layer: "test", Kind: "created", Provenance: SandboxControl, Semantic: map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if events, err := VerifyFile(path); err != nil || len(events) != 1 {
		t.Fatalf("verify: %v events=%d", err, len(events))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 1
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyFile(path); err == nil {
		t.Fatal("expected tampering detection")
	}
}

func TestReferencesIncludesSemanticBlobReferences(t *testing.T) {
	first := digest.Bytes([]byte("request"))
	second := digest.Bytes([]byte("response"))
	event := Event{RawRef: first, Semantic: json.RawMessage(`{"request_ref":"` + first + `","nested":{"response_ref":"` + second + `"}}`)}
	references, err := References(event)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{first, second}
	sort.Strings(expected)
	if len(references) != 2 || references[0] != expected[0] || references[1] != expected[1] {
		t.Fatalf("unexpected references: %#v", references)
	}
}
