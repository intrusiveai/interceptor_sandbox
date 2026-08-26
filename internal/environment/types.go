package environment

import "time"

const (
	APIVersion    = "interceptor.dev/v1alpha1"
	Kind          = "Environment"
	BlueprintFile = "environment.yaml"
	LockFile      = "environment.lock.json"
)

type Blueprint struct {
	APIVersion       string            `yaml:"api_version" json:"api_version"`
	Kind             string            `yaml:"kind" json:"kind"`
	Metadata         Metadata          `yaml:"metadata" json:"metadata"`
	Application      Application       `yaml:"application" json:"application"`
	Organization     Organization      `yaml:"organization,omitempty" json:"organization,omitempty"`
	Time             TimeConfig        `yaml:"time,omitempty" json:"time,omitempty"`
	Filesystem       Filesystem        `yaml:"filesystem,omitempty" json:"filesystem,omitempty"`
	Services         []Service         `yaml:"services,omitempty" json:"services,omitempty"`
	Credentials      []Credential      `yaml:"credentials,omitempty" json:"credentials,omitempty"`
	Canaries         []Canary          `yaml:"canaries,omitempty" json:"canaries,omitempty"`
	AttackerServices []AttackerService `yaml:"attacker_services,omitempty" json:"attacker_services,omitempty"`
	Oracles          []Oracle          `yaml:"oracles,omitempty" json:"oracles,omitempty"`
	Limits           Limits            `yaml:"limits,omitempty" json:"limits,omitempty"`
}

type Metadata struct {
	Name        string    `yaml:"name" json:"name"`
	Version     string    `yaml:"version" json:"version"`
	Description string    `yaml:"description,omitempty" json:"description,omitempty"`
	ReviewedBy  string    `yaml:"reviewed_by,omitempty" json:"reviewed_by,omitempty"`
	ReviewedAt  time.Time `yaml:"reviewed_at,omitempty" json:"reviewed_at,omitempty"`
}

type Application struct {
	Image       string      `yaml:"image" json:"image"`
	Launch      Launch      `yaml:"launch" json:"launch"`
	IO          IOBinding   `yaml:"io" json:"io"`
	Persistence Persistence `yaml:"persistence" json:"persistence"`
}

type Launch struct {
	Command     []string          `yaml:"command" json:"command"`
	Workdir     string            `yaml:"workdir" json:"workdir"`
	Environment map[string]string `yaml:"environment,omitempty" json:"environment,omitempty"`
}

type IOBinding struct {
	Adapter           string        `yaml:"adapter" json:"adapter"`
	ApplicationListen string        `yaml:"application_listen" json:"application_listen"`
	ServedRoutes      []ServedRoute `yaml:"served_routes" json:"served_routes"`
	HealthPath        string        `yaml:"health_path,omitempty" json:"health_path,omitempty"`
	StartupTimeoutMS  int64         `yaml:"startup_timeout_ms,omitempty" json:"startup_timeout_ms,omitempty"`
	TurnTimeoutMS     int64         `yaml:"turn_timeout_ms,omitempty" json:"turn_timeout_ms,omitempty"`
}

type ServedRoute struct {
	Method string `yaml:"method" json:"method"`
	Path   string `yaml:"path" json:"path"`
}

type Persistence struct {
	SnapshotCapable bool          `yaml:"snapshot_capable" json:"snapshot_capable"`
	MutableRoots    []string      `yaml:"mutable_roots,omitempty" json:"mutable_roots,omitempty"`
	Quiesce         QuiesceConfig `yaml:"quiesce,omitempty" json:"quiesce,omitempty"`
}

type QuiesceConfig struct {
	Kind      string   `yaml:"kind,omitempty" json:"kind,omitempty"`
	Method    string   `yaml:"method,omitempty" json:"method,omitempty"`
	URL       string   `yaml:"url,omitempty" json:"url,omitempty"`
	Signal    string   `yaml:"signal,omitempty" json:"signal,omitempty"`
	Command   []string `yaml:"command,omitempty" json:"command,omitempty"`
	ReadyFile string   `yaml:"ready_file,omitempty" json:"ready_file,omitempty"`
	ReadyURL  string   `yaml:"ready_url,omitempty" json:"ready_url,omitempty"`
	TimeoutMS int64    `yaml:"timeout_ms,omitempty" json:"timeout_ms,omitempty"`
}

type Organization struct {
	Name     string `yaml:"name,omitempty" json:"name,omitempty"`
	Domain   string `yaml:"domain,omitempty" json:"domain,omitempty"`
	Industry string `yaml:"industry,omitempty" json:"industry,omitempty"`
}

type TimeConfig struct {
	Base time.Time `yaml:"base,omitempty" json:"base,omitempty"`
	Mode string    `yaml:"mode,omitempty" json:"mode,omitempty"`
}

type Filesystem struct {
	Seed []SeedEntry `yaml:"seed,omitempty" json:"seed,omitempty"`
}

type SeedEntry struct {
	Source   string `yaml:"source" json:"source"`
	Target   string `yaml:"target" json:"target"`
	ReadOnly bool   `yaml:"read_only" json:"read_only"`
}

type Service struct {
	ID        string         `yaml:"id" json:"id"`
	Kind      string         `yaml:"kind" json:"kind"`
	Listen    string         `yaml:"listen" json:"listen"`
	Hostnames []string       `yaml:"hostnames,omitempty" json:"hostnames,omitempty"`
	Seed      string         `yaml:"seed,omitempty" json:"seed,omitempty"`
	Routes    string         `yaml:"routes,omitempty" json:"routes,omitempty"`
	Fixtures  string         `yaml:"fixtures,omitempty" json:"fixtures,omitempty"`
	Endpoint  string         `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Tools     string         `yaml:"tools,omitempty" json:"tools,omitempty"`
	Options   map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}

type Credential struct {
	ID                  string `yaml:"id" json:"id"`
	EnvironmentVariable string `yaml:"environment_variable" json:"environment_variable"`
	Value               string `yaml:"value" json:"value"`
	AcceptedBy          string `yaml:"accepted_by" json:"accepted_by"`
}

type Canary struct {
	ID       string `yaml:"id" json:"id"`
	Value    string `yaml:"value" json:"value"`
	Location string `yaml:"location" json:"location"`
	Proves   string `yaml:"proves" json:"proves"`
}

type AttackerService struct {
	ID              string         `yaml:"id" json:"id"`
	Kind            string         `yaml:"kind" json:"kind"`
	Listen          string         `yaml:"listen" json:"listen"`
	Hostnames       []string       `yaml:"hostnames,omitempty" json:"hostnames,omitempty"`
	Response        SinkResponse   `yaml:"response" json:"response"`
	MaxCaptureBytes int64          `yaml:"max_capture_bytes,omitempty" json:"max_capture_bytes,omitempty"`
	Options         map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}

type SinkResponse struct {
	Status int            `yaml:"status" json:"status"`
	JSON   map[string]any `yaml:"json,omitempty" json:"json,omitempty"`
	Body   string         `yaml:"body,omitempty" json:"body,omitempty"`
}

type Oracle struct {
	ID      string         `yaml:"id" json:"id"`
	Kind    string         `yaml:"kind" json:"kind"`
	Service string         `yaml:"service,omitempty" json:"service,omitempty"`
	Value   string         `yaml:"value,omitempty" json:"value,omitempty"`
	Match   map[string]any `yaml:"match,omitempty" json:"match,omitempty"`
}

type Limits struct {
	MaxTurns       int64  `yaml:"max_turns,omitempty" json:"max_turns,omitempty"`
	MaxTokens      int64  `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`
	MaxWallSeconds int64  `yaml:"max_wall_seconds,omitempty" json:"max_wall_seconds,omitempty"`
	MaxSnapshots   int64  `yaml:"max_snapshots,omitempty" json:"max_snapshots,omitempty"`
	MaxBlobBytes   int64  `yaml:"max_blob_bytes,omitempty" json:"max_blob_bytes,omitempty"`
	MaxPIDs        int64  `yaml:"max_pids,omitempty" json:"max_pids,omitempty"`
	MemoryBytes    int64  `yaml:"memory_bytes,omitempty" json:"memory_bytes,omitempty"`
	CPUs           string `yaml:"cpus,omitempty" json:"cpus,omitempty"`
}

func (l *Limits) ApplyDefaults() {
	if l.MaxTurns == 0 {
		l.MaxTurns = 200
	}
	if l.MaxTokens == 0 {
		l.MaxTokens = 5_000_000
	}
	if l.MaxWallSeconds == 0 {
		l.MaxWallSeconds = 3_600
	}
	if l.MaxSnapshots == 0 {
		l.MaxSnapshots = 20
	}
	if l.MaxBlobBytes == 0 {
		l.MaxBlobBytes = 100 << 20
	}
	if l.MaxPIDs == 0 {
		l.MaxPIDs = 256
	}
	if l.MemoryBytes == 0 {
		l.MemoryBytes = 2 << 30
	}
	if l.CPUs == "" {
		l.CPUs = "2"
	}
}

type Asset struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	Type   string `json:"type"`
	Target string `json:"target,omitempty"`
}

type Lock struct {
	SchemaVersion     int       `json:"schema_version"`
	Name              string    `json:"name"`
	Version           string    `json:"version"`
	ApplicationDigest string    `json:"application_digest"`
	Assets            []Asset   `json:"assets"`
	EnvironmentDigest string    `json:"environment_digest"`
	SealedAt          time.Time `json:"sealed_at"`
}

type ValidationReport struct {
	Blueprint *Blueprint `json:"-"`
	Warnings  []string   `json:"warnings"`
	Assets    []Asset    `json:"assets"`
}
