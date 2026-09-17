package credentials

import (
	"encoding/json"
	"errors"
	"github.com/intrusive-ai/interceptor-sandbox/internal/filemanagement"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const AuditVersion = "interceptor.dev/credential-resolution-audit/v1alpha1"

// AuditEvent intentionally contains no locator, secret value, authentication
// material, or value digest.
type AuditEvent struct {
	APIVersion     string    `json:"api_version"`
	CredentialID   string    `json:"credential_id"`
	ProfileID      string    `json:"profile_id,omitempty"`
	BackendVersion string    `json:"backend_version,omitempty"`
	ResolvedAt     time.Time `json:"resolved_at"`
	Outcome        string    `json:"outcome"`
}

func AppendAudit(dataRoot string, event AuditEvent) error {
	if dataRoot == "" || event.CredentialID == "" || event.ResolvedAt.IsZero() || event.Outcome != "resolved" && event.Outcome != "failed" {
		return errors.New("credential audit event is invalid")
	}
	maintenance, err := filemanagement.Lock(dataRoot, "maintenance", false)
	if err != nil {
		return err
	}
	defer maintenance.Close()
	event.APIVersion = AuditVersion
	directory := filepath.Join(dataRoot, "audit")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(directory, "credential-resolutions.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	if err := json.NewEncoder(file).Encode(event); err != nil {
		return err
	}
	return file.Sync()
}
