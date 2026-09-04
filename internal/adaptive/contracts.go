package adaptive

import "time"

const (
	AttemptContextVersion        = "interceptor.dev/attempt-context/v1alpha1"
	ObservationViewVersion       = "interceptor.dev/observation-view/v1alpha1"
	CapabilityManifestVersion    = "interceptor.dev/capability-manifest/v1alpha1"
	EvidenceBundleVersion        = "interceptor.dev/evidence-bundle/v1alpha1"
	ExecutionLeaseVersion        = "interceptor.dev/execution-lease/v1alpha1"
	ExecutionLeaseRequestVersion = "interceptor.dev/execution-lease-request/v1alpha1"
	CrossVMRequestVersion        = "interceptor.dev/cross-vm-request/v1alpha1"
)

type ArtifactDescriptor struct {
	Digest           string `json:"digest"`
	SizeBytes        int64  `json:"size_bytes"`
	MediaType        string `json:"media_type"`
	Canonicalization string `json:"canonicalization,omitempty"`
}

type AttemptContext struct {
	APIVersion            string             `json:"api_version"`
	CampaignID            string             `json:"campaign_id"`
	ThreadID              string             `json:"thread_id"`
	AttemptID             string             `json:"attempt_id"`
	ParentAttemptID       string             `json:"parent_attempt_id,omitempty"`
	Generation            uint64             `json:"generation"`
	AttemptIndex          uint64             `json:"attempt_index"`
	Payload               ArtifactDescriptor `json:"payload"`
	Generator             Generator          `json:"generator"`
	StrategyProvenanceRef string             `json:"strategy_provenance_ref,omitempty"`
	FeedbackProfile       string             `json:"feedback_profile"`
	CreatedAt             time.Time          `json:"created_at"`
	Digest                string             `json:"digest,omitempty"`
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
	State     string `json:"state"`
	ReceiptID string `json:"receipt_id,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
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
	APIVersion      string        `json:"api_version"`
	SessionID       string        `json:"session_id"`
	AttemptID       string        `json:"attempt_id"`
	FeedbackProfile string        `json:"feedback_profile"`
	Operation       OperationView `json:"operation"`
	TargetOutput    TargetOutput  `json:"target_output"`
	Observations    []Observation `json:"observations"`
	SessionRevision uint64        `json:"session_revision"`
}

type Limits struct {
	MaximumAttempts                int64 `json:"maximum_attempts,omitempty"`
	MaximumConcurrentInvocations   int64 `json:"maximum_concurrent_invocations,omitempty"`
	MaximumArtifactBytes           int64 `json:"maximum_artifact_bytes,omitempty"`
	MaximumWallSeconds             int64 `json:"maximum_wall_seconds,omitempty"`
	MaximumSnapshotCreations       int64 `json:"maximum_snapshot_creations,omitempty"`
	MaximumCumulativeSnapshotBytes int64 `json:"maximum_cumulative_snapshot_bytes,omitempty"`
}

type ExecutionLeaseRequest struct {
	APIVersion               string    `json:"api_version"`
	IdempotencyKey           string    `json:"idempotency_key"`
	OperatorVMInstanceID     string    `json:"operator_vm_instance_id"`
	CampaignID               string    `json:"campaign_id"`
	SessionID                string    `json:"session_id"`
	ScenarioBundleDigest     string    `json:"scenario_bundle_digest,omitempty"`
	CapabilityManifestDigest string    `json:"capability_manifest_digest"`
	EnvironmentDigest        string    `json:"environment_digest"`
	ApplicationDigest        string    `json:"application_digest"`
	FeedbackProfile          string    `json:"feedback_profile"`
	AllowedOperations        []string  `json:"allowed_operations"`
	Limits                   Limits    `json:"limits"`
	ExpiresAt                time.Time `json:"expires_at"`
}

type ExecutionLease struct {
	APIVersion               string    `json:"api_version"`
	LeaseID                  string    `json:"lease_id"`
	Principal                string    `json:"principal"`
	OperatorVMInstanceID     string    `json:"operator_vm_instance_id"`
	CampaignID               string    `json:"campaign_id"`
	SessionID                string    `json:"session_id"`
	ScenarioBundleDigest     string    `json:"scenario_bundle_digest,omitempty"`
	CapabilityManifestDigest string    `json:"capability_manifest_digest"`
	EnvironmentDigest        string    `json:"environment_digest"`
	ApplicationDigest        string    `json:"application_digest"`
	FeedbackProfile          string    `json:"feedback_profile"`
	AllowedOperations        []string  `json:"allowed_operations"`
	Limits                   Limits    `json:"limits"`
	IssuedAt                 time.Time `json:"issued_at"`
	ExpiresAt                time.Time `json:"expires_at"`
	Status                   string    `json:"status"`
}

type ExecutionLeaseResponse struct {
	Lease       ExecutionLease `json:"lease"`
	AccessToken string         `json:"access_token"`
	TokenType   string         `json:"token_type"`
}

type CrossVMRequest struct {
	APIVersion              string    `json:"api_version"`
	RequestID               string    `json:"request_id"`
	IdempotencyKey          string    `json:"idempotency_key"`
	LeaseID                 string    `json:"lease_id"`
	Operation               string    `json:"operation"`
	SessionID               string    `json:"session_id"`
	OperatorVMInstanceID    string    `json:"operator_vm_instance_id"`
	ExpectedSessionRevision uint64    `json:"expected_session_revision,omitempty"`
	AttemptID               string    `json:"attempt_id,omitempty"`
	AttemptContextDigest    string    `json:"attempt_context_digest,omitempty"`
	BodyDigest              string    `json:"body_digest"`
	Deadline                time.Time `json:"deadline"`
}
