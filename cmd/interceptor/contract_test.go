package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo"
	"github.com/intrusive-ai/interceptor-sandbox/internal/integrationcontract"
)

func TestContractCommandsWithoutRuntime(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("INTERCEPTOR_DATA_DIR", "/unavailable/contract-test")
	for _, productVersion := range []string{"0.1.4", "9.8.7"} {
		previous := version
		version = productVersion
		for _, command := range []string{"version", "contract"} {
			var out, stderr bytes.Buffer
			if err := run([]string{command, "--json"}, &out, &stderr); err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result["interceptor_contract_version"] != integrationcontract.Version() {
				t.Fatal(result)
			}
			if command == "version" && (result["version"] != productVersion || result["source_commit"] != buildinfo.SourceCommit) {
				t.Fatal(result)
			}
		}
		for _, alias := range []string{"version", "--version", "-version"} {
			var out bytes.Buffer
			if err := run([]string{alias}, &out, &out); err != nil || out.String() != productVersion+"\n" {
				t.Fatal(out.String(), err)
			}
		}
		version = previous
	}
}
