package session

import "time"

type Phase string

const (
	Creating  Phase = "creating"
	Starting  Phase = "starting"
	Running   Phase = "running"
	Quiescing Phase = "quiescing"
	Stopping  Phase = "stopping"
	Stopped   Phase = "stopped"
	Error     Phase = "error"
)

type Consumed struct {
	Turns            int64 `json:"turns"`
	Tokens           int64 `json:"tokens"`
	SnapshotAttempts int64 `json:"snapshot_attempts"`
	SnapshotBytes    int64 `json:"snapshot_bytes"`
}

type Metadata struct {
	CampaignID               string    `json:"campaign_id,omitempty"`
	SpoolMaxBytes            int64     `json:"spool_max_bytes,omitempty"`
	OperationAPIVersion      string    `json:"operation_api_version,omitempty"`
	SchemaVersion            int       `json:"schema_version"`
	ID                       string    `json:"id"`
	ParentSessionID          string    `json:"parent_session_id,omitempty"`
	ParentCheckpoint         string    `json:"parent_checkpoint,omitempty"`
	EnvironmentPath          string    `json:"environment_path"`
	EnvironmentDigest        string    `json:"environment_digest"`
	AppImage                 string    `json:"app_image"`
	AppDigest                string    `json:"app_digest"`
	CapabilityManifestDigest string    `json:"capability_manifest_digest,omitempty"`
	FeedbackProfile          string    `json:"feedback_profile,omitempty"`
	Revision                 uint64    `json:"revision"`
	Phase                    Phase     `json:"phase"`
	Generation               int       `json:"generation"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
	Limits                   Limits    `json:"limits"`
	Consumed                 Consumed  `json:"consumed"`
	BridgeTransport          string    `json:"bridge_transport"`
	ContainerID              string    `json:"container_id,omitempty"`
	ApplicationURL           string    `json:"application_url,omitempty"`
	ControlURL               string    `json:"control_url,omitempty"`
	TerminationReason        string    `json:"termination_reason,omitempty"`
	Taints                   []string  `json:"taints"`
}

type Limits struct {
	MaxTurns       int64  `json:"max_turns"`
	MaxTokens      int64  `json:"max_tokens"`
	MaxWallSeconds int64  `json:"max_wall_seconds"`
	MaxSnapshots   int64  `json:"max_snapshots"`
	MaxBlobBytes   int64  `json:"max_blob_bytes"`
	MaxPIDs        int64  `json:"max_pids"`
	MemoryBytes    int64  `json:"memory_bytes"`
	CPUs           string `json:"cpus"`
}

type Paths struct {
	Blobs        string
	Exports      string
	Root         string
	Metadata     string
	Events       string
	Journal      string
	Evaluation   string
	FileManifest string
	Checkpoints  string
	Generations  string
	Logs         string
	Sockets      string
}

func PathsFor(dataRoot, sessionID string) Paths {
	root := dataRoot + "/sessions/" + sessionID
	return Paths{
		Blobs: root + "/blobs", Exports: root + "/exports",
		Root: root, Metadata: root + "/session.json", Events: root + "/events.jsonl",
		Journal: root + "/state.jsonl", Evaluation: root + "/final-evaluation.json", FileManifest: root + "/file-manifest.json",
		Checkpoints: root + "/checkpoints", Generations: root + "/generations",
		Logs: root + "/logs", Sockets: root + "/sockets",
	}
}
