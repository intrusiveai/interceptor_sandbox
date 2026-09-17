package adaptive

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evaluation"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

type EvidenceCompleteness struct {
	State         string `json:"state"`
	StartSequence uint64 `json:"start_sequence"`
	EndSequence   uint64 `json:"end_sequence"`
	Truncated     bool   `json:"truncated"`
}
type AttemptEvidence struct {
	AttemptID       string   `json:"attempt_id"`
	ParentAttemptID string   `json:"parent_attempt_id,omitempty"`
	PayloadDigest   string   `json:"payload_digest"`
	ContextDigest   string   `json:"context_digest"`
	EventSequences  []uint64 `json:"event_sequences"`
}
type SnapshotEvidence struct {
	SnapshotID         string    `json:"snapshot_id"`
	Digest             string    `json:"digest"`
	ParentSnapshot     string    `json:"parent_snapshot,omitempty"`
	CanonicalSizeBytes int64     `json:"canonical_size_bytes"`
	EventSequence      uint64    `json:"event_sequence"`
	CreatedAt          time.Time `json:"created_at"`
	Status             string    `json:"status"`
}
type RestoreEvidence struct {
	SourceSessionID    string `json:"source_session_id"`
	SnapshotID         string `json:"snapshot_id"`
	ResultingSessionID string `json:"resulting_session_id"`
}
type EvidenceBundle struct {
	APIVersion               string                       `json:"api_version"`
	Kind                     string                       `json:"kind"`
	CampaignID               string                       `json:"campaign_id,omitempty"`
	SessionID                string                       `json:"session_id"`
	EnvironmentDigest        string                       `json:"environment_digest"`
	ApplicationDigest        string                       `json:"application_digest"`
	CapabilityManifestDigest string                       `json:"capability_manifest_digest"`
	InterceptorReleaseDigest string                       `json:"interceptor_release_digest"`
	FeedbackProfile          string                       `json:"feedback_profile"`
	Completeness             EvidenceCompleteness         `json:"completeness"`
	Attempts                 []AttemptEvidence            `json:"attempts"`
	Snapshots                []SnapshotEvidence           `json:"snapshots"`
	Restores                 []RestoreEvidence            `json:"restores"`
	ArtifactRefs             []ArtifactDescriptor         `json:"artifact_refs"`
	Oracles                  []evaluation.OracleResult    `json:"oracles"`
	Injections               []evaluation.InjectionResult `json:"injections"`
	CoverageGaps             []string                     `json:"coverage_gaps"`
	CreatedAt                time.Time                    `json:"created_at"`
	Digest                   string                       `json:"digest"`
}

func BuildEvidenceBundle(metadata session.Metadata, attempts []AttemptContext, artifacts []ArtifactDescriptor, events []recording.Event, checkpoints []snapshot.Checkpoint, report evaluation.Report, evaluationComplete bool) (EvidenceBundle, error) {
	state := "complete"
	if !evaluationComplete {
		state = "partial"
	}
	bundle := EvidenceBundle{APIVersion: EvidenceBundleVersion, Kind: "EvidenceBundle", SessionID: metadata.ID, EnvironmentDigest: metadata.EnvironmentDigest, ApplicationDigest: metadata.AppDigest, CapabilityManifestDigest: metadata.CapabilityManifestDigest, InterceptorReleaseDigest: digest.Bytes([]byte(buildinfo.Version)), FeedbackProfile: metadata.FeedbackProfile, Completeness: EvidenceCompleteness{State: state, StartSequence: 1, EndSequence: uint64(len(events)), Truncated: !evaluationComplete}, Attempts: []AttemptEvidence{}, Snapshots: []SnapshotEvidence{}, Restores: []RestoreEvidence{}, ArtifactRefs: append([]ArtifactDescriptor{}, artifacts...), Oracles: append([]evaluation.OracleResult{}, report.Oracles...), Injections: append([]evaluation.InjectionResult{}, report.Injections...), CoverageGaps: append([]string{}, report.Coverage...), CreatedAt: metadata.UpdatedAt.UTC()}
	if len(events) == 0 {
		bundle.Completeness.StartSequence = 0
	}
	byAttempt := map[string]int{}
	for _, attempt := range attempts {
		if bundle.CampaignID == "" {
			bundle.CampaignID = attempt.CampaignID
		}
		item := AttemptEvidence{AttemptID: attempt.AttemptID, ParentAttemptID: attempt.ParentAttemptID, PayloadDigest: attempt.Payload.Digest, ContextDigest: attempt.Digest, EventSequences: []uint64{}}
		bundle.Attempts = append(bundle.Attempts, item)
		byAttempt[item.AttemptID] = len(bundle.Attempts) - 1
	}
	for _, event := range events {
		if index, ok := byAttempt[event.AttemptID]; ok {
			bundle.Attempts[index].EventSequences = append(bundle.Attempts[index].EventSequences, event.Seq)
		}
	}
	for _, checkpoint := range checkpoints {
		bundle.Snapshots = append(bundle.Snapshots, SnapshotEvidence{SnapshotID: checkpoint.ID, Digest: checkpoint.Hash, ParentSnapshot: checkpoint.ParentCheckpoint, CanonicalSizeBytes: checkpoint.CanonicalSizeBytes, EventSequence: checkpoint.EventSeq, CreatedAt: checkpoint.CreatedAt, Status: checkpoint.Status})
	}
	if metadata.ParentCheckpoint != "" {
		bundle.Restores = append(bundle.Restores, RestoreEvidence{SourceSessionID: metadata.ParentSessionID, SnapshotID: metadata.ParentCheckpoint, ResultingSessionID: metadata.ID})
	}
	sort.Slice(bundle.Attempts, func(i, j int) bool { return bundle.Attempts[i].AttemptID < bundle.Attempts[j].AttemptID })
	sort.Slice(bundle.ArtifactRefs, func(i, j int) bool { return bundle.ArtifactRefs[i].Digest < bundle.ArtifactRefs[j].Digest })
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return EvidenceBundle{}, err
	}
	bundle.Digest = digest.Bytes(encoded)
	return bundle, nil
}

// BindExecutionEvidence adds owner attribution and explicit uncertainty using
// existing EvidenceBundle fields, preserving readability of historical bundles.
func BindExecutionEvidence(bundle EvidenceBundle, state ExecutionState) (EvidenceBundle, error) {
	if bundle.SessionID != state.SessionID {
		return EvidenceBundle{}, errors.New("execution evidence session mismatch")
	}
	if state.Owner != nil {
		if bundle.CampaignID != "" && bundle.CampaignID != state.Owner.CampaignID {
			return EvidenceBundle{}, errors.New("execution evidence campaign mismatch")
		}
		bundle.CampaignID = state.Owner.CampaignID
		if state.Owner.ClosedAt == nil {
			bundle.CoverageGaps = append(bundle.CoverageGaps, "execution_closure_unconfirmed")
			bundle.Completeness.State = "partial"
		}
	}
	for id, record := range state.Records {
		if record.State == "running" || record.State == "unknown" {
			bundle.CoverageGaps = append(bundle.CoverageGaps, "operation_outcome_unknown:"+id)
			bundle.Completeness.State = "partial"
		}
	}
	sort.Strings(bundle.CoverageGaps)
	bundle.Digest = ""
	raw, err := json.Marshal(bundle)
	if err != nil {
		return EvidenceBundle{}, err
	}
	bundle.Digest = digest.Bytes(raw)
	return bundle, nil
}
