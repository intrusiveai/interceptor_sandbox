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
