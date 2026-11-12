package contracttest

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

// EvidenceArchive checks actual producer output against the contract's archive
// member mapping and native record hashes. This qualification check complements,
// and does not replace, Operator's independent provenance/binding verifier.
func (v *Validator) EvidenceArchive(input io.Reader) error {
	response := v.Document["paths"].(map[string]any)["/v1/evidence"].(map[string]any)["post"].(map[string]any)["responses"].(map[string]any)["200"].(map[string]any)
	members := response["x-evidence-members"].([]any)
	seen := map[string]bool{}
	matched := map[string]bool{}
	reader := tar.NewReader(input)
	var session struct {
		ParentSessionID     string `json:"parent_session_id"`
		OperationAPIVersion string `json:"operation_api_version"`
	}
	var bundle adaptive.EvidenceBundle
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA || h.Name == "." || path.IsAbs(h.Name) || path.Clean(h.Name) != h.Name || strings.HasPrefix(h.Name, "../") || seen[h.Name] {
			return fmt.Errorf("unsafe/duplicate archive member %s", h.Name)
		}
		seen[h.Name] = true
		var spec map[string]any
		for _, item := range members {
			m := item.(map[string]any)
			ok, err := regexp.MatchString(m["pattern"].(string), h.Name)
			if err != nil {
				return err
			}
			if ok {
				spec = m
				matched[m["pattern"].(string)] = true
				break
			}
		}
		if spec == nil {
			return fmt.Errorf("undocumented archive member %s", h.Name)
		}
		if spec["encoding"] == "bytes" {
			hash, _, err := digest.Reader(reader)
			if err != nil {
				return err
			}
			if h.Name != "blobs/sha256/"+strings.TrimPrefix(hash, "sha256:") {
				return fmt.Errorf("blob digest mismatch: %s", h.Name)
			}
			continue
		}
		pointer := spec["schema"].(map[string]any)["$ref"].(string)
		previous := recording.ZeroHash
		seq := uint64(0)
		sessionID := ""
		validate := func(raw []byte) error {
			if err := v.Validate(pointer, raw); err != nil {
				return fmt.Errorf("%s: %w", h.Name, err)
			}
			if err := verifyNativeRecord(pointer, raw); err != nil {
				return fmt.Errorf("%s: %w", h.Name, err)
			}
			if spec["encoding"] == "jsonl" {
				var record struct {
					SessionID    string `json:"session_id"`
					Seq          uint64 `json:"seq"`
					PreviousHash string `json:"previous_hash"`
					Hash         string `json:"hash"`
				}
				if err := json.Unmarshal(raw, &record); err != nil {
					return err
				}
				seq++
				if seq == 1 {
					sessionID = record.SessionID
				}
				if record.Seq != seq || record.PreviousHash != previous || record.SessionID != sessionID {
					return fmt.Errorf("%s: invalid hash-chain position or identity", h.Name)
				}
				previous = record.Hash
			}
			return nil
		}
		if spec["encoding"] == "jsonl" {
			lines := bufio.NewReader(reader)
			for {
				raw, err := lines.ReadBytes('\n')
				if err != nil && err != io.EOF {
					return err
				}
				if len(raw) > 0 {
					if err := validate(raw); err != nil {
						return err
					}
				}
				if err == io.EOF {
					break
				}
			}
		} else {
			raw, err := io.ReadAll(reader)
			if err != nil {
				return err
			}
			if err := validate(raw); err != nil {
				return err
			}
			if h.Name == "session.json" {
				if err := json.Unmarshal(raw, &session); err != nil {
					return err
				}
			}
			if h.Name == "evidence-bundle.json" {
				if err := json.Unmarshal(raw, &bundle); err != nil {
					return err
				}
			}
		}
	}
	for _, item := range members {
		m := item.(map[string]any)
		if m["required"] == true && !matched[m["pattern"].(string)] {
			return fmt.Errorf("missing required archive member %s", m["pattern"])
		}
	}
	if session.ParentSessionID != "" && (!seen["restore-source/checkpoint.json"] || !seen["restore-source/state.jsonl"]) {
		return fmt.Errorf("missing restore-source evidence")
	}
	if session.OperationAPIVersion != "" && !seen["execution.json"] {
		return fmt.Errorf("missing execution evidence")
	}
	if bundle.Completeness.State == "complete" && (!seen["final-evaluation.json"] || bundle.Completeness.Truncated || seen["host-interruption.json"] || seen["transport-failure.json"] || seen["cleanup-pending.json"]) {
		return fmt.Errorf("incomplete evidence advertised as complete")
	}
	return nil
}

func verifyNativeRecord(pointer string, raw []byte) error {
	check := func(value any, expected string) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if digest.Bytes(encoded) != expected {
			return fmt.Errorf("native record digest mismatch")
		}
		return nil
	}
	switch pointer {
	case "#/components/schemas/snapshot.Manifest":
		var m snapshot.Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		return snapshot.VerifyManifest(m)
	case "#/components/schemas/snapshot.Checkpoint":
		var cp snapshot.Checkpoint
		if err := json.Unmarshal(raw, &cp); err != nil {
			return err
		}
		return snapshot.VerifyCheckpoint(cp)
	case "#/components/schemas/statejournal.Transaction":
		var tx statejournal.Transaction
		if err := json.Unmarshal(raw, &tx); err != nil {
			return err
		}
		for _, op := range tx.Operations {
			if err := statejournal.ValidateOperation(op); err != nil {
				return err
			}
		}
		h := tx.Hash
		tx.Hash = ""
		return check(tx, h)
	case "#/components/schemas/recording.Event":
		var e recording.Event
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		h := e.Hash
		e.Hash = ""
		return check(e, h)
	case "#/components/schemas/adaptive.EvidenceBundle":
		var b adaptive.EvidenceBundle
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		h := b.Digest
		b.Digest = ""
		return check(b, h)
	}
	return nil
}

// EvidenceMember exposes the same schema/hash check for focused negative fixtures.
func (v *Validator) EvidenceMember(model string, raw []byte) error {
	pointer := "#/components/schemas/" + model
	if err := v.Validate(pointer, bytes.TrimSpace(raw)); err != nil {
		return err
	}
	return verifyNativeRecord(pointer, raw)
}
