package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo"
	"github.com/intrusive-ai/interceptor-sandbox/internal/integrationcontract"
	"github.com/intrusive-ai/interceptor-sandbox/internal/localapi"
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
			actual := result["interceptor_contract_version"]
			if command == "contract" {
				actual = result["info"].(map[string]any)["version"]
			}
			if actual != integrationcontract.Version() {
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

func TestContractExportMatchesAPIHash(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("INTERCEPTOR_DATA_DIR", "/unavailable/contract-test")
	var exported, stderr bytes.Buffer
	if err := run([]string{"contract", "--json"}, &exported, &stderr); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(exported.Bytes(), integrationcontract.JSON()) {
		t.Fatal("export changed embedded contract bytes")
	}
	// Exercise the real HTTP endpoint and CLI output without a target or campaign.
	s := httptest.NewServer(&localapi.Service{})
	defer s.Close()
	r, err := s.Client().Post(s.URL+"/v1/contract-version", "application/json", bytes.NewBufferString("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var reply struct {
		Status int
		Body   map[string]string
	}
	if err := json.NewDecoder(r.Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 200 || reply.Status != 200 || reply.Body["interceptor_contract_sha256"] != fmt.Sprintf("%x", sha256.Sum256(exported.Bytes())) || reply.Body["interceptor_contract_version"] != integrationcontract.Version() {
		t.Fatalf("API identity does not match exported contract: %+v", reply)
	}
}
