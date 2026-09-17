package delivery

import (
	"strings"
	"testing"
)

func TestContractAndBodyValidation(t *testing.T) {
	input := Body{MediaType: "application/json", Encoding: "json", MaxBytes: 1024, Basis: "source-derived",
		Schema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string", "minLength": 1}}, "required": []any{"query"}, "additionalProperties": false}}
	contract := &Contract{Description: "Submit an agent task", Input: input,
		Output:   Body{MediaType: "application/json", Encoding: "json", MaxBytes: 1024, Basis: "unknown"},
		Examples: []Example{{ID: "benign", Input: map[string]any{"query": "Summarize the ticket"}, Output: map[string]any{"answer": "Example only"}}}}
	if err := Validate(contract); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`{"query":"hello"}`, `{"query":"attack text is not restricted by the benign example"}`} {
		if err := ValidateBytes(input, []byte(value)); err != nil {
			t.Fatalf("valid body: %v", err)
		}
	}
	for _, value := range []string{`{}`, `{"query":1}`, `{"query":"x","extra":true}`, `{"query":"x","query":"y"}`, "prose", strings.Repeat(" ", 1025)} {
		if err := ValidateBytes(input, []byte(value)); err == nil {
			t.Fatalf("accepted invalid body %q", value)
		}
	}
	contract.Examples[0].Input = map[string]any{"query": 1}
	if Validate(contract) == nil {
		t.Fatal("invalid benign example accepted")
	}
	contract.Examples = nil
	contract.Output.Schema = map[string]any{"type": "object"}
	if Validate(contract) == nil {
		t.Fatal("unknown output asserted a schema")
	}
}

func TestClosedOfflineSchemaProfile(t *testing.T) {
	for _, schema := range []map[string]any{
		{"$ref": "https://example.invalid/schema"}, {"$ref": "#/properties/x"},
		{"pattern": ".*"}, {"default": "guessed"}, {"type": "invented"},
		{"properties": map[string]any{"nested": map[string]any{"$ref": "file:///etc/passwd"}}},
	} {
		if _, err := Compile(schema); err == nil {
			t.Fatalf("accepted unsupported schema %#v", schema)
		}
	}
	if _, err := Compile(map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "maxItems": 2}); err != nil {
		t.Fatal(err)
	}
}

func TestStrictJSONBoundaries(t *testing.T) {
	for _, value := range []string{`{"x":1,"x":2}`, `{} {}`, `"\ud800"`, `"\udc00"`, string([]byte{'"', 0xff, '"'}), strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34)} {
		if _, err := DecodeJSON([]byte(value)); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	for _, value := range []string{`"\ud83d\ude00"`, `"\\ud800"`, `{"x":"escaped \" quote"}`, `null`} {
		if _, err := DecodeJSON([]byte(value)); err != nil {
			t.Fatalf("rejected %q: %v", value, err)
		}
	}
	body := Body{Encoding: "utf8", MaxBytes: 3}
	if ValidateBytes(body, []byte("abc")) != nil || ValidateBytes(body, []byte("abcd")) == nil {
		t.Fatal("byte boundary")
	}
}

func FuzzDecodeJSON(f *testing.F) {
	for _, seed := range []string{`{"query":"hello"}`, `{"a":1,"a":2}`, `"\ud800"`, "[]"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) <= 4096 {
			_, _ = DecodeJSON(data)
		}
	})
}
