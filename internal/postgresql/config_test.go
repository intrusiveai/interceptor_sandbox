package postgresql

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/jackc/pgx/v5/pgproto3"
)

func TestConfigurationAndMutationPolicy(t *testing.T) {
	cfg := Config{Database: "support", Roles: []Role{{Name: "agent", Password: "synthetic"}}, Schema: []string{"schema.sql"}, Tables: []Table{{ID: "docs", Schema: "public", Name: "documents", Keys: []string{"tenant", "id"}, Fields: []string{"body"}}}}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	e := Engine{Config: cfg}
	good := injection.Definition{Selector: injection.Selector{Collection: "docs", Key: `{"tenant":"a","id":1}`}, Placement: injection.Placement{Operation: "append_text", Pointer: "/body"}}
	if err := e.ValidateMutation(good); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`{"id":1}`, `{"tenant":null,"id":1}`, `{"tenant":{},"id":1}`, `{"tenant":"a","id":1,"extra":1}`} {
		bad := good
		bad.Selector.Key = key
		if e.ValidateMutation(bad) == nil {
			t.Fatalf("accepted %s", key)
		}
	}
	cfg.Roles[0].Name = "interceptor_admin"
	if cfg.Validate() == nil {
		t.Fatal("accepted management role")
	}
	cfg.Roles[0].Name = "agent"
	cfg.Tables[0].Fields = append(cfg.Tables[0].Fields, "id")
	if cfg.Validate() == nil {
		t.Fatal("writable key accepted")
	}
}
func TestTypedResultPlacement(t *testing.T) {
	for _, format := range []int16{0, 1} {
		f := pgproto3.FieldDescription{DataTypeOID: 25, Format: format}
		out, err := encodeCell(f, []byte("original"), "changed")
		if err != nil || string(out) != "changed" {
			t.Fatalf("text %q %v", out, err)
		}
		f.DataTypeOID = 3802
		original := []byte(`{"body":"old"}`)
		if format == 1 {
			original = append([]byte{1}, original...)
		}
		out, err = encodeCell(f, original, map[string]any{"body": "new"})
		if err != nil {
			t.Fatal(err)
		}
		if format == 1 {
			if out[0] != 1 {
				t.Fatal("lost JSONB version")
			}
			out = out[1:]
		}
		if string(out) != `{"body":"new"}` {
			t.Fatal(string(out))
		}
	}
	f := pgproto3.FieldDescription{DataTypeOID: 1043, TypeModifier: 7}
	if _, err := encodeCell(f, []byte("old"), "too long"); err == nil {
		t.Fatal("varchar overflow accepted")
	}
	f.DataTypeOID = 23
	original := []byte{0, 0, 0, 7}
	f.Format = 1
	if out, err := encodeCell(f, original, decodeCell(f, original)); err != nil || string(out) != string(original) {
		t.Fatal("opaque passthrough changed")
	}
	if _, err := encodeCell(f, original, "payload"); err == nil {
		t.Fatal("numeric rewrite accepted")
	}
	if _, err := encodeCell(f, nil, "payload"); err == nil {
		t.Fatal("NULL rewrite accepted")
	}
	rows := []*pgproto3.DataRow{{Values: [][]byte{[]byte("value")}}}
	if _, err := encodeRows([]pgproto3.FieldDescription{{DataTypeOID: 25}}, rows, map[string]any{"rows": []any{}}, 1024); err == nil {
		t.Fatal("row removal accepted")
	}
}
func TestSQLCursorAndUnsupportedFlowLexing(t *testing.T) {
	c := newConnection(nil, 1)
	declaration := `DECLARE "docs" CURSOR FOR SELECT body FROM documents`
	c.cursorQuery(declaration)
	if got := c.cursorQuery(`FETCH FORWARD 1 FROM "docs"`); got != declaration {
		t.Fatal(got)
	}
	c.cursorQuery(`CLOSE "docs"`)
	if got := c.cursorQuery(`FETCH ALL FROM "docs"`); got == declaration {
		t.Fatal("closed cursor retained")
	}
	for _, sql := range []string{`SELECT 'COPY'`, `/* COPY */ SELECT $$COPY$$`, `SELECT "COPY"`} {
		tokens, err := sqlTokens(sql)
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range tokens {
			if strings.EqualFold(token, "COPY") {
				t.Fatal("quoted COPY treated as command")
			}
		}
	}
	if _, err := sqlTokens(`SELECT 'unterminated`); err == nil {
		t.Fatal("unterminated literal accepted")
	}
	raw := checkpointValue(map[string]json.RawMessage{"refund": json.RawMessage(`[[0]]`)})
	var metadata CheckpointMetadata
	if json.Unmarshal(raw, &metadata) != nil || string(metadata.OracleBaselines["refund"]) != "[[0]]" {
		t.Fatal(string(raw))
	}
}
