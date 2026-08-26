package environment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSealIsDeterministicAndVerifyDetectsAssetMutation(t *testing.T) {
	directory := testBlueprint(t, "")
	sealedAt := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	first, _, err := Seal(directory, sealedAt)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := Seal(directory, sealedAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if first.EnvironmentDigest != second.EnvironmentDigest {
		t.Fatalf("seal digest changed with sealing time: %s != %s", first.EnvironmentDigest, second.EnvironmentDigest)
	}
	if _, _, err := Verify(directory); err != nil {
		t.Fatalf("verify sealed environment: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "assets", "fixtures.yaml"), []byte("rules:\n  - id: default\n    priority: 1\n    match: {method: GET, path: /fixture}\n    response: {status: 200, body: changed}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(directory); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected stale lock after asset mutation, got %v", err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	directory := testBlueprint(t, "unexpected_field: true\n")
	if _, err := Load(directory); err == nil || !strings.Contains(err.Error(), "field unexpected_field not found") {
		t.Fatalf("expected strict field error, got %v", err)
	}
}

func TestValidateRejectsWritableSeedOutsideMutableRoots(t *testing.T) {
	directory := testBlueprint(t, "filesystem:\n  seed:\n    - source: assets/fixtures.yaml\n      target: /elsewhere/file\n      read_only: false\n")
	if _, err := Validate(directory); err == nil || !strings.Contains(err.Error(), "outside application mutable_roots") {
		t.Fatalf("expected mutable-root validation error, got %v", err)
	}
}

func TestValidateAcceptsMCPHTTPAssetsAndRejectsMissingSeed(t *testing.T) {
	directory := testBlueprint(t, "")
	tools := `tools:
  - name: get_ticket
    description: Read a ticket
    input_schema:
      type: object
      properties: {id: {type: string}}
      required: [id]
    behavior:
      kind: stateful
      action: get
      collection: tickets
      key_argument: id
`
	if err := os.WriteFile(filepath.Join(directory, "assets", "mcp-tools.yaml"), []byte(tools), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "assets", "mcp-seed.json"), []byte(`{"tickets":{"t-1":{"id":"t-1"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	blueprintPath := filepath.Join(directory, BlueprintFile)
	blueprint, err := os.ReadFile(blueprintPath)
	if err != nil {
		t.Fatal(err)
	}
	addition := `  - id: tools
    kind: mcp_http
    listen: 127.0.0.1:18101
    hostnames: [tools.sandbox.local]
    endpoint: /mcp
    tools: assets/mcp-tools.yaml
    seed: assets/mcp-seed.json
`
	updated := strings.Replace(string(blueprint), "    fixtures: assets/fixtures.yaml\n", "    fixtures: assets/fixtures.yaml\n"+addition, 1)
	if err := os.WriteFile(blueprintPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Validate(directory)
	if err != nil {
		t.Fatalf("validate MCP service: %v", err)
	}
	if len(report.Assets) != 3 {
		t.Fatalf("MCP tool and seed files were not sealed assets: %#v", report.Assets)
	}

	withoutSeed := strings.Replace(updated, "    seed: assets/mcp-seed.json\n", "", 1)
	if err := os.WriteFile(blueprintPath, []byte(withoutSeed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(directory); err == nil || !strings.Contains(err.Error(), "requires a service seed") {
		t.Fatalf("expected stateful MCP tool without seed to fail, got %v", err)
	}
}

func TestValidateAcceptsSyntheticSDKCredentialsForModelProxy(t *testing.T) {
	directory := testBlueprint(t, `credentials:
  - id: local-aws-key
    environment_variable: AWS_ACCESS_KEY_ID
    value: interceptor-local
    accepted_by: model-proxy
`)
	path := filepath.Join(directory, BlueprintFile)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "      MODEL_BASE_URL: http://127.0.0.1:18080/v1\n", "      MODEL_BASE_URL: http://127.0.0.1:18080/v1\n      AWS_ACCESS_KEY_ID: interceptor-local\n", 1))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(directory); err != nil {
		t.Fatalf("synthetic SDK credential for model proxy was rejected: %v", err)
	}
}

func testBlueprint(t *testing.T, extra string) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "assets", "fixtures.yaml"), []byte("rules:\n  - id: default\n    priority: 1\n    match: {method: GET, path: /fixture}\n    response: {status: 200, body: original}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := `api_version: interceptor.dev/v1alpha1
kind: Environment
metadata:
  name: test-environment
  version: 0.1.0
  reviewed_by: test
  reviewed_at: 2026-08-24T00:00:00Z
application:
  image: test/app@sha256:0000000000000000000000000000000000000000000000000000000000000000
  launch:
    command: ["/app/test"]
    workdir: /app
    environment:
      MODEL_BASE_URL: http://127.0.0.1:18080/v1
  io:
    adapter: http
    application_listen: http://127.0.0.1:8080
    served_routes:
      - {method: POST, path: /invoke}
  persistence:
    snapshot_capable: false
    mutable_roots: [/state/app]
    quiesce: {kind: none}
services:
  - id: fixture
    kind: fixture_http
    listen: 127.0.0.1:18100
    hostnames: [fixture.sandbox.local]
    fixtures: assets/fixtures.yaml
` + extra
	if err := os.WriteFile(filepath.Join(directory, BlueprintFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return directory
}
