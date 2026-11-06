package integrationcontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
	if !bytes.Equal(raw, JSON()) || SHA256() != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatal("hash/export do not identify the entire embedded contract file")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	if SHA256() == fmt.Sprintf("%x", sha256.Sum256(compact.Bytes())) {
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
	var doc struct {
		Capabilities []struct {
			ID                                              string
			Operations, Inputs, Outputs, Errors, Guarantees []string
		}
	}
	if err := json.Unmarshal(JSON(), &doc); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, capability := range doc.Capabilities {
		if len(capability.Inputs)*len(capability.Outputs)*len(capability.Errors)*len(capability.Guarantees) == 0 {
			t.Fatalf("missing API behavior details: %s", capability.ID)
		}
		for _, operation := range capability.Operations {
			if !want[operation] || seen[operation] {
				t.Errorf("non-API or duplicated operation %q in %s", operation, capability.ID)
			}
			seen[operation] = true
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
