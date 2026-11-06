package integrationcontract_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/intrusive-ai/interceptor-sandbox/internal/contracttest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/integrationcontract"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestContractHashCoversExactDocument(t *testing.T) {
	raw, err := os.ReadFile("contract.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, integrationcontract.JSON()) || integrationcontract.SHA256() != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatal("hash/export do not identify the entire embedded contract file")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	if integrationcontract.SHA256() == fmt.Sprintf("%x", sha256.Sum256(compact.Bytes())) {
		t.Fatal("hash discarded document whitespace")
	}
}

// Compare the written API inventory with the actual public route dispatcher
// and native operation allowlist. No parallel production registry is introduced.
func TestContractOperationsMatchPublicAPI(t *testing.T) {
	want := map[string]bool{}
	for _, value := range sourceStrings(t, "../localapi/service.go", "ServeHTTP") {
		if strings.HasPrefix(value, "/v1/") {
			want["POST "+value] = true
		}
	}
	for _, value := range sourceStrings(t, "../adaptive/gateway.go", "allowedGatewayOperations") {
		// Public stop is mediated by /v1/lifecycle; the native route rejects it.
		if value != "session.stop" {
			want[value] = true
		}
	}
	if len(want) == 0 {
		t.Fatal("no API definitions found")
	}
	v, err := contracttest.New()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for route, item := range v.Document["paths"].(map[string]any) {
		if _, ok := item.(map[string]any)["post"]; !ok {
			t.Fatalf("missing POST %s", route)
		}
		seen["POST "+route] = true
	}
	schemas := v.Document["components"].(map[string]any)["schemas"].(map[string]any)
	for _, branch := range schemas["NativeRequest"].(map[string]any)["oneOf"].([]any) {
		ref := branch.(map[string]any)["$ref"].(string)
		variant := schemas[strings.TrimPrefix(ref, "#/components/schemas/")].(map[string]any)
		props := variant["properties"].(map[string]any)
		header := props["request"].(map[string]any)["properties"].(map[string]any)
		operation := header["operation"].(map[string]any)["const"].(string)
		if seen[operation] || !want[operation] {
			t.Errorf("duplicate or non-API operation %s", operation)
		}
		if props["body"].(map[string]any)["$ref"] == nil || variant["x-response-schema"] == nil {
			t.Errorf("untyped operation %s", operation)
		}
		seen[operation] = true
	}
	for operation := range seen {
		if !want[operation] {
			t.Errorf("non-API operation %s", operation)
		}
	}
	for operation := range want {
		if !seen[operation] {
			t.Errorf("API operation missing from contract: %s", operation)
		}
	}
}

func sourceStrings(t *testing.T, path, declaration string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var root ast.Node
	ast.Inspect(f, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncDecl:
			if n.Name.Name == declaration {
				root = n.Body
			}
		case *ast.ValueSpec:
			if len(n.Names) == 1 && n.Names[0].Name == declaration {
				root = n
			}
		}
		return true
	})
	if root == nil {
		t.Fatalf("missing declaration %s in %s", declaration, path)
	}
	var values []string
	ast.Inspect(root, func(node ast.Node) bool {
		if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			values = append(values, value)
		}
		return true
	})
	return values
}
