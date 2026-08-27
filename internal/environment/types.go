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
	Identity         Identity          `yaml:"identity,omitempty" json:"identity,omitempty"`
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
	Image       string               `yaml:"image" json:"image"`
	Launch      Launch               `yaml:"launch" json:"launch"`
	Interface   ApplicationInterface `yaml:"interface" json:"interface"`
	Persistence Persistence          `yaml:"persistence" json:"persistence"`
}

type Launch struct {
	Command     []string          `yaml:"command" json:"command"`
	Workdir     string            `yaml:"workdir" json:"workdir"`
	Environment map[string]string `yaml:"environment,omitempty" json:"environment,omitempty"`
}

type ApplicationInterface struct {
	Adapter          string                 `yaml:"kind" json:"kind"`
	StartupTimeoutMS int64                  `yaml:"startup_timeout_ms,omitempty" json:"startup_timeout_ms,omitempty"`
	TurnTimeoutMS    int64                  `yaml:"turn_timeout_ms,omitempty" json:"turn_timeout_ms,omitempty"`
	Operations       []ApplicationOperation `yaml:"operations" json:"operations"`
	HTTP             *HTTPInterface         `yaml:"http,omitempty" json:"http,omitempty"`
	Command          *CommandInterface      `yaml:"command,omitempty" json:"command,omitempty"`
	Stdio            *StdioInterface        `yaml:"stdio,omitempty" json:"stdio,omitempty"`
	External         *ExternalInterface     `yaml:"external,omitempty" json:"external,omitempty"`
	Code             *CodeInterface         `yaml:"code,omitempty" json:"code,omitempty"`
}

type ApplicationOperation struct {
	ID        string            `yaml:"id" json:"id"`
	Method    string            `yaml:"method,omitempty" json:"method,omitempty"`
	Path      string            `yaml:"path,omitempty" json:"path,omitempty"`
	Arguments []ArgumentBinding `yaml:"arguments,omitempty" json:"arguments,omitempty"`
}

type ArgumentBinding struct {
	Literal string `yaml:"literal,omitempty" json:"literal,omitempty"`
	From    string `yaml:"from,omitempty" json:"from,omitempty"`
}

type HTTPInterface struct {
	Listen     string `yaml:"listen" json:"listen"`
	HealthPath string `yaml:"health_path,omitempty" json:"health_path,omitempty"`
}

type CommandInterface struct {
	Stdin           string `yaml:"stdin,omitempty" json:"stdin,omitempty"`
	OutputMediaType string `yaml:"output_media_type,omitempty" json:"output_media_type,omitempty"`
	SuccessCodes    []int  `yaml:"success_exit_codes,omitempty" json:"success_exit_codes,omitempty"`
	Concurrency     int    `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
}

type StdioInterface struct {
	Transport       string `yaml:"transport,omitempty" json:"transport,omitempty"`
	InputSuffix     string `yaml:"input_suffix,omitempty" json:"input_suffix,omitempty"`
	ResponseMode    string `yaml:"response_mode,omitempty" json:"response_mode,omitempty"`
	Delimiter       string `yaml:"delimiter,omitempty" json:"delimiter,omitempty"`
	ReadyPattern    string `yaml:"ready_pattern,omitempty" json:"ready_pattern,omitempty"`
	TerminalPattern string `yaml:"terminal_pattern,omitempty" json:"terminal_pattern,omitempty"`
	StripEcho       bool   `yaml:"strip_echo,omitempty" json:"strip_echo,omitempty"`
	StripTerminal   bool   `yaml:"strip_terminal,omitempty" json:"strip_terminal,omitempty"`
	IdleMS          int64  `yaml:"idle_ms,omitempty" json:"idle_ms,omitempty"`
	OutputMediaType string `yaml:"output_media_type,omitempty" json:"output_media_type,omitempty"`
}

type ExternalInterface struct {
	Protocol    string         `yaml:"protocol" json:"protocol"`
	Config      map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
	ConfigAsset string         `yaml:"config_asset,omitempty" json:"config_asset,omitempty"`
	Concurrency int            `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
}

type CodeInterface struct {
	Language      string         `yaml:"language" json:"language"`
	Module        string         `yaml:"module" json:"module"`
	Initialize    string         `yaml:"initialize,omitempty" json:"initialize,omitempty"`
	Invoke        string         `yaml:"invoke" json:"invoke"`
	InputMode     string         `yaml:"input_mode,omitempty" json:"input_mode,omitempty"`
	InitArguments map[string]any `yaml:"init_arguments,omitempty" json:"init_arguments,omitempty"`
	Concurrency   int            `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
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
	ID            string                `yaml:"id" json:"id"`
	Kind          string                `yaml:"kind" json:"kind"`
	Listen        string                `yaml:"listen" json:"listen"`
	Hostnames     []string              `yaml:"hostnames,omitempty" json:"hostnames,omitempty"`
	Seed          string                `yaml:"seed,omitempty" json:"seed,omitempty"`
	Routes        string                `yaml:"routes,omitempty" json:"routes,omitempty"`
	Fixtures      string                `yaml:"fixtures,omitempty" json:"fixtures,omitempty"`
	Endpoint      string                `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Tools         string                `yaml:"tools,omitempty" json:"tools,omitempty"`
	Options       map[string]any        `yaml:"options,omitempty" json:"options,omitempty"`
	Authorization *ServiceAuthorization `yaml:"authorization,omitempty" json:"authorization,omitempty"`
}

// Identity describes the synthetic principals and credential issuer available
// to the application. All secrets and credentials produced from this section
// are local simulation values and must not be trusted outside the sandbox.
type Identity struct {
	Issuers     []IdentityIssuer     `yaml:"issuers,omitempty" json:"issuers,omitempty"`
	Principals  []IdentityPrincipal  `yaml:"principals,omitempty" json:"principals,omitempty"`
	Grants      []IdentityGrant      `yaml:"grants,omitempty" json:"grants,omitempty"`
	Delegations []IdentityDelegation `yaml:"delegations,omitempty" json:"delegations,omitempty"`
	Deliveries  []IdentityDelivery   `yaml:"deliveries,omitempty" json:"deliveries,omitempty"`
	Inbound     *InboundIdentity     `yaml:"inbound,omitempty" json:"inbound,omitempty"`
}

type IdentityIssuer struct {
	ID                    string   `yaml:"id" json:"id"`
	Kind                  string   `yaml:"kind" json:"kind"`
	Listen                string   `yaml:"listen" json:"listen"`
	Hostnames             []string `yaml:"hostnames,omitempty" json:"hostnames,omitempty"`
	IssuerURL             string   `yaml:"issuer_url" json:"issuer_url"`
	AccessTokenTTLSeconds int64    `yaml:"access_token_ttl_seconds,omitempty" json:"access_token_ttl_seconds,omitempty"`
}

type IdentityPrincipal struct {
	ID           string         `yaml:"id" json:"id"`
	Kind         string         `yaml:"kind" json:"kind"`
	Issuer       string         `yaml:"issuer" json:"issuer"`
	ClientID     string         `yaml:"client_id,omitempty" json:"client_id,omitempty"`
	ClientSecret string         `yaml:"client_secret,omitempty" json:"client_secret,omitempty"`
	Claims       map[string]any `yaml:"claims,omitempty" json:"claims,omitempty"`
}

type IdentityGrant struct {
	ID        string   `yaml:"id" json:"id"`
	Principal string   `yaml:"principal" json:"principal"`
	Service   string   `yaml:"service" json:"service"`
	Scopes    []string `yaml:"scopes" json:"scopes"`
}

type IdentityDelegation struct {
	ID      string   `yaml:"id" json:"id"`
	Agent   string   `yaml:"agent" json:"agent"`
	User    string   `yaml:"user" json:"user"`
	Service string   `yaml:"service" json:"service"`
	Scopes  []string `yaml:"scopes" json:"scopes"`
}

type IdentityDelivery struct {
	ID                            string `yaml:"id" json:"id"`
	Kind                          string `yaml:"kind" json:"kind"`
	Issuer                        string `yaml:"issuer" json:"issuer"`
	Principal                     string `yaml:"principal" json:"principal"`
	Audience                      string `yaml:"audience,omitempty" json:"audience,omitempty"`
	Path                          string `yaml:"path,omitempty" json:"path,omitempty"`
	EnvironmentVariable           string `yaml:"environment_variable,omitempty" json:"environment_variable,omitempty"`
	ClientIDEnvironment           string `yaml:"client_id_environment,omitempty" json:"client_id_environment,omitempty"`
	ClientSecretEnvironment       string `yaml:"client_secret_environment,omitempty" json:"client_secret_environment,omitempty"`
	TokenURLEnvironment           string `yaml:"token_url_environment,omitempty" json:"token_url_environment,omitempty"`
	EndpointEnvironment           string `yaml:"endpoint_environment,omitempty" json:"endpoint_environment,omitempty"`
	AuthorizationTokenEnvironment string `yaml:"authorization_token_environment,omitempty" json:"authorization_token_environment,omitempty"`
}

type InboundIdentity struct {
	Issuer            string `yaml:"issuer" json:"issuer"`
	Audience          string `yaml:"audience" json:"audience"`
	ApplicationHeader string `yaml:"application_header,omitempty" json:"application_header,omitempty"`
}

type ServiceAuthorization struct {
	Issuer         string   `yaml:"issuer" json:"issuer"`
	Audience       string   `yaml:"audience" json:"audience"`
	RequiredScopes []string `yaml:"required_scopes,omitempty" json:"required_scopes,omitempty"`
	AllowDelegated bool     `yaml:"allow_delegated,omitempty" json:"allow_delegated,omitempty"`
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
