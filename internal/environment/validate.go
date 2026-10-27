package environment

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/services"
)

var (
	identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	environmentName   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	imageReference    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[0-9a-f]{64}$`)
)

func Validate(directory string) (*ValidationReport, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve Blueprint directory: %w", err)
	}
	blueprint, err := Load(root)
	if err != nil {
		return nil, err
	}
	report := &ValidationReport{Blueprint: blueprint, Warnings: []string{}, Assets: []Asset{}}
	var failures []error
	add := func(err error) {
		if err != nil {
			failures = append(failures, err)
		}
	}
	assetSources := []string{}
	for index, operation := range blueprint.Application.Interface.Operations {
		if operation.Delivery != nil || operation.DeliveryAsset != "" {
			if blueprint.APIVersion != DeliveryAPIVersion {
				add(errors.New("delivery fields require interceptor.dev/v1alpha2"))
			}
		}
		if operation.Delivery != nil && operation.DeliveryAsset != "" {
			add(fmt.Errorf("operation %d selects both delivery and delivery_asset", index))
		}
		if operation.DeliveryAsset != "" {
			assetSources = append(assetSources, operation.DeliveryAsset)
		}
	}
	for _, service := range blueprint.Services {
		if service.ExportContracts || service.Implementation != "" {
			if blueprint.APIVersion != DeliveryAPIVersion {
				add(errors.New("service contract/implementation fields require interceptor.dev/v1alpha2"))
			}
		}
		if service.Implementation != "" && (service.Kind != "mcp_http" || !services.SupportsMCPImplementation(service.Implementation)) {
			add(fmt.Errorf("service %q selects an unavailable MCP implementation", service.ID))
		}
	}
	if blueprint.Adaptive.MaxAttempts < 1 || blueprint.Adaptive.MaxAttempts > 100000 || blueprint.Adaptive.MaxArtifacts < 1 || blueprint.Adaptive.MaxArtifacts > 200000 {
		add(errors.New("adaptive attempt or artifact limit is out of bounds"))
	}
	seenProfiles := map[string]bool{}
	for _, profile := range blueprint.Adaptive.FeedbackProfiles {
		if profile != "black-box" && profile != "diagnostic" && profile != "oracle-assisted" {
			add(fmt.Errorf("unsupported adaptive feedback profile %q", profile))
		}
		if seenProfiles[profile] {
			add(fmt.Errorf("adaptive feedback profile %q is duplicated", profile))
		}
		seenProfiles[profile] = true
	}

	if blueprint.APIVersion != APIVersion && blueprint.APIVersion != DeliveryAPIVersion {
		add(fmt.Errorf("api_version must be %q or %q", APIVersion, DeliveryAPIVersion))
	}
	if blueprint.Kind != Kind {
		add(fmt.Errorf("kind must be %q", Kind))
	}
	if !identifierPattern.MatchString(blueprint.Metadata.Name) {
		add(errors.New("metadata.name must be a lowercase DNS-like identifier"))
	}
	if strings.TrimSpace(blueprint.Metadata.Version) == "" {
		add(errors.New("metadata.version is required"))
	}
	if strings.TrimSpace(blueprint.Metadata.ReviewedBy) == "" || blueprint.Metadata.ReviewedAt.IsZero() {
		add(errors.New("metadata.reviewed_by and metadata.reviewed_at are required before sealing"))
	}
	if _, err := ApplicationDigest(blueprint.Application.Image); err != nil {
		add(err)
	}
	if len(blueprint.Application.Launch.Command) == 0 || strings.TrimSpace(blueprint.Application.Launch.Command[0]) == "" {
		add(errors.New("application.launch.command must contain an executable argv"))
	}
	if !filepath.IsAbs(blueprint.Application.Launch.Workdir) {
		add(errors.New("application.launch.workdir must be an absolute container path"))
	}
	for name, value := range blueprint.Application.Launch.Environment {
		if !environmentName.MatchString(name) {
			add(fmt.Errorf("application environment variable %q is invalid", name))
		}
		if strings.Contains(value, "${") || strings.HasPrefix(strings.TrimSpace(value), "$") {
			add(fmt.Errorf("application environment variable %q may not interpolate host environment values", name))
		}
		if strings.ContainsAny(value, "\x00\r\n") {
			add(fmt.Errorf("application environment variable %q contains forbidden control characters", name))
		}
	}
	modelBaseURL, hasModelBaseURL := blueprint.Application.Launch.Environment["MODEL_BASE_URL"]
	if !hasModelBaseURL {
		add(errors.New("application.launch.environment must declare MODEL_BASE_URL for the inference proxy"))
	} else {
		add(validateLoopbackURL("application.launch.environment.MODEL_BASE_URL", modelBaseURL))
	}
	for _, err := range validateApplicationInterface(blueprint.Application.Interface) {
		add(err)
	}
	if blueprint.Application.Interface.External != nil && blueprint.Application.Interface.External.ConfigAsset != "" {
		assetSources = append(assetSources, blueprint.Application.Interface.External.ConfigAsset)
	}
	if code := blueprint.Application.Interface.Code; code != nil && code.AdapterAsset != "" {
		add(validateAdapter(root, blueprint))
		assetSources = append(assetSources, code.AdapterAsset)
	}

	mutableRoots := map[string]struct{}{}
	for i, path := range blueprint.Application.Persistence.MutableRoots {
		clean, err := validateContainerPath(fmt.Sprintf("application.persistence.mutable_roots[%d]", i), path)
		add(err)
		if err == nil {
			if reservedMutableRoot(clean) {
				add(fmt.Errorf("mutable root %q is reserved or too broad", clean))
			}
			for existing := range mutableRoots {
				if pathWithin(clean, existing) || pathWithin(existing, clean) {
					add(fmt.Errorf("mutable roots %q and %q overlap", clean, existing))
				}
			}
			mutableRoots[clean] = struct{}{}
		}
	}
	if blueprint.Application.Persistence.SnapshotCapable {
		if len(mutableRoots) == 0 && blueprint.Application.Persistence.Quiesce.Kind != "none" {
			add(errors.New("snapshot-capable application requires mutable_roots or quiesce.kind none"))
		}
		add(validateQuiesce(blueprint.Application.Persistence.Quiesce))
		if blueprint.Application.Interface.Adapter == "command" && blueprint.Application.Persistence.Quiesce.Kind != "none" && blueprint.Application.Persistence.Quiesce.Kind != "command" {
			add(errors.New("command application interfaces support only command or none quiescence"))
		}
	}

	for i, seed := range blueprint.Filesystem.Seed {
		if strings.TrimSpace(seed.Source) == "" {
			add(fmt.Errorf("filesystem.seed[%d].source is required", i))
		} else {
			assetSources = append(assetSources, seed.Source)
		}
		target, err := validateContainerPath(fmt.Sprintf("filesystem.seed[%d].target", i), seed.Target)
		add(err)
		if err == nil && !seed.ReadOnly && !withinAny(target, mutableRoots) {
			add(fmt.Errorf("writable seed target %q is outside application mutable_roots", target))
		}
	}
	fileNamespaceIDs := map[string]struct{}{}
	fileNamespaceRoots := map[string]struct{}{}
	for i, namespace := range blueprint.Filesystem.InjectionNamespaces {
		prefix := fmt.Sprintf("filesystem.injection_namespaces[%d]", i)
		add(validateID(prefix+".id", namespace.ID, fileNamespaceIDs))
		root, err := validateContainerPath(prefix+".root", namespace.Root)
		add(err)
		if err == nil && !withinAny(root, mutableRoots) {
			add(fmt.Errorf("%s.root %q is outside application mutable_roots", prefix, root))
		}
		if err == nil {
			for existing := range fileNamespaceRoots {
				if pathWithin(root, existing) || pathWithin(existing, root) {
					add(fmt.Errorf("filesystem injection namespace roots %q and %q overlap", root, existing))
				}
			}
			fileNamespaceRoots[root] = struct{}{}
		}
		if namespace.AllowCreate {
			if namespace.MaxFiles < 1 || namespace.MaxFiles > 100000 {
				add(fmt.Errorf("%s.max_files must be between 1 and 100000 when creation is enabled", prefix))
			}
			if namespace.MaxFileBytes < 1 || namespace.MaxFileBytes > blueprint.Limits.MaxBlobBytes {
				add(fmt.Errorf("%s.max_file_bytes must be positive and no greater than limits.max_blob_bytes", prefix))
			}
		} else if namespace.MaxFiles != 0 || namespace.MaxFileBytes != 0 {
			add(fmt.Errorf("%s creation limits require allow_create", prefix))
		}
	}

	serviceIDs := map[string]struct{}{}
	hostnames := map[string]string{}
	listens := map[string]string{}
	if blueprint.Network.DNS.Enabled {
		add(validateListen("network.dns.listen", blueprint.Network.DNS.Listen, listens, "network-dns"))
		host, port, err := net.SplitHostPort(blueprint.Network.DNS.Listen)
		if err == nil && host != "127.0.0.1" {
			add(errors.New("network.dns.listen must use 127.0.0.1"))
		}
		if err == nil && port != "53" {
			add(errors.New("network.dns.listen must use standard DNS port 53"))
		}
		if blueprint.Network.DNS.UnknownPolicy != "nxdomain" && blueprint.Network.DNS.UnknownPolicy != "refused" {
			add(errors.New("network.dns.unknown_policy must be nxdomain or refused"))
		}
		if blueprint.Network.DNS.MaxQueries < 1 || blueprint.Network.DNS.MaxQueries > 1_000_000 {
			add(errors.New("network.dns.max_queries must be between 1 and 1000000"))
		}
	} else if blueprint.Network.DNS.Listen != "" || blueprint.Network.DNS.UnknownPolicy != "" || blueprint.Network.DNS.MaxQueries != 0 {
		add(errors.New("network.dns settings require enabled: true"))
	}
	if blueprint.Network.HTTP.Enabled {
		add(validateListen("network.http.listen", blueprint.Network.HTTP.Listen, listens, "network-http"))
		if host, _, err := net.SplitHostPort(blueprint.Network.HTTP.Listen); err == nil && host != "127.0.0.1" {
			add(errors.New("network.http.listen must use 127.0.0.1"))
		}
	} else if blueprint.Network.HTTP.Listen != "" {
		add(errors.New("network.http.listen requires enabled: true"))
	}
	if blueprint.Network.TLS.Enabled {
		add(validateListen("network.tls.listen", blueprint.Network.TLS.Listen, listens, "network-tls"))
		if host, _, err := net.SplitHostPort(blueprint.Network.TLS.Listen); err == nil && host != "127.0.0.1" {
			add(errors.New("network.tls.listen must use 127.0.0.1"))
		}
		switch blueprint.Network.TLS.TrustMode {
		case "auto", "bundle":
			if len(blueprint.Network.TLS.ApplicationManagedProbe) != 0 {
				add(errors.New("network.tls.application_managed_probe is valid only with application-managed trust"))
			}
		case "application-managed":
			if len(blueprint.Network.TLS.ApplicationManagedProbe) == 0 {
				add(errors.New("network.tls.application_managed_probe is required with application-managed trust"))
			} else if !filepath.IsAbs(blueprint.Network.TLS.ApplicationManagedProbe[0]) {
				add(errors.New("network.tls.application_managed_probe executable must be an absolute path"))
			}
		default:
			add(errors.New("network.tls.trust_mode must be auto, bundle, or application-managed"))
		}
		for index, argument := range blueprint.Network.TLS.ApplicationManagedProbe {
			if argument == "" || strings.ContainsRune(argument, '\x00') || len(argument) > 4096 {
				add(fmt.Errorf("network.tls.application_managed_probe[%d] is invalid", index))
			}
		}
	} else if blueprint.Network.TLS.Listen != "" || blueprint.Network.TLS.TrustMode != "" || len(blueprint.Network.TLS.ApplicationManagedProbe) != 0 {
		add(errors.New("network.tls settings require enabled: true"))
	}
	for i, service := range blueprint.Services {
		prefix := fmt.Sprintf("services[%d]", i)
		add(validateID(prefix+".id", service.ID, serviceIDs))
		if service.Kind != "fixture_http" && service.Kind != "stateful_http" && service.Kind != "mcp_http" {
			add(fmt.Errorf("%s.kind %q is unsupported", prefix, service.Kind))
		}
		add(validateListen(prefix+".listen", service.Listen, listens, service.ID))
		add(validateHostnames(prefix, service.Hostnames, hostnames, service.ID))
		creationCollections := map[string]struct{}{}
		for policyIndex, policy := range service.Injections.CreateCollections {
			policyPrefix := fmt.Sprintf("%s.injections.create_collections[%d]", prefix, policyIndex)
			if policy.Collection == "" || strings.ContainsAny(policy.Collection, "\x00\r\n/") {
				add(fmt.Errorf("%s.collection is invalid", policyPrefix))
			}
			if _, duplicate := creationCollections[policy.Collection]; duplicate {
				add(fmt.Errorf("%s.collection %q is duplicated", policyPrefix, policy.Collection))
			}
			creationCollections[policy.Collection] = struct{}{}
			if policy.MaxResources < 1 || policy.MaxResources > 100000 {
				add(fmt.Errorf("%s.max_resources must be between 1 and 100000", policyPrefix))
			}
			if service.Kind != "stateful_http" && service.Kind != "mcp_http" {
				add(fmt.Errorf("%s is valid only for stateful_http or mcp_http", policyPrefix))
			}
		}
		if service.Kind == "fixture_http" {
			if service.Fixtures == "" {
				add(fmt.Errorf("%s.fixtures is required for fixture_http", prefix))
			} else {
				assetSources = append(assetSources, service.Fixtures)
			}
		}
		if service.Kind == "stateful_http" {
			if service.Seed == "" || service.Routes == "" {
				add(fmt.Errorf("%s.seed and routes are required for stateful_http", prefix))
			}
			if service.Seed != "" {
				assetSources = append(assetSources, service.Seed)
			}
			if service.Routes != "" {
				assetSources = append(assetSources, service.Routes)
			}
		}
		if service.Kind == "mcp_http" {
			if service.Tools == "" {
				add(fmt.Errorf("%s.tools is required for mcp_http", prefix))
			} else {
				assetSources = append(assetSources, service.Tools)
			}
			if service.Endpoint == "" || !strings.HasPrefix(service.Endpoint, "/") ||
				path.Clean(service.Endpoint) != service.Endpoint || strings.ContainsAny(service.Endpoint, "\x00\r\n?#") {
				add(fmt.Errorf("%s.endpoint must be a clean absolute HTTP path", prefix))
			}
			if service.Fixtures != "" || service.Routes != "" {
				add(fmt.Errorf("%s mcp_http may not declare fixtures or routes", prefix))
			}
			if service.Seed != "" {
				assetSources = append(assetSources, service.Seed)
			}
		}
	}
	identityResult, identityFailures := validateIdentity(blueprint, serviceIDs, hostnames, listens)
	for _, identityFailure := range identityFailures {
		add(identityFailure)
	}

	attackerIDs := map[string]struct{}{}
	for i, service := range blueprint.AttackerServices {
		prefix := fmt.Sprintf("attacker_services[%d]", i)
		add(validateID(prefix+".id", service.ID, attackerIDs))
		if _, duplicate := serviceIDs[service.ID]; duplicate {
			add(fmt.Errorf("%s.id %q conflicts with a synthetic service", prefix, service.ID))
		}
		if _, duplicate := identityResult.issuerIDs[service.ID]; duplicate {
			add(fmt.Errorf("%s.id %q conflicts with an identity issuer", prefix, service.ID))
		}
		if service.Kind != "http_sink" {
			add(fmt.Errorf("%s.kind %q is unsupported", prefix, service.Kind))
		}
		add(validateListen(prefix+".listen", service.Listen, listens, service.ID))
		add(validateHostnames(prefix, service.Hostnames, hostnames, service.ID))
		if service.Response.Status < 100 || service.Response.Status > 599 {
			add(fmt.Errorf("%s.response.status must be an HTTP status", prefix))
		}
		if service.Response.Mode != "static" && service.Response.Mode != "dynamic" {
			add(fmt.Errorf("%s.response.mode must be static or dynamic", prefix))
		}
		if service.Response.Mode == "dynamic" && (service.Response.Status < 200 || service.Response.Status >= 300 || service.Response.Status == http.StatusNoContent || service.Response.Status == http.StatusResetContent) {
			add(fmt.Errorf("%s dynamic response requires a body-capable 2xx status", prefix))
		}
		if service.MaxCaptureBytes < 0 {
			add(fmt.Errorf("%s.max_capture_bytes must not be negative", prefix))
		}
	}
	if (blueprint.Network.DNS.Enabled || blueprint.Network.HTTP.Enabled || blueprint.Network.TLS.Enabled) && len(hostnames) == 0 {
		add(errors.New("enabled network simulation requires at least one declared service hostname"))
	}
	if blueprint.Network.TLS.Enabled {
		reservedTrustVariables := map[string]struct{}{"INTERCEPTOR_CA_BUNDLE": {}}
		if blueprint.Network.TLS.TrustMode == "auto" {
			for _, name := range []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "AWS_CA_BUNDLE", "GRPC_DEFAULT_SSL_ROOTS_FILE_PATH"} {
				reservedTrustVariables[name] = struct{}{}
			}
		}
		for name := range reservedTrustVariables {
			if _, exists := blueprint.Application.Launch.Environment[name]; exists {
				add(fmt.Errorf("application.launch.environment.%s is managed by network.tls trust mode %s", name, blueprint.Network.TLS.TrustMode))
			}
		}
	}
	for name, value := range blueprint.Application.Launch.Environment {
		if name == "MODEL_BASE_URL" || !strings.HasSuffix(name, "_BASE_URL") {
			continue
		}
		parsed, err := url.Parse(value)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			add(fmt.Errorf("application endpoint %s must be an HTTP or HTTPS URL", name))
			continue
		}
		if parsed.Scheme == "https" && !blueprint.Network.TLS.Enabled {
			add(fmt.Errorf("application endpoint %s uses HTTPS but network.tls is not enabled", name))
		}
		host := strings.ToLower(parsed.Hostname())
		if parsed.Scheme == "https" {
			if _, declared := hostnames[host]; !declared {
				add(fmt.Errorf("application endpoint %s HTTPS host %q must be a declared simulated-service hostname", name, host))
			}
			_, tlsPort, _ := net.SplitHostPort(blueprint.Network.TLS.Listen)
			urlPort := parsed.Port()
			if urlPort == "" {
				urlPort = "443"
			}
			if urlPort != tlsPort {
				add(fmt.Errorf("application endpoint %s HTTPS port must match network.tls.listen port %s", name, tlsPort))
			}
		}
		if !isLoopbackHost(host) {
			if _, declared := hostnames[host]; !declared {
				add(fmt.Errorf("application endpoint %s points to undeclared non-local hostname %q", name, host))
			}
		}
	}

	credentialIDs := map[string]struct{}{}
	credentialVariables := map[string]string{}
	for i, credential := range blueprint.Credentials {
		prefix := fmt.Sprintf("credentials[%d]", i)
		add(validateID(prefix+".id", credential.ID, credentialIDs))
		if !environmentName.MatchString(credential.EnvironmentVariable) {
			add(fmt.Errorf("%s.environment_variable is invalid", prefix))
		}
		if credential.Value == "" {
			add(fmt.Errorf("%s.value is required", prefix))
		}
		if _, exists := serviceIDs[credential.AcceptedBy]; !exists && credential.AcceptedBy != "model-proxy" {
			add(fmt.Errorf("%s.accepted_by references unknown service %q", prefix, credential.AcceptedBy))
		}
		if current, exists := blueprint.Application.Launch.Environment[credential.EnvironmentVariable]; exists && current != credential.Value {
			add(fmt.Errorf("%s conflicts with application.launch.environment", prefix))
		}
		credentialVariables[credential.EnvironmentVariable] = credential.Value
	}
	for name, value := range blueprint.Application.Launch.Environment {
		upper := strings.ToUpper(name)
		looksCredential := strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") || strings.HasSuffix(upper, "_KEY")
		if !looksCredential {
			continue
		}
		if declared, exists := credentialVariables[name]; !exists || declared != value {
			add(fmt.Errorf("credential-like application environment variable %q must be declared as a synthetic credential", name))
		}
	}

	canaryIDs, canaryValues := map[string]struct{}{}, map[string]string{}
	for i, canary := range blueprint.Canaries {
		prefix := fmt.Sprintf("canaries[%d]", i)
		add(validateID(prefix+".id", canary.ID, canaryIDs))
		if canary.Value == "" {
			add(fmt.Errorf("%s.value is required", prefix))
		} else if previous, exists := canaryValues[canary.Value]; exists {
			add(fmt.Errorf("%s.value duplicates canary %q", prefix, previous))
		} else {
			canaryValues[canary.Value] = canary.ID
		}
		if canary.Location != "" {
			_, err := validateContainerPath(prefix+".location", canary.Location)
			add(err)
		}
		if canary.Proves == "" {
			add(fmt.Errorf("%s.proves is required", prefix))
		}
	}

	oracleIDs := map[string]struct{}{}
	for i, oracle := range blueprint.Oracles {
		prefix := fmt.Sprintf("oracles[%d]", i)
		add(validateID(prefix+".id", oracle.ID, oracleIDs))
		switch oracle.Kind {
		case "attacker_sink_match":
			if _, ok := attackerIDs[oracle.Service]; !ok {
				add(fmt.Errorf("%s.service references unknown attacker service %q", prefix, oracle.Service))
			}
		case "output_contains", "model_output_contains":
			if oracle.Value == "" {
				add(fmt.Errorf("%s.value is required for %s", prefix, oracle.Kind))
			}
		case "tool_call_match", "service_state_match", "file_state_match", "event_match":
			if len(oracle.Match) == 0 {
				add(fmt.Errorf("%s.match is required for %s", prefix, oracle.Kind))
			}
			if oracle.Kind == "event_match" {
				add(validateEventMatch(prefix+".match", oracle.Match))
			}
		default:
			add(fmt.Errorf("%s.kind %q is unsupported", prefix, oracle.Kind))
		}
	}

	if blueprint.Time.Mode != "" && blueprint.Time.Mode != "fixed" {
		add(fmt.Errorf("time.mode %q is unsupported; MVP supports fixed", blueprint.Time.Mode))
	}
	if blueprint.Time.Mode == "fixed" && blueprint.Time.Base.IsZero() {
		add(errors.New("time.base is required when time.mode is fixed"))
	}
	if blueprint.Limits.MaxTurns <= 0 || blueprint.Limits.MaxTokens <= 0 ||
		blueprint.Limits.MaxWallSeconds <= 0 || blueprint.Limits.MaxSnapshots < 0 ||
		blueprint.Limits.MaxBlobBytes <= 0 || blueprint.Limits.MaxPIDs <= 0 ||
		blueprint.Limits.MemoryBytes <= 0 {
		add(errors.New("limits must be positive; max_snapshots may be zero"))
	}
	if blueprint.Limits.MaxBlobBytes > 256<<20 {
		add(errors.New("max_blob_bytes may not exceed 256 MiB"))
	}

	assets, err := collectAssets(root, assetSources)
	add(err)
	report.Assets = assets
	if err == nil {
		for i, service := range blueprint.Services {
			switch service.Kind {
			case "fixture_http":
				if _, fixtureErr := services.LoadFixtureSet(filepath.Join(root, filepath.FromSlash(service.Fixtures))); fixtureErr != nil {
					add(fmt.Errorf("services[%d] fixtures: %w", i, fixtureErr))
				}
			case "stateful_http":
				stateful, stateErr := services.LoadStateful(filepath.Join(root, filepath.FromSlash(service.Seed)), filepath.Join(root, filepath.FromSlash(service.Routes)), service.ID, nil, nil)
				if stateErr != nil {
					add(fmt.Errorf("services[%d] stateful definition: %w", i, stateErr))
				} else {
					for _, policy := range service.Injections.CreateCollections {
						if !stateful.SupportsCreate(policy.Collection) {
							add(fmt.Errorf("services[%d] injection collection %q has no sealed create route", i, policy.Collection))
						}
					}
				}
			case "mcp_http":
				seedPath := ""
				if service.Seed != "" {
					seedPath = filepath.Join(root, filepath.FromSlash(service.Seed))
				}
				mcp, mcpErr := services.LoadMCP(filepath.Join(root, filepath.FromSlash(service.Tools)), seedPath, service.Endpoint, service.ID, nil, nil)
				if mcpErr != nil {
					add(fmt.Errorf("services[%d] MCP definition: %w", i, mcpErr))
				} else {
					for _, policy := range service.Injections.CreateCollections {
						if !mcp.SupportsCreate(policy.Collection) {
							add(fmt.Errorf("services[%d] injection collection %q has no sealed create tool", i, policy.Collection))
						}
					}
				}
			}
		}
		for _, canary := range blueprint.Canaries {
			if canary.Location != "" {
				add(validateCanaryFile(root, blueprint.Filesystem.Seed, canary))
			}
		}
	}
	if len(failures) == 0 {
		add(resolveDelivery(root, report))
	}
	if len(failures) > 0 {
		return report, errors.Join(failures...)
	}
	return report, nil
}

func validateEventMatch(prefix string, match map[string]any) error {
	var failures []error
	allowed := map[string]bool{"channel": true, "layer": true, "kind": true, "provenance": true, "attempt_id": true, "injection_id": true, "predicates": true}
	for key, value := range match {
		if !allowed[key] {
			failures = append(failures, fmt.Errorf("%s.%s is unsupported", prefix, key))
			continue
		}
		if key != "predicates" {
			if text, ok := value.(string); !ok || text == "" || len(text) > 128 {
				failures = append(failures, fmt.Errorf("%s.%s must be a bounded non-empty string", prefix, key))
			}
		}
	}
	rawPredicates, exists := match["predicates"]
	if !exists {
		return errors.Join(failures...)
	}
	predicates, ok := rawPredicates.([]any)
	if !ok || len(predicates) == 0 || len(predicates) > 16 {
		failures = append(failures, fmt.Errorf("%s.predicates must contain 1 through 16 predicates", prefix))
		return errors.Join(failures...)
	}
	for index, raw := range predicates {
		predicate, ok := raw.(map[string]any)
		if !ok {
			failures = append(failures, fmt.Errorf("%s.predicates[%d] must be an object", prefix, index))
			continue
		}
		for key := range predicate {
			if key != "field" && key != "operator" && key != "value" {
				failures = append(failures, fmt.Errorf("%s.predicates[%d].%s is unsupported", prefix, index, key))
			}
		}
		field, fieldOK := predicate["field"].(string)
		if !fieldOK || field == "" || len(field) > 128 || strings.ContainsAny(field, "/.[]") {
			failures = append(failures, fmt.Errorf("%s.predicates[%d].field must name one top-level scalar field", prefix, index))
		}
		operator, _ := predicate["operator"].(string)
		value, hasValue := predicate["value"]
		scalar := func(value any) bool {
			switch value.(type) {
			case nil, string, bool, int, int64, uint64, float64:
				return true
			default:
				return false
			}
		}
		switch operator {
		case "exists":
			if _, ok := value.(bool); !hasValue || !ok {
				failures = append(failures, fmt.Errorf("%s.predicates[%d].value must be boolean for exists", prefix, index))
			}
		case "contains":
			if text, ok := value.(string); !hasValue || !ok || text == "" {
				failures = append(failures, fmt.Errorf("%s.predicates[%d].value must be non-empty text for contains", prefix, index))
			}
		case "equals":
			if !hasValue || !scalar(value) {
				failures = append(failures, fmt.Errorf("%s.predicates[%d].value must be scalar for equals", prefix, index))
			}
		case "one_of":
			values, ok := value.([]any)
			if !hasValue || !ok || len(values) == 0 || len(values) > 32 {
				failures = append(failures, fmt.Errorf("%s.predicates[%d].value must contain 1 through 32 scalar values", prefix, index))
			} else {
				for _, candidate := range values {
					if !scalar(candidate) {
						failures = append(failures, fmt.Errorf("%s.predicates[%d].value must not contain nested values", prefix, index))
						break
					}
				}
			}
		default:
			failures = append(failures, fmt.Errorf("%s.predicates[%d].operator is unsupported", prefix, index))
		}
	}
	return errors.Join(failures...)
}

func validateCanaryFile(root string, seeds []SeedEntry, canary Canary) error {
	for _, seed := range seeds {
		if canary.Location != seed.Target && !strings.HasPrefix(canary.Location, seed.Target+"/") {
			continue
		}
		source := filepath.Join(root, filepath.FromSlash(seed.Source))
		info, err := os.Lstat(source)
		if err != nil {
			return err
		}
		path := source
		if info.IsDir() {
			relative := strings.TrimPrefix(strings.TrimPrefix(canary.Location, seed.Target), "/")
			path = filepath.Join(source, filepath.FromSlash(relative))
		} else if canary.Location != seed.Target {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("canary %q location cannot be read: %w", canary.ID, err)
		}
		if !strings.Contains(string(content), canary.Value) {
			return fmt.Errorf("canary %q value is absent from declared location %s", canary.ID, canary.Location)
		}
		return nil
	}
	return fmt.Errorf("canary %q location %s is not provided by a filesystem seed", canary.ID, canary.Location)
}

func ApplicationDigest(image string) (string, error) {
	if !imageReference.MatchString(image) {
		return "", errors.New("application.image contains invalid OCI reference characters or is not digest pinned")
	}
	index := strings.LastIndex(image, "@")
	if index < 1 || index == len(image)-1 || !digest.Valid(image[index+1:]) {
		return "", errors.New("application.image must be an immutable OCI reference ending in @sha256:<64 lowercase hex>")
	}
	return image[index+1:], nil
}

func validateApplicationInterface(value ApplicationInterface) []error {
	var failures []error
	add := func(err error) {
		if err != nil {
			failures = append(failures, err)
		}
	}
	if value.StartupTimeoutMS <= 0 || value.StartupTimeoutMS > 10*60*1000 {
		add(errors.New("application.interface.startup_timeout_ms must be between 1 and 600000"))
	}
	if value.TurnTimeoutMS <= 0 || value.TurnTimeoutMS > 10*60*1000 {
		add(errors.New("application.interface.turn_timeout_ms must be between 1 and 600000"))
	}
	if len(value.Operations) == 0 {
		add(errors.New("application.interface.operations must not be empty"))
	}
	seenIDs, seenRoutes := map[string]struct{}{}, map[string]struct{}{}
	for index, operation := range value.Operations {
		prefix := fmt.Sprintf("application.interface.operations[%d]", index)
		add(validateID(prefix+".id", operation.ID, seenIDs))
		if value.Adapter == "http" {
			method := strings.ToUpper(strings.TrimSpace(operation.Method))
			if method == "" || operation.Path == "" || !strings.HasPrefix(operation.Path, "/") || path.Clean(operation.Path) != operation.Path || strings.ContainsAny(operation.Path, "\x00\r\n?#") {
				add(fmt.Errorf("%s requires an HTTP method and clean absolute path", prefix))
			}
			key := method + " " + operation.Path
			if _, exists := seenRoutes[key]; exists {
				add(fmt.Errorf("duplicate application HTTP route %s", key))
			}
			seenRoutes[key] = struct{}{}
		} else if operation.Method != "" || operation.Path != "" {
			add(fmt.Errorf("%s method/path are valid only for the HTTP interface", prefix))
		}
		for bindingIndex, binding := range operation.Arguments {
			if value.Adapter != "command" {
				add(fmt.Errorf("%s.arguments are valid only for the command interface", prefix))
				break
			}
			if (binding.Literal == "") == (binding.From == "") || binding.From != "" && !strings.HasPrefix(binding.From, "/") {
				add(fmt.Errorf("%s.arguments[%d] requires exactly one literal or JSON Pointer from", prefix, bindingIndex))
			}
		}
	}
	configured := 0
	for _, present := range []bool{value.HTTP != nil, value.Command != nil, value.Stdio != nil, value.External != nil, value.Code != nil} {
		if present {
			configured++
		}
	}
	if configured != 1 {
		add(errors.New("application.interface requires exactly one matching adapter configuration"))
	}
	switch value.Adapter {
	case "http":
		if value.HTTP == nil {
			add(errors.New("application.interface.http is required for kind http"))
		} else {
			add(validateLoopbackURL("application.interface.http.listen", value.HTTP.Listen))
			if value.HTTP.HealthPath != "" && (!strings.HasPrefix(value.HTTP.HealthPath, "/") || path.Clean(value.HTTP.HealthPath) != value.HTTP.HealthPath) {
				add(errors.New("application.interface.http.health_path must be a clean absolute path"))
			}
		}
	case "command":
		if value.Command == nil {
			add(errors.New("application.interface.command is required for kind command"))
		} else {
			stdin := value.Command.Stdin
			if stdin == "" {
				stdin = "raw"
			}
			if stdin != "raw" && stdin != "json" && stdin != "none" {
				add(errors.New("application.interface.command.stdin must be raw, json, or none"))
			}
			if value.Command.Concurrency < 0 || value.Command.Concurrency > 32 {
				add(errors.New("application.interface.command.concurrency must be between 0 and 32"))
			}
			seenExitCodes := map[int]struct{}{}
			for _, code := range value.Command.SuccessCodes {
				if code < 0 || code > 255 {
					add(errors.New("application.interface.command.success_exit_codes must contain values from 0 through 255"))
				}
				if _, exists := seenExitCodes[code]; exists {
					add(fmt.Errorf("application.interface.command.success_exit_codes contains duplicate %d", code))
				}
				seenExitCodes[code] = struct{}{}
			}
			if strings.ContainsAny(value.Command.OutputMediaType, "\x00\r\n") {
				add(errors.New("application.interface.command.output_media_type contains forbidden control characters"))
			}
		}
	case "stdio", "repl":
		if value.Stdio == nil {
			add(fmt.Errorf("application.interface.stdio is required for kind %s", value.Adapter))
		} else {
			transport := value.Stdio.Transport
			if transport == "" {
				transport = map[bool]string{true: "pty", false: "pipes"}[value.Adapter == "repl"]
			}
			if transport != "pipes" && transport != "pty" {
				add(errors.New("application.interface.stdio.transport must be pipes or pty"))
			}
			mode := value.Stdio.ResponseMode
			if mode == "" {
				mode = "line"
			}
			if mode != "line" && mode != "delimiter" && mode != "prompt" && mode != "idle" {
				add(errors.New("application.interface.stdio.response_mode must be line, delimiter, prompt, or idle"))
			}
			if mode == "delimiter" && value.Stdio.Delimiter == "" || mode == "prompt" && value.Stdio.TerminalPattern == "" {
				add(fmt.Errorf("application.interface.stdio.%s completion value is required", mode))
			}
			for name, pattern := range map[string]string{"ready_pattern": value.Stdio.ReadyPattern, "terminal_pattern": value.Stdio.TerminalPattern} {
				if pattern != "" {
					if _, err := regexp.Compile(pattern); err != nil {
						add(fmt.Errorf("application.interface.stdio.%s is invalid: %w", name, err))
					}
				}
			}
			if value.Stdio.IdleMS < 0 || value.Stdio.IdleMS > 60000 {
				add(errors.New("application.interface.stdio.idle_ms must be between 0 and 60000"))
			}
			if strings.ContainsRune(value.Stdio.InputSuffix, '\x00') || strings.ContainsAny(value.Stdio.OutputMediaType, "\x00\r\n") {
				add(errors.New("application.interface.stdio contains forbidden control characters"))
			}
		}
	case "external":
		if value.External == nil || value.External.Protocol != "interceptor.application/v1" {
			add(errors.New("external interface requires protocol interceptor.application/v1"))
		} else if value.External.Concurrency < 0 || value.External.Concurrency > 32 {
			add(errors.New("application.interface.external.concurrency must be between 0 and 32"))
		}
	case "code":
		if value.Code == nil {
			add(errors.New("application.interface.code is required for kind code"))
		} else {
			if value.Code.Language != "python" && value.Code.Language != "node" {
				add(errors.New("application.interface.code.language must be python or node"))
			}
			if strings.TrimSpace(value.Code.Module) == "" || strings.TrimSpace(value.Code.Invoke) == "" {
				add(errors.New("application.interface.code.module and invoke are required"))
			}
			if strings.ContainsAny(value.Code.Module+value.Code.Initialize+value.Code.Invoke+value.Code.Close, "\x00\r\n") {
				add(errors.New("application.interface.code names contain forbidden control characters"))
			}
			if value.Code.InputMode != "" && value.Code.InputMode != "single" && value.Code.InputMode != "kwargs" {
				add(errors.New("application.interface.code.input_mode must be single or kwargs"))
			}
			if value.Code.Language == "node" && value.Code.InputMode == "kwargs" {
				add(errors.New("application.interface.code.input_mode kwargs is supported only for Python"))
			}
			if value.Code.Concurrency < 0 || value.Code.Concurrency > 32 {
				add(errors.New("application.interface.code.concurrency must be between 0 and 32"))
			}
			if value.Code.Language == "python" && value.Code.Concurrency > 1 {
				add(errors.New("application.interface.code.concurrency greater than 1 is unsupported for Python"))
			}
		}
	default:
		add(fmt.Errorf("application.interface.kind %q is unsupported", value.Adapter))
	}
	return failures
}

func validateQuiesce(value QuiesceConfig) error {
	switch value.Kind {
	case "http":
		if value.Method == "" || value.URL == "" {
			return errors.New("persistence.quiesce http requires method and url")
		}
		return validateLoopbackURL("application.persistence.quiesce.url", value.URL)
	case "signal":
		if value.Signal == "" || (value.ReadyFile == "" && value.ReadyURL == "") {
			return errors.New("persistence.quiesce signal requires signal and ready_file or ready_url")
		}
		if value.ReadyURL != "" {
			return validateLoopbackURL("application.persistence.quiesce.ready_url", value.ReadyURL)
		}
		_, err := validateContainerPath("application.persistence.quiesce.ready_file", value.ReadyFile)
		return err
	case "command":
		if len(value.Command) == 0 || value.Command[0] == "" {
			return errors.New("persistence.quiesce command requires argv")
		}
		return nil
	case "none":
		return nil
	default:
		return fmt.Errorf("persistence.quiesce.kind %q is unsupported", value.Kind)
	}
}

func validateLoopbackURL(name, value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return fmt.Errorf("%s must be an http loopback URL", name)
	}
	host := parsed.Hostname()
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("%s host must be loopback", name)
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateListen(name, value string, seen map[string]string, id string) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil || port == "" {
		return fmt.Errorf("%s must be host:port", name)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("%s must listen on loopback", name)
	}
	canonicalHost := host
	if host == "localhost" {
		canonicalHost = "127.0.0.1"
	} else if ip != nil {
		canonicalHost = ip.String()
	}
	canonical := net.JoinHostPort(canonicalHost, port)
	if previous, exists := seen[canonical]; exists {
		return fmt.Errorf("%s conflicts with service %q", name, previous)
	}
	seen[canonical] = id
	return nil
}

func validateHostnames(prefix string, values []string, seen map[string]string, id string) error {
	if len(values) == 0 {
		return fmt.Errorf("%s.hostnames must not be empty", prefix)
	}
	var failures []error
	for _, value := range values {
		name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
		if !validDNSHostname(name) {
			failures = append(failures, fmt.Errorf("%s hostname %q is invalid", prefix, value))
			continue
		}
		if previous, exists := seen[name]; exists {
			failures = append(failures, fmt.Errorf("%s hostname %q conflicts with service %q", prefix, name, previous))
			continue
		}
		seen[name] = id
	}
	return errors.Join(failures...)
}

func validDNSHostname(name string) bool {
	if len(name) < 3 || len(name) > 253 || !strings.Contains(name, ".") {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
				return false
			}
		}
	}
	return true
}

func validateID(name, value string, seen map[string]struct{}) error {
	if !identifierPattern.MatchString(value) {
		return fmt.Errorf("%s must be a lowercase identifier", name)
	}
	if _, exists := seen[value]; exists {
		return fmt.Errorf("%s %q is duplicated", name, value)
	}
	seen[value] = struct{}{}
	return nil
}

func validateContainerPath(name, value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n,") {
		return "", fmt.Errorf("%s contains unsupported characters", name)
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must be an absolute container path", name)
	}
	clean := filepath.Clean(value)
	if clean != value {
		return "", fmt.Errorf("%s must be clean; got %q", name, value)
	}
	return clean, nil
}

func reservedMutableRoot(path string) bool {
	for _, reserved := range []string{"/", "/proc", "/sys", "/dev", "/run", "/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/environment"} {
		if path == reserved || reserved != "/" && pathWithin(path, reserved) {
			return true
		}
	}
	return false
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func withinAny(path string, roots map[string]struct{}) bool {
	for root := range roots {
		if pathWithin(path, root) {
			return true
		}
	}
	return false
}

func collectAssets(root string, sources []string) ([]Asset, error) {
	assets := map[string]Asset{}
	var failures []error
	for _, source := range sources {
		clean := filepath.Clean(source)
		if source == "" || strings.ContainsAny(source, "\x00\r\n,") || filepath.IsAbs(source) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			failures = append(failures, fmt.Errorf("asset source %q must be a relative path within the Blueprint", source))
			continue
		}
		full := filepath.Join(root, clean)
		if !pathWithin(full, root) {
			failures = append(failures, fmt.Errorf("asset source %q escapes Blueprint directory", source))
			continue
		}
		if _, err := os.Lstat(full); err != nil {
			failures = append(failures, fmt.Errorf("asset source %q: %w", source, err))
			continue
		}
		err := filepath.WalkDir(full, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(root, path)
			if err != nil || strings.HasPrefix(relative, "..") {
				return fmt.Errorf("asset %s escapes Blueprint directory", path)
			}
			relative = filepath.ToSlash(relative)
			info, err := entry.Info()
			if err != nil {
				return err
			}
			mode := info.Mode()
			asset := Asset{Path: relative, Mode: uint32(mode.Perm())}
			switch {
			case mode.IsRegular():
				if hardLinked(info) {
					return fmt.Errorf("asset %s is hard-linked", relative)
				}
				file, err := os.Open(path)
				if err != nil {
					return err
				}
				asset.Digest, asset.Size, err = digest.Reader(file)
				file.Close()
				if err != nil {
					return err
				}
				asset.Type = "file"
			case mode.IsDir():
				asset.Type = "directory"
				asset.Digest = digest.Bytes(nil)
			case mode&os.ModeSymlink != 0:
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				resolved := filepath.Clean(filepath.Join(filepath.Dir(path), target))
				if !pathWithin(resolved, root) {
					return fmt.Errorf("asset symlink %s escapes Blueprint directory", relative)
				}
				asset.Type, asset.Target = "symlink", filepath.ToSlash(target)
				asset.Digest, asset.Size = digest.Bytes([]byte(asset.Target)), int64(len(asset.Target))
			default:
				return fmt.Errorf("asset %s has unsupported type %s", relative, mode.Type())
			}
			if existing, exists := assets[relative]; exists && existing != asset {
				return fmt.Errorf("asset %s has conflicting manifests", relative)
			}
			assets[relative] = asset
			return nil
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("inspect asset %q: %w", source, err))
		}
	}
	result := make([]Asset, 0, len(assets))
	for _, asset := range assets {
		result = append(result, asset)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, errors.Join(failures...)
}

func hardLinked(info fs.FileInfo) bool {
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return false
	}
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return false
	}
	field := value.FieldByName("Nlink")
	return field.IsValid() && field.CanUint() && field.Uint() > 1
}
