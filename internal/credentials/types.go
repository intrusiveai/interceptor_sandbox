package credentials

import "time"

const (
	CredentialRefVersion = "interceptor.dev/host-credential-ref/v1alpha1"
	StoreProfileVersion  = "interceptor.dev/secret-store-profile/v1alpha1"
)

type Config struct {
	Profiles    []SecretStoreProfile `yaml:"profiles" json:"profiles"`
	Credentials []HostCredentialRef  `yaml:"credentials" json:"credentials"`
}

type SecretStoreProfile struct {
	APIVersion             string   `yaml:"api_version" json:"api_version"`
	Kind                   string   `yaml:"kind" json:"kind"`
	ID                     string   `yaml:"id" json:"id"`
	BackendKind            string   `yaml:"backend_kind" json:"backend_kind"`
	Region                 string   `yaml:"region,omitempty" json:"region,omitempty"`
	VaultURL               string   `yaml:"vault_url,omitempty" json:"vault_url,omitempty"`
	VaultNamespace         string   `yaml:"vault_namespace,omitempty" json:"vault_namespace,omitempty"`
	VaultTokenFile         string   `yaml:"vault_token_file,omitempty" json:"vault_token_file,omitempty"`
	VaultCACertificate     string   `yaml:"vault_ca_certificate,omitempty" json:"vault_ca_certificate,omitempty"`
	AllowedLocatorPrefixes []string `yaml:"allowed_locator_prefixes" json:"allowed_locator_prefixes"`
	TimeoutSeconds         int64    `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	MaxValueBytes          int64    `yaml:"max_value_bytes,omitempty" json:"max_value_bytes,omitempty"`
	MaxCacheTTLSeconds     int64    `yaml:"max_cache_ttl_seconds,omitempty" json:"max_cache_ttl_seconds,omitempty"`
}

type HostCredentialRef struct {
	APIVersion      string         `yaml:"api_version" json:"api_version"`
	Kind            string         `yaml:"kind" json:"kind"`
	CredentialID    string         `yaml:"credential_id" json:"credential_id"`
	StoreProfileID  string         `yaml:"store_profile_id" json:"store_profile_id"`
	Locator         Locator        `yaml:"locator" json:"locator"`
	Value           ValueSelection `yaml:"value" json:"value"`
	CacheTTLSeconds int64          `yaml:"cache_ttl_seconds,omitempty" json:"cache_ttl_seconds,omitempty"`
}

type Locator struct {
	BackendKind  string `yaml:"backend_kind" json:"backend_kind"`
	SecretID     string `yaml:"secret_id,omitempty" json:"secret_id,omitempty"`
	Region       string `yaml:"region,omitempty" json:"region,omitempty"`
	VersionID    string `yaml:"version_id,omitempty" json:"version_id,omitempty"`
	VersionStage string `yaml:"version_stage,omitempty" json:"version_stage,omitempty"`
	VaultURL     string `yaml:"vault_url,omitempty" json:"vault_url,omitempty"`
	SecretName   string `yaml:"secret_name,omitempty" json:"secret_name,omitempty"`
	Version      string `yaml:"version,omitempty" json:"version,omitempty"`
	Project      string `yaml:"project,omitempty" json:"project,omitempty"`
	Mount        string `yaml:"mount,omitempty" json:"mount,omitempty"`
	Path         string `yaml:"path,omitempty" json:"path,omitempty"`
}

type ValueSelection struct {
	Format string `yaml:"format" json:"format"`
	Field  string `yaml:"field,omitempty" json:"field,omitempty"`
}

type Resolution struct {
	CredentialID   string    `json:"credential_id"`
	ProfileID      string    `json:"profile_id"`
	BackendVersion string    `json:"backend_version,omitempty"`
	ResolvedAt     time.Time `json:"resolved_at"`
	Value          string    `json:"-"`
}
