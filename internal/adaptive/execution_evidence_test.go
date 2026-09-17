package adaptive

import (
	"encoding/json"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
)

func TestExecutionEvidencePreservesUncertaintyAndAttribution(t *testing.T) {
	bundle := EvidenceBundle{APIVersion: EvidenceBundleVersion, SessionID: "session", Completeness: EvidenceCompleteness{State: "complete"}}
	state := ExecutionState{SessionID: "session", Owner: &ExecutionOwner{CampaignID: "campaign"}, Records: map[string]*OperationRecord{"lost": {State: "running"}}}
	result, err := BindExecutionEvidence(bundle, state)
	if err != nil {
		t.Fatal(err)
	}
	if result.CampaignID != "campaign" || result.Completeness.State != "partial" || len(result.CoverageGaps) != 2 {
		t.Fatalf("missing provenance/uncertainty: %+v", result)
	}
	claimed := result.Digest
	result.Digest = ""
	raw, _ := json.Marshal(result)
	if digest.Bytes(raw) != claimed {
		t.Fatal("evidence digest not recomputed")
	}
	bundle.CampaignID = "different"
	if _, err := BindExecutionEvidence(bundle, state); err == nil {
		t.Fatal("conflicting campaign accepted")
	}
}
