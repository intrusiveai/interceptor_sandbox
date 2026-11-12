package contracttest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func TestOwnershipEvidenceContract(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	encode := func(value any) []byte {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, schema := range []int{1, 2} {
		m := snapshot.Manifest{SchemaVersion: schema, Entries: []snapshot.Entry{{Path: "/state/app", Type: "directory", Mode: 0700}, {Path: "/state/app/private", Type: "file", Mode: 0600, Blob: digest.Bytes(nil)}}}
		if schema == 2 {
			m.Entries[0].Owner = snapshot.SupervisorOwner
			m.Entries[1].Owner = snapshot.ApplicationOwner
		}
		m.Digest = digest.Bytes(encode(m))
		if err := v.EvidenceMember("snapshot.Manifest", encode(m)); err != nil {
			t.Fatal(schema, err)
		}
		for _, bad := range []string{"owner-tamper", "missing-owner", "unknown-owner", "unknown-field", "unsupported-version", "mode-tamper"} {
			if schema == 1 && bad == "missing-owner" {
				continue
			}
			var value map[string]any
			json.Unmarshal(encode(m), &value)
			entry := value["entries"].([]any)[1].(map[string]any)
			switch bad {
			case "owner-tamper":
				entry["owner"] = "supervisor"
			case "missing-owner":
				delete(entry, "owner")
			case "unknown-owner":
				entry["owner"] = "root"
			case "unknown-field":
				entry["uid"] = 20000
			case "unsupported-version":
				value["schema_version"] = 3
			case "mode-tamper":
				entry["mode"] = 511
			}
			if err := v.EvidenceMember("snapshot.Manifest", encode(value)); err == nil {
				t.Fatal("accepted", schema, bad)
			}
		}
	}
	for _, kind := range []string{"file_upsert", "directory_ensure", "symlink_set"} {
		op := statejournal.Operation{Kind: kind, Path: "/state/app/private", Owner: "application", Mode: 0600}
		if kind == "file_upsert" {
			op.Blob = digest.Bytes(nil)
		}
		if kind == "symlink_set" {
			op.Target = "target"
		}
		tx := statejournal.Transaction{SchemaVersion: 1, SessionID: "session-test", Seq: 1, TxID: "tx-test", CommittedAt: time.Unix(0, 0).UTC(), Reason: "snapshot", Operations: []statejournal.Operation{op}, PreviousHash: "sha256:" + strings.Repeat("0", 64)}
		tx.Hash = digest.Bytes(encode(tx))
		if err := v.EvidenceMember("statejournal.Transaction", encode(tx)); err != nil {
			t.Fatal(kind, err)
		}
		tx.Operations[0].Owner = "supervisor"
		if err := v.EvidenceMember("statejournal.Transaction", encode(tx)); err == nil {
			t.Fatal("ownership tampering accepted", kind)
		}
		// Historical transactions retain the original omitted-owner hash encoding.
		tx.Operations[0].Owner = ""
		tx.Hash = ""
		tx.Hash = digest.Bytes(encode(tx))
		if err := v.EvidenceMember("statejournal.Transaction", encode(tx)); err != nil {
			t.Fatal("legacy journal rejected", err)
		}
	}
	for _, raw := range []string{`{"kind":"file_delete","path":"/state/app/x","owner":"application"}`, `{"kind":"directory_ensure","path":"/state/app","owner":"root"}`, `{"kind":"future_operation"}`} {
		if err := v.Validate("#/components/schemas/statejournal.Operation", []byte(raw)); err == nil {
			t.Fatal("invalid operation accepted", raw)
		}
	}
}

// Keep private QA data outside the repository. This opt-in test exercises the
// exact retained exports through the same contract checks used by archive CI.
func TestRetainedEvidenceArchives(t *testing.T) {
	files := os.Getenv("INTERCEPTOR_CONTRACT_EVIDENCE_ARCHIVES")
	if files == "" {
		t.Skip("set newline-separated INTERCEPTOR_CONTRACT_EVIDENCE_ARCHIVES to retained tar paths")
	}
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range strings.Split(strings.TrimSpace(files), "\n") {
		t.Run(filepath.Base(name), func(t *testing.T) {
			f, err := os.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := v.EvidenceArchive(f); err != nil {
				t.Fatal(err)
			}
		})
	}
}
