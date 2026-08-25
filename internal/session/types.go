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
}

type Metadata struct {
	SchemaVersion     int       `json:"schema_version"`
	ID                string    `json:"id"`
	ParentCheckpoint  string    `json:"parent_checkpoint,omitempty"`
	EnvironmentPath   string    `json:"environment_path"`
	EnvironmentDigest string    `json:"environment_digest"`
	AppImage          string    `json:"app_image"`
	AppDigest         string    `json:"app_digest"`
	Phase             Phase     `json:"phase"`
	Generation        int       `json:"generation"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	Limits            Limits    `json:"limits"`
	Consumed          Consumed  `json:"consumed"`
	BridgeTransport   string    `json:"bridge_transport"`
	ContainerID       string    `json:"container_id,omitempty"`
	ApplicationURL    string    `json:"application_url,omitempty"`
	ControlURL        string    `json:"control_url,omitempty"`
	TerminationReason string    `json:"termination_reason,omitempty"`
	Taints            []string  `json:"taints"`
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
	Root         string
	Metadata     string
	Events       string
	Journal      string
	FileManifest string
	Checkpoints  string
	Generations  string
	Logs         string
	Sockets      string
}

func PathsFor(dataRoot, sessionID string) Paths {
	root := dataRoot + "/sessions/" + sessionID
	return Paths{
		Root: root, Metadata: root + "/session.json", Events: root + "/events.jsonl",
		Journal: root + "/state.jsonl", FileManifest: root + "/file-manifest.json",
		Checkpoints: root + "/checkpoints", Generations: root + "/generations",
		Logs: root + "/logs", Sockets: root + "/sockets",
	}
}
