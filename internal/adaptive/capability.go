package adaptive

import (
	"encoding/json"
	"errors"
	"sort"

	"github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo"
	"github.com/intrusive-ai/interceptor-sandbox/internal/delivery"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

type FeedbackCapability struct {
	ViewVersion       string   `json:"view_version"`
	SelectionModes    []string `json:"selection_modes"`
	Kinds             []string `json:"kinds"`
	MaximumChunkBytes int      `json:"maximum_chunk_bytes"`
	MaximumEntries    int      `json:"maximum_entries"`
}

type CapabilityManifest struct {
	Feedback              FeedbackCapability        `json:"feedback"`
	DeliverySchemaProfile string                    `json:"delivery_schema_profile,omitempty"`
	APIVersion            string                    `json:"api_version"`
	Kind                  string                    `json:"kind"`
	InterceptorRelease    string                    `json:"interceptor_release"`
	EnvironmentDigest     string                    `json:"environment_digest"`
	ApplicationDigest     string                    `json:"application_digest"`
	Target                TargetCapability          `json:"target"`
	FeedbackProfiles      []string                  `json:"feedback_profiles"`
	Operations            []OperationCapability     `json:"operations"`
	CrossVMOperations     []string                  `json:"cross_vm_operations"`
	InjectionSurfaces     []string                  `json:"injection_surfaces"`
	InjectionProfiles     []InjectionProfile        `json:"injection_profiles"`
	Services              []ServiceCapability       `json:"services"`
	FileNamespaces        []FileNamespaceCapability `json:"file_namespaces,omitempty"`
	Oracles               []string                  `json:"oracle_types"`
	Network               NetworkCapability         `json:"network"`
	SnapshotCapable       bool                      `json:"snapshot_capable"`
	Limits                CapabilityLimits          `json:"limits"`
	Digest                string                    `json:"digest"`
}
type TargetCapability struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Interface string `json:"interface"`
}
type OperationCapability struct {
	Delivery          *delivery.Contract `json:"delivery,omitempty"`
	DeliveryStatus    string             `json:"delivery_status,omitempty"`
	MaximumInputBytes int64              `json:"maximum_input_bytes,omitempty"`
	ID                string             `json:"id"`
	Method            string             `json:"method,omitempty"`
	Path              string             `json:"path,omitempty"`
}
type InjectionProfile struct {
	Surface        string   `json:"surface"`
	Scopes         []string `json:"scopes"`
	Placements     []string `json:"placements"`
	SelectorFields []string `json:"selector_fields"`
	Carriers       []string `json:"carriers"`
}
type DatabaseTableCapability struct {
	ID          string   `json:"id"`
	Keys        []string `json:"keys"`
	Fields      []string `json:"fields"`
	AllowCreate bool     `json:"allow_create"`
	MaxRecords  int      `json:"max_records,omitempty"`
}
type ServiceCapability struct {
	DatabaseTables       []DatabaseTableCapability    `json:"database_tables,omitempty"`
	Implementation       string                       `json:"implementation,omitempty"`
	Endpoints            []delivery.Endpoint          `json:"endpoints,omitempty"`
	ID                   string                       `json:"id"`
	Kind                 string                       `json:"kind"`
	Role                 string                       `json:"role"`
	MCP                  bool                         `json:"mcp,omitempty"`
	ResponseMode         string                       `json:"response_mode,omitempty"`
	Transports           []string                     `json:"transports"`
	InjectionSurfaces    []string                     `json:"injection_surfaces,omitempty"`
	CreatableCollections []ResourceCreationCapability `json:"creatable_collections,omitempty"`
}
type ResourceCreationCapability struct {
	Collection   string `json:"collection"`
	MaxResources int    `json:"max_resources"`
}
type FileNamespaceCapability struct {
	ID           string `json:"id"`
	AllowCreate  bool   `json:"allow_create"`
	MaxFiles     int    `json:"max_files,omitempty"`
	MaxFileBytes int64  `json:"max_file_bytes,omitempty"`
}
type NetworkCapability struct {
	DNS   DNSCapability `json:"dns"`
	HTTP  bool          `json:"http_frontdoor"`
	HTTPS TLSCapability `json:"https_frontdoor"`
}
type DNSCapability struct {
	Enabled        bool     `json:"enabled"`
	RecordTypes    []string `json:"record_types,omitempty"`
	UnknownPolicy  string   `json:"unknown_policy,omitempty"`
	Upstream       bool     `json:"upstream_forwarding"`
	MaximumQueries int64    `json:"maximum_queries,omitempty"`
}
type TLSCapability struct {
	Enabled        bool   `json:"enabled"`
	TrustMode      string `json:"trust_mode,omitempty"`
	MinimumVersion string `json:"minimum_version,omitempty"`
}
type CapabilityLimits struct {
	MaximumAttempts      int   `json:"maximum_attempts"`
	MaximumArtifacts     int   `json:"maximum_artifacts"`
	MaximumArtifactBytes int64 `json:"maximum_artifact_bytes"`
	MaximumInvocations   int64 `json:"maximum_invocations"`
	MaximumSnapshots     int64 `json:"maximum_snapshots"`
}

func BuildCapabilityManifest(blueprint *environment.Blueprint, environmentDigest, applicationDigest string) (CapabilityManifest, error) {
	m := CapabilityManifest{Feedback: FeedbackCapability{ViewVersion: ObservationViewVersion, SelectionModes: []string{"all-permitted", "selected"}, Kinds: append([]string(nil), FeedbackKinds...), MaximumChunkBytes: MaxFeedbackChunk, MaximumEntries: MaxFeedbackEntries}, APIVersion: CapabilityManifestVersion, Kind: "CapabilityManifest", InterceptorRelease: buildinfo.Version, EnvironmentDigest: environmentDigest, ApplicationDigest: applicationDigest, Target: TargetCapability{Name: blueprint.Metadata.Name, Version: blueprint.Metadata.Version, Interface: blueprint.Application.Interface.Adapter}, FeedbackProfiles: append([]string(nil), blueprint.Adaptive.FeedbackProfiles...), InjectionSurfaces: []string{"model_tool_result"}, SnapshotCapable: blueprint.Application.Persistence.SnapshotCapable, Limits: CapabilityLimits{MaximumAttempts: blueprint.Adaptive.MaxAttempts, MaximumArtifacts: blueprint.Adaptive.MaxArtifacts, MaximumArtifactBytes: blueprint.Limits.MaxBlobBytes, MaximumInvocations: blueprint.Limits.MaxTurns, MaximumSnapshots: blueprint.Limits.MaxSnapshots}}
	if blueprint.APIVersion == environment.DeliveryAPIVersion {
		m.DeliverySchemaProfile = delivery.SchemaProfile
	}
	m.Network = NetworkCapability{
		DNS:   DNSCapability{Enabled: blueprint.Network.DNS.Enabled, UnknownPolicy: blueprint.Network.DNS.UnknownPolicy, Upstream: false, MaximumQueries: blueprint.Network.DNS.MaxQueries},
		HTTP:  blueprint.Network.HTTP.Enabled,
		HTTPS: TLSCapability{Enabled: blueprint.Network.TLS.Enabled, TrustMode: blueprint.Network.TLS.TrustMode},
	}
	if m.Network.DNS.Enabled {
		m.Network.DNS.RecordTypes = []string{"A"}
	}
	if m.Network.HTTPS.Enabled {
		m.Network.HTTPS.MinimumVersion = "TLS1.2"
	}
	for _, op := range blueprint.Application.Interface.Operations {
		capability := OperationCapability{ID: op.ID, Method: op.Method, Path: op.Path}
		if blueprint.APIVersion == environment.DeliveryAPIVersion {
			capability.DeliveryStatus = "missing-input-contract"
			capability.MaximumInputBytes = delivery.MaxBodyBytes
			if blueprint.Limits.MaxBlobBytes > 0 && blueprint.Limits.MaxBlobBytes < capability.MaximumInputBytes {
				capability.MaximumInputBytes = blueprint.Limits.MaxBlobBytes
			}
			if op.DeliveryAsset != "" && op.ResolvedDelivery == nil {
				return CapabilityManifest{}, errors.New("delivery asset must be resolved from a verified environment")
			}
			contract := op.ResolvedDelivery
			if contract == nil {
				contract = op.Delivery
			}
			if err := delivery.Validate(contract); err != nil {
				return CapabilityManifest{}, err
			}
			if contract != nil {
				var copied delivery.Contract
				if err := delivery.Clone(contract, &copied); err != nil {
					return CapabilityManifest{}, err
				}
				capability.Delivery = &copied
				capability.DeliveryStatus = "described"
				if copied.Input.MaxBytes < capability.MaximumInputBytes {
					capability.MaximumInputBytes = copied.Input.MaxBytes
				}
			}
		}
		m.Operations = append(m.Operations, capability)
	}
	for _, service := range blueprint.Services {
		capability := ServiceCapability{ID: service.ID, Kind: service.Kind, Role: "environment", MCP: service.Kind == "mcp_http", Transports: serviceTransports(blueprint), InjectionSurfaces: []string{"service_response"}}
		if blueprint.APIVersion == environment.DeliveryAPIVersion {
			if service.Kind == "mcp_http" {
				capability.Implementation = service.Implementation
				if capability.Implementation == "" {
					capability.Implementation = "generic"
				}
			}
			if service.ExportContracts {
				if service.ResolvedEndpoints == nil {
					return CapabilityManifest{}, errors.New("service contracts must be resolved from a verified environment")
				}
				// The environment validator excludes data/credentials; detach maps.
				if err := delivery.Clone(service.ResolvedEndpoints, &capability.Endpoints); err != nil {
					return CapabilityManifest{}, err
				}
			}
		}
		if service.Kind == "stateful_http" || service.Kind == "mcp_http" && service.Seed != "" {
			capability.InjectionSurfaces = append(capability.InjectionSurfaces, "environment_state")
		}
		if service.Kind == "mcp_http" {
			capability.InjectionSurfaces = append(capability.InjectionSurfaces, "mcp_tool_result")
		}
		if service.Kind == "postgresql" {
			capability.Transports = []string{"postgresql+tls"}
			capability.InjectionSurfaces = []string{"database_response"}
			if service.PostgreSQL != nil && len(service.PostgreSQL.Tables) > 0 {
				capability.InjectionSurfaces = append(capability.InjectionSurfaces, "environment_state")
				for _, table := range service.PostgreSQL.Tables {
					capability.DatabaseTables = append(capability.DatabaseTables, DatabaseTableCapability{ID: table.ID, Keys: append([]string{}, table.Keys...), Fields: append([]string{}, table.Fields...), AllowCreate: table.AllowCreate, MaxRecords: table.MaxRecords})
					if table.AllowCreate {
						capability.CreatableCollections = append(capability.CreatableCollections, ResourceCreationCapability{Collection: table.ID, MaxResources: table.MaxRecords})
					}
				}
			}
		}
		for _, policy := range service.Injections.CreateCollections {
			capability.CreatableCollections = append(capability.CreatableCollections, ResourceCreationCapability{Collection: policy.Collection, MaxResources: policy.MaxResources})
		}
		sort.Strings(capability.InjectionSurfaces)
		sort.Slice(capability.CreatableCollections, func(i, j int) bool {
			return capability.CreatableCollections[i].Collection < capability.CreatableCollections[j].Collection
		})
		m.InjectionSurfaces = append(m.InjectionSurfaces, capability.InjectionSurfaces...)
		m.Services = append(m.Services, capability)
	}
	for _, issuer := range blueprint.Identity.Issuers {
		m.Services = append(m.Services, ServiceCapability{ID: issuer.ID, Kind: issuer.Kind, Role: "identity", Transports: serviceTransports(blueprint), InjectionSurfaces: []string{"environment_state"}})
		m.InjectionSurfaces = append(m.InjectionSurfaces, "environment_state")
	}
	for _, service := range blueprint.AttackerServices {
		responseMode := service.Response.Mode
		if responseMode == "" {
			responseMode = "static"
		}
		capability := ServiceCapability{ID: service.ID, Kind: service.Kind, Role: "attacker", ResponseMode: responseMode, Transports: serviceTransports(blueprint)}
		if responseMode == "dynamic" {
			capability.InjectionSurfaces = []string{"service_response"}
			m.InjectionSurfaces = append(m.InjectionSurfaces, "service_response")
		}
		m.Services = append(m.Services, capability)
	}
	for _, namespace := range blueprint.Filesystem.InjectionNamespaces {
		m.FileNamespaces = append(m.FileNamespaces, FileNamespaceCapability{ID: namespace.ID, AllowCreate: namespace.AllowCreate, MaxFiles: namespace.MaxFiles, MaxFileBytes: namespace.MaxFileBytes})
		m.InjectionSurfaces = append(m.InjectionSurfaces, "environment_state")
	}
	for _, oracle := range blueprint.Oracles {
		m.Oracles = append(m.Oracles, oracle.Kind)
	}

	sort.Strings(m.FeedbackProfiles)
	sort.Slice(m.Operations, func(i, j int) bool { return m.Operations[i].ID < m.Operations[j].ID })
	for operation := range allowedGatewayOperations {
		m.CrossVMOperations = append(m.CrossVMOperations, operation)
	}
	sort.Strings(m.CrossVMOperations)
	sort.Slice(m.Services, func(i, j int) bool { return m.Services[i].ID < m.Services[j].ID })
	sort.Slice(m.FileNamespaces, func(i, j int) bool { return m.FileNamespaces[i].ID < m.FileNamespaces[j].ID })
	sort.Strings(m.InjectionSurfaces)
	m.InjectionSurfaces = unique(m.InjectionSurfaces)
	m.InjectionProfiles = injectionProfiles(m.InjectionSurfaces)
	sort.Strings(m.Oracles)
	m.Oracles = unique(m.Oracles)
	encoded, err := json.Marshal(m)
	if err != nil {
		return CapabilityManifest{}, err
	}
	m.Digest = digest.Bytes(encoded)
	return m, nil
}

func injectionProfiles(surfaces []string) []InjectionProfile {
	profiles := make([]InjectionProfile, 0, len(surfaces))
	for _, surface := range surfaces {
		profile := InjectionProfile{Surface: surface}
		switch surface {
		case "model_tool_result":
			profile.Scopes = []string{"once", "next_turn", "standing"}
			profile.Placements = []string{"replace", "prepend", "append", "insert", "merge"}
			profile.SelectorFields = []string{"tool_name", "call_ordinal", "arguments_json_pointer", "arguments_equal", "arguments_contain", "turn_id"}
			profile.Carriers = []string{"model_client_tool_result"}
		case "service_response":
			profile.Scopes = []string{"once", "standing"}
			profile.Placements = []string{"replace", "prepend", "append", "insert", "merge"}
			profile.SelectorFields = []string{"service", "method", "path", "call_ordinal", "query_equal", "headers_equal", "request_body_json_pointer", "request_body_equal", "request_body_contain", "turn_id"}
			profile.Carriers = []string{"managed_http_response", "dynamic_attacker_sink_response"}
		case "database_response":
			profile.Scopes = []string{"once", "standing"}
			profile.Placements = []string{"replace", "prepend", "append", "insert", "merge"}
			profile.SelectorFields = []string{"service", "call_ordinal", "request_body_json_pointer", "request_body_equal", "request_body_contain"}
			profile.Carriers = []string{"postgresql_result"}
		case "mcp_tool_result":
			profile.Scopes = []string{"once", "standing"}
			profile.Placements = []string{"replace", "prepend", "append", "insert", "merge"}
			profile.SelectorFields = []string{"service", "tool_name", "call_ordinal", "arguments_json_pointer", "arguments_equal", "arguments_contain", "turn_id"}
			profile.Carriers = []string{"mcp_tools_call_result"}
		case "environment_state":
			profile.Scopes = []string{"once", "next_turn", "standing"}
			profile.Placements = []string{"replace", "prepend", "append", "insert", "merge"}
			profile.SelectorFields = []string{"service", "collection", "key", "path", "file_namespace", "relative_path", "turn_id"}
			profile.Carriers = []string{"managed_service_state", "mutable_file"}
		}
		profiles = append(profiles, profile)
	}
	return profiles
}

func serviceTransports(blueprint *environment.Blueprint) []string {
	result := []string{"http"}
	if blueprint.Network.TLS.Enabled {
		result = append(result, "https")
	}
	return result
}

func unique(items []string) []string {
	result := items[:0]
	for _, item := range items {
		if len(result) == 0 || result[len(result)-1] != item {
			result = append(result, item)
		}
	}
	return result
}
