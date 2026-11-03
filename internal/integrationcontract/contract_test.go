package integrationcontract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestEmbeddedContract(t *testing.T) {
	if len(Version()) > 128 || !regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(Version()) {
		t.Fatal("invalid contract version", Version())
	}
	var doc struct {
		Version      string `json:"interceptor_contract_version"`
		Capabilities []struct {
			ID                                                  string `json:"id"`
			Description                                         string `json:"description"`
			Operations, Inputs, Outputs, Guarantees, References []string
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(JSON(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != Version() || len(doc.Capabilities) == 0 {
		t.Fatal("missing contract")
	}
	seen := map[string]bool{}
	for _, c := range doc.Capabilities {
		if c.ID == "" || seen[c.ID] || c.Description == "" || len(c.Operations)*len(c.Inputs)*len(c.Outputs)*len(c.Guarantees)*len(c.References) == 0 {
			t.Fatalf("incomplete or duplicate capability: %+v", c)
		}
		seen[c.ID] = true
		for _, ref := range c.References {
			if !filepath.IsLocal(ref) {
				t.Fatal(ref)
			}
			if _, err := os.Stat(filepath.Join("../..", ref)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The exported document is a copy; a caller cannot change the process identity.
	data := JSON()
	data[0] = '!'
	if JSON()[0] != '{' {
		t.Fatal("mutable embedded contract")
	}
}
