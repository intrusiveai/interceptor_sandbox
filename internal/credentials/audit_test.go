package credentials

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCredentialAuditExcludesSecretMaterial(t *testing.T) {
	root := t.TempDir()
	event := AuditEvent{CredentialID: "model-key", ProfileID: "prod-store", BackendVersion: "version-7", ResolvedAt: time.Now().UTC(), Outcome: "resolved"}
	if err := AppendAudit(root, event); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "audit", "credential-resolutions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-value") || !strings.Contains(string(data), `"credential_id":"model-key"`) {
		t.Fatalf("unexpected credential audit record: %s", data)
	}
}
