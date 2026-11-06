package integrationcontract_test

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/contracttest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/integrationcontract"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestEmbeddedOpenAPI(t *testing.T) {
	v, err := contracttest.New()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(integrationcontract.Version()) {
		t.Fatal("invalid contract version")
	}
	if v.Document["info"].(map[string]any)["version"] != integrationcontract.Version() {
		t.Fatal("version differs")
	}
	raw, err := os.ReadFile("testdata/openapi-3.1-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var meta any
	if err = json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	const id = "https://spec.openapis.org/oas/3.1/schema/2022-10-07"
	if err = c.AddResource(id, meta); err != nil {
		t.Fatal(err)
	}
	compiled, err := c.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = compiled.Validate(v.Document); err != nil {
		t.Fatal(err)
	}
	schemas := v.Document["components"].(map[string]any)["schemas"].(map[string]any)
	for name := range schemas {
		if _, err = v.Schema("#/components/schemas/" + name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	data := integrationcontract.JSON()
	data[0] = '!'
	if integrationcontract.JSON()[0] != '{' {
		t.Fatal("mutable contract")
	}
	// All references must resolve inside the exact document being hashed.
	validator := v
	var visit func(any)
	visit = func(value any) {
		switch n := value.(type) {
		case map[string]any:
			for k, v := range n {
				if k == "$ref" {
					r, ok := v.(string)
					if !ok || len(r) < 2 || r[0] != '#' {
						t.Fatalf("external reference %v", v)
					}
					if _, err := validator.Schema(r); err != nil {
						t.Fatal(err)
					}
				}
				visit(v)
			}
		case []any:
			for _, v := range n {
				visit(v)
			}
		}
	}
	visit(v.Document)
}
