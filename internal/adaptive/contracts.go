package adaptive

import (
	"encoding/json"
	"time"
)

const (
	OperationRequestVersion   = "interceptor.dev/operation-request/v1alpha2"
	AttemptContextVersion     = "interceptor.dev/attempt-context/v1alpha1"
	ObservationViewVersion    = "interceptor.dev/observation-view/v1alpha2"
	CapabilityManifestVersion = "interceptor.dev/capability-manifest/v1alpha3"
	EvidenceBundleVersion     = "interceptor.dev/evidence-bundle/v1alpha1"
)

type ArtifactDescriptor struct {
	Digest           string `json:"digest"`
	SizeBytes        int64  `json:"size_bytes"`
	MediaType        string `json:"media_type"`
	Canonicalization string `json:"canonicalization,omitempty"`
}

type AttemptContext struct {
	ObservationSelection  *ObservationSelection `json:"observation_selection,omitempty"`
	APIVersion            string                `json:"api_version"`
	CampaignID            string                `json:"campaign_id"`
	ThreadID              string                `json:"thread_id"`
	AttemptID             string                `json:"attempt_id"`
	ParentAttemptID       string                `json:"parent_attempt_id,omitempty"`
	Generation            uint64                `json:"generation"`
	AttemptIndex          uint64                `json:"attempt_index"`
	Payload               ArtifactDescriptor    `json:"payload"`
	Generator             Generator             `json:"generator"`
	StrategyProvenanceRef string                `json:"strategy_provenance_ref,omitempty"`
	FeedbackProfile       string                `json:"feedback_profile"`
	CreatedAt             time.Time             `json:"created_at"`
	Digest                string                `json:"digest,omitempty"`
}

type Generator struct {
	Kind          string `json:"kind"`
	ReleaseDigest string `json:"release_digest,omitempty"`
}

type Visibility string

const (
	TargetVisible  Visibility = "target_visible"
	HarnessVisible Visibility = "harness_visible"
	OperatorOnly   Visibility = "operator_only"
	Protected      Visibility = "protected"
)

type OperationView struct {
	ResponseCode int    `json:"response_code,omitempty"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	State        string `json:"state"`
	ReceiptID    string `json:"receipt_id,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
}
type TargetOutput struct {
	ArtifactDigest string `json:"artifact_digest,omitempty"`
	MediaType      string `json:"media_type,omitempty"`
	Inline         string `json:"inline,omitempty"`
}
type Observation struct {
	Kind       string     `json:"kind"`
	Visibility Visibility `json:"visibility"`
	Value      any        `json:"value,omitempty"`
}
type ObservationView struct {
	ReceiptID            string             `json:"receipt_id,omitempty"`
	CampaignID           string             `json:"campaign_id"`
	AttemptContextDigest string             `json:"attempt_context_digest"`
	TurnID               string             `json:"turn_id,omitempty"`
	CapturedAt           time.Time          `json:"captured_at"`
	WindowStart          time.Time          `json:"window_start"`
	WindowEnd            time.Time          `json:"window_end"`
	ThroughEventSeq      uint64             `json:"through_event_seq"`
	CollectionState      string             `json:"collection_state"`
	Categories           []FeedbackCategory `json:"categories"`
	Entries              []FeedbackEntry    `json:"entries"`
	Hash                 string             `json:"hash,omitempty"`
	APIVersion           string             `json:"api_version"`
	SessionID            string             `json:"session_id"`
	AttemptID            string             `json:"attempt_id"`
	FeedbackProfile      string             `json:"feedback_profile"`
	Operation            OperationView      `json:"operation"`
	TargetOutput         TargetOutput       `json:"target_output"`
	Observations         []Observation      `json:"observations"`
	SessionRevision      uint64             `json:"session_revision"`
}

// OperationRequest identifies a command within a campaign-associated session.
// WorkerInstanceID and RunRevision are caller attribution, never access grants.
type OperationRequest struct {
	APIVersion              string    `json:"api_version"`
	RequestID               string    `json:"request_id"`
	OperationID             string    `json:"operation_id"`
	Operation               string    `json:"operation"`
	SessionID               string    `json:"session_id"`
	CampaignID              string    `json:"campaign_id"`
	WorkerInstanceID        string    `json:"worker_instance_id"`
	RunRevision             uint64    `json:"run_revision"`
	ExpectedSessionRevision uint64    `json:"expected_session_revision,omitempty"`
	AttemptID               string    `json:"attempt_id,omitempty"`
	AttemptContextDigest    string    `json:"attempt_context_digest,omitempty"`
	BodyDigest              string    `json:"body_digest"`
	Deadline                time.Time `json:"deadline"`
}

type OperationEnvelope struct {
	Request OperationRequest `json:"request"`
	Body    json.RawMessage  `json:"body"`
}

type OwnerRequest struct {
	Action           string `json:"action"`
	SessionID        string `json:"session_id"`
	CampaignID       string `json:"campaign_id"`
	WorkerInstanceID string `json:"worker_instance_id,omitempty"` // Legacy body attribution; envelope is authoritative.
	AllowTargetStop  bool   `json:"allow_target_stop,omitempty"`
}
