package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/appimage"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo"
	"github.com/intrusive-ai/interceptor-sandbox/internal/credentials"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evaluation"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evidence"
	"github.com/intrusive-ai/interceptor-sandbox/internal/filemanagement"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/hostlifetime"
	"github.com/intrusive-ai/interceptor-sandbox/internal/hostrun"
	"github.com/intrusive-ai/interceptor-sandbox/internal/localapi"
	"github.com/intrusive-ai/interceptor-sandbox/internal/ociruntime"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/sandboxd"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

var version = buildinfo.Version

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(arguments []string, stdout, stderr io.Writer) error {
	if len(arguments) == 0 {
		usage(stderr)
		return errors.New("command is required")
	}

	switch arguments[0] {
	case "run", "status", "list", "logs", "snapshot", "identity", "stop", "evaluate", "coverage":
		root, err := session.DataRoot()
		if err != nil {
			return err
		}
		lock, err := filemanagement.Lock(root, "maintenance", false)
		if err != nil {
			return err
		}
		defer lock.Close()
	}
	switch arguments[0] {
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, version)
		return nil
	case "environment":
		return environmentCommand(arguments[1:], stdout)
	case "credentials":
		return credentialsCheckCommand(arguments[1:], stdout, resolveStoredCredential)
	case "app":
		return appCommand(arguments[1:], stdout)
	case "run":
		return runCommand(arguments[1:], stdout)
	case "sandboxd":
		return sandboxdCommand(arguments[1:])
	case "app-helper":
		return sandboxd.RunHelper()
	case "image-install-ca":
		if len(arguments) != 2 {
			return errors.New("image-install-ca requires one CA path")
		}
		return appimage.InstallSystemCA(arguments[1])
	case "image-check-adapter":
		if len(arguments) != 2 {
			return errors.New("image-check-adapter requires one destination")
		}
		return appimage.CheckAdapterDestination(arguments[1])
	case "image-install-python-ca":
		if len(arguments) < 4 {
			return errors.New("image-install-python-ca requires CA path, mount list JSON, and interpreter paths")
		}
		return appimage.InstallPythonCA(arguments[1], arguments[2], arguments[3:])
	case "status":
		return statusCommand(arguments[1:], stdout)
	case "list":
		return listCommand(arguments[1:], stdout)
	case "logs":
		return logsCommand(arguments[1:], stdout)
	case "snapshot":
		return snapshotCommand(arguments[1:], stdout)
	case "identity":
		return identityCommand(arguments[1:], stdout)
	case "restore":
		return errors.New("restore is managed by the running local service; use POST /v1/lifecycle")
	case "stop":
		return stopCommand(arguments[1:], stdout)
	case "purge":
		return purgeCommand(arguments[1:], stdout)
	case "delete":
		return deleteCommand(arguments[1:], stdout)
	case "evaluate":
		return sessionAPICommand(arguments[1:], stdout, "/v1/evaluation")
	case "coverage":
		return coverageCommand(arguments[1:], stdout)
	case "capabilities":
		return capabilitiesCommand(arguments[1:], stdout)
	case "gateway":
		return errors.New("gateway is integrated into interceptor run; use http://127.0.0.1:8080")
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func capabilitiesCommand(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("capabilities", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	directory := flags.String("environment", "", "Environment Blueprint directory")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *directory == "" {
		return errors.New("usage: interceptor capabilities --environment <directory>")
	}
	lock, report, err := environment.Verify(*directory)
	if err != nil {
		return err
	}
	manifest, err := adaptive.BuildCapabilityManifest(report.Blueprint, lock.EnvironmentDigest, lock.ApplicationDigest)
	if err != nil {
		return err
	}
	return writeJSON(stdout, manifest)
}

func appCommand(arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: interceptor app <build|verify> [options]")
	}
	flags := flag.NewFlagSet("app "+arguments[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	directory := flags.String("environment", "", "Environment Blueprint directory")
	tag := flags.String("tag", "", "wrapper image tag")
	image := flags.String("image", "", "wrapper image reference")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if *directory == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: interceptor app %s --environment <directory> [--tag|--image <image>]", arguments[0])
	}
	dataRoot, err := session.DataRoot()
	if err != nil {
		return err
	}
	docker := ociruntime.Docker{}
	switch arguments[0] {
	case "build":
		if *tag == "" || *image != "" {
			return errors.New("app build requires --tag")
		}
		lock, err := appimage.Build(context.Background(), *directory, dataRoot, *tag, version, docker)
		if err != nil {
			return err
		}
		if *jsonOutput {
			return writeJSON(stdout, lock)
		}
		fmt.Fprintf(stdout, "Built wrapper image %s (%s)\n", lock.WrapperImage, lock.WrapperDigest)
		return nil
	case "verify":
		environmentLock, report, err := environment.Verify(*directory)
		if err != nil {
			return err
		}
		lock, err := appimage.Load(appimage.LockPath(dataRoot, report.Blueprint))
		if err != nil {
			return err
		}
		if *image != "" && lock.WrapperImage != *image {
			return fmt.Errorf("application lock references %s, not %s", lock.WrapperImage, *image)
		}
		if err := appimage.Verify(context.Background(), lock, environmentLock, docker); err != nil {
			return err
		}
		if err := appimage.VerifyTLSMaterial(dataRoot, report.Blueprint, lock); err != nil {
			return err
		}
		if *jsonOutput {
			return writeJSON(stdout, map[string]any{"valid": true, "lock": lock})
		}
		fmt.Fprintf(stdout, "Verified wrapper image %s (%s)\n", lock.WrapperImage, lock.WrapperDigest)
		return nil
	default:
		return fmt.Errorf("unknown app command %q", arguments[0])
	}
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value may not be empty")
	}
	*values = append(*values, value)
	return nil
}

func evidenceLimitFlag(flags *flag.FlagSet) *string {
	value, configured := os.LookupEnv("INTERCEPTOR_EVIDENCE_MAX_BYTES")
	if !configured {
		value = "4294967296"
	}
	return flags.String("evidence-max-bytes", value, "maximum bytes per evidence archive (default 4294967296)")
}
func runCommand(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	directory := flags.String("environment", "", "Environment Blueprint directory")
	spoolDefault, spoolConfigured := os.LookupEnv("INTERCEPTOR_SPOOL_MAX_BYTES")
	if !spoolConfigured {
		spoolDefault = "536870912"
	}
	evidenceLimit := evidenceLimitFlag(flags)
	spoolLimit := flags.String("spool-max-bytes", spoolDefault, "maximum combined spool bytes (default 536870912)")
	bridgeTransport := flags.String("bridge-transport", "spool", "host/container bridge transport: spool or unix")
	providerType := flags.String("model-provider", bridge.ProviderOpenAIChat, "host model-provider adapter")
	providerURL := flags.String("provider-base-url", os.Getenv("INTERCEPTOR_PROVIDER_BASE_URL"), "fixed provider base URL")
	credentialEnvironment := flags.String("model-api-key-env", "", "host environment variable containing provider credential (defaults to OPENAI_API_KEY for OpenAI providers)")
	credentialConfig := flags.String("credential-config", "", "host secret-store profile and credential-reference YAML")
	credentialID := flags.String("model-credential-id", "", "stable host credential ID used by the model provider")
	production := flags.Bool("production", false, "enforce production credential policy")
	feedbackProfile := flags.String("feedback-profile", "", "immutable adaptive feedback profile (default black-box)")
	awsRegion := flags.String("aws-region", "", "AWS Region override for bedrock-converse")
	awsProfile := flags.String("aws-profile", "", "AWS shared configuration profile for bedrock-converse")
	providerAPIVersion := flags.String("provider-api-version", "", "pinned provider API version (Anthropic only in this release)")
	providerAuthMode := flags.String("provider-auth", "", "provider authentication selector; see docs/cloud-authentication.md")
	var embeddings embeddingsOptions
	flags.StringVar(&embeddings.provider, "embeddings-provider", "", "optional embeddings provider: openai-compatible")
	flags.StringVar(&embeddings.baseURL, "embeddings-base-url", "", "fixed embeddings API base URL")
	flags.StringVar(&embeddings.auth, "embeddings-auth", "", "embeddings authentication: secret-store, api-key-env, or none")
	flags.StringVar(&embeddings.credentialConfig, "embeddings-credential-config", "", "embeddings secret-store configuration")
	flags.StringVar(&embeddings.credentialID, "embeddings-credential-id", "", "embeddings host credential ID")
	flags.StringVar(&embeddings.keyEnv, "embeddings-api-key-env", "", "explicit host embeddings key environment variable")
	flags.Var(&embeddings.models, "allow-embedding-model", "allowed embeddings model (repeatable)")
	var allowedModels stringList
	flags.Var(&allowedModels, "allow-model", "allowed model identifier (repeatable)")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *directory == "" || len(allowedModels) == 0 {
		return errors.New("usage: interceptor run --environment <directory> --model-provider <type> --allow-model <model> [provider options]")
	}
	evidenceMaxBytes, err := evidence.ParseLimit(*evidenceLimit)
	if err != nil {
		return err
	}
	limitValue := *spoolLimit
	spoolMaxBytes, err := filemanagement.ParseSpoolLimit(limitValue)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	defer cancel(nil)
	go func() {
		select {
		case sig := <-signals:
			if sig == os.Interrupt {
				cancel(nil)
			} else {
				cancel(hostlifetime.Interrupted("HOST_SERVICE_INTERRUPTED"))
			}
		case <-ctx.Done():
		}
	}()
	if *awsProfile != "" {
		if *providerType != bridge.ProviderBedrockConverse || (*providerAuthMode != "" && *providerAuthMode != "aws-profile") {
			return errors.New("AWS profile conflicts with provider authentication")
		}
		*providerAuthMode = "aws-profile"
	} else if *providerAuthMode == "aws-profile" {
		return errors.New("aws-profile requires --aws-profile")
	}
	apiKey, err := resolveProviderOptions(ctx, *providerType, *providerURL, *credentialEnvironment, *credentialConfig, *credentialID, *providerAuthMode, *production)
	if err != nil {
		return err
	}
	embeddingKey, err := embeddings.resolve(ctx, *production, resolveStoredCredential)
	if err != nil {
		return err
	}
	err = localapi.Run(ctx, hostrun.Config{
		EmbeddingsProviderType: embeddings.provider, EmbeddingsURL: embeddings.baseURL,
		EmbeddingsAPIKey: embeddingKey, AllowedEmbeddingModels: embeddings.models,
		EnvironmentDirectory: *directory,
		ProviderType:         *providerType, ProviderURL: *providerURL, ProviderAPIKey: apiKey,
		AWSRegion: *awsRegion, AWSProfile: *awsProfile, AllowedModels: allowedModels,
		ProviderAPIVersion: *providerAPIVersion,
		ProviderAuthMode:   *providerAuthMode,
		BridgeTransport:    *bridgeTransport, Docker: ociruntime.Docker{}, Output: stdout,
		FeedbackProfile: *feedbackProfile, SpoolMaxBytes: spoolMaxBytes, EvidenceMaxBytes: evidenceMaxBytes,
	}, exportEvidence)
	return err
}

func validateProviderOptions(providerType, providerURL, credentialEnvironment string) (string, error) {
	return resolveProviderOptions(context.Background(), providerType, providerURL, credentialEnvironment, "", "", "", false)
}

func resolveProviderOptions(ctx context.Context, providerType, providerURL, credentialEnvironment, credentialConfig, credentialID, authMode string, production bool) (string, error) {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	switch providerType {
	case bridge.ProviderBedrockConverse:
		if providerURL != "" || credentialEnvironment != "" || credentialConfig != "" || credentialID != "" || authMode != "" && authMode != "workload-identity" && authMode != "aws-profile" && authMode != "aws-environment" {
			return "", errors.New("bedrock-converse uses the host AWS SDK identity and accepts no HTTP credential or endpoint options")
		}
		return "", nil
	case bridge.ProviderOpenAIChat, bridge.ProviderOpenAIResponses, bridge.ProviderLiteLLM,
		bridge.ProviderAnthropicMessages, bridge.ProviderGeminiAPI, bridge.ProviderVertexGemini, bridge.ProviderAzureOpenAI:
		if strings.TrimSpace(providerURL) == "" {
			return "", fmt.Errorf("--provider-base-url is required for model provider %s", providerType)
		}
	default:
		return "", fmt.Errorf("unsupported model provider %q", providerType)
	}
	if authMode == "" {
		if providerType == bridge.ProviderVertexGemini {
			authMode = "workload-identity"
		} else if providerType == bridge.ProviderLiteLLM && credentialConfig == "" && credentialID == "" && credentialEnvironment == "" {
			authMode = "none"
		} else {
			authMode = "api-key"
		}
	}
	if authMode == "secret-store" {
		authMode = "api-key"
		if credentialID == "" {
			return "", errors.New("secret-store requires --model-credential-id")
		}
	}
	apiKeyProvider := providerType == bridge.ProviderOpenAIChat || providerType == bridge.ProviderOpenAIResponses || providerType == bridge.ProviderLiteLLM || providerType == bridge.ProviderAnthropicMessages || providerType == bridge.ProviderGeminiAPI || providerType == bridge.ProviderAzureOpenAI || providerType == bridge.ProviderVertexGemini
	validAuth := apiKeyProvider && (authMode == "api-key" || authMode == "api-key-env") || providerType == bridge.ProviderLiteLLM && authMode == "none" || providerType == bridge.ProviderAnthropicMessages && authMode == "workload-token" || (providerType == bridge.ProviderVertexGemini || providerType == bridge.ProviderAzureOpenAI) && authMode == "workload-identity"
	validAuth = validAuth || providerType == bridge.ProviderVertexGemini && authMode == "google-adc" || providerType == bridge.ProviderAzureOpenAI && (authMode == "azure-cli" || authMode == "azure-client-secret")
	if !validAuth {
		return "", fmt.Errorf("authentication mode %q is invalid for provider %s", authMode, providerType)
	}
	if authMode == "none" {
		if credentialConfig != "" || credentialID != "" || credentialEnvironment != "" {
			return "", errors.New("unauthenticated provider mode may not be combined with a stored or environment credential")
		}
		return "", nil
	}
	if authMode == "workload-identity" || authMode == "google-adc" || authMode == "azure-cli" || authMode == "azure-client-secret" {
		if credentialConfig != "" || credentialID != "" || credentialEnvironment != "" {
			return "", errors.New("workload identity may not be combined with a stored or environment credential")
		}
		return "", nil
	}
	if credentialEnvironment != "" && credentialID != "" {
		return "", errors.New("choose a stored credential or environment variable, not both")
	}
	if authMode == "api-key-env" && (credentialEnvironment == "" || credentialID != "" || credentialConfig != "") {
		return "", errors.New("api-key-env requires only --model-api-key-env")
	}
	if credentialID != "" {
		if credentialConfig == "" {
			return "", errors.New("--model-credential-id requires --credential-config")
		}
		return resolveStoredCredential(ctx, credentialConfig, credentialID)
	}
	if credentialConfig != "" {
		return "", errors.New("--credential-config requires --model-credential-id")
	}
	if production && authMode != "api-key-env" {
		return "", errors.New("environment-based provider credentials are disabled in production mode; use --model-credential-id")
	}
	if credentialEnvironment == "" && providerType == bridge.ProviderLiteLLM {
		return "", nil
	}
	if credentialEnvironment == "" {
		switch providerType {
		case bridge.ProviderAnthropicMessages:
			credentialEnvironment = "ANTHROPIC_API_KEY"
		case bridge.ProviderGeminiAPI, bridge.ProviderVertexGemini:
			credentialEnvironment = "GEMINI_API_KEY"
		case bridge.ProviderAzureOpenAI:
			credentialEnvironment = "AZURE_OPENAI_API_KEY"
		default:
			credentialEnvironment = "OPENAI_API_KEY"
		}
	}
	apiKey := os.Getenv(credentialEnvironment)
	if apiKey == "" || len(apiKey) > 1<<20 || strings.ContainsAny(apiKey, "\x00\r\n") {
		return "", fmt.Errorf("provider credential environment variable %s is empty", credentialEnvironment)
	}
	return apiKey, nil
}

func resolveStoredCredential(ctx context.Context, credentialConfig, credentialID string) (string, error) {
	return resolveStoredCredentialWithFactories(ctx, credentialConfig, credentialID, nil)
}

func resolveStoredCredentialWithFactories(ctx context.Context, credentialConfig, credentialID string, factories map[string]credentials.BackendFactory) (string, error) {
	config, err := credentials.Load(credentialConfig)
	if err != nil {
		return "", err
	}
	resolver, err := credentials.New(config, factories)
	if err != nil {
		return "", err
	}
	profileID := ""
	for _, reference := range config.Credentials {
		if reference.CredentialID == credentialID {
			profileID = reference.StoreProfileID
			break
		}
	}
	resolved, err := resolver.Resolve(ctx, credentialID)
	audit := credentials.AuditEvent{CredentialID: credentialID, ProfileID: profileID, ResolvedAt: time.Now().UTC(), Outcome: "failed"}
	if err == nil {
		audit.ProfileID = resolved.ProfileID
		audit.BackendVersion = resolved.BackendVersion
		audit.ResolvedAt = resolved.ResolvedAt
		audit.Outcome = "resolved"
	}
	dataRoot, rootErr := session.DataRoot()
	if rootErr != nil {
		return "", rootErr
	}
	if auditErr := credentials.AppendAudit(dataRoot, audit); auditErr != nil {
		return "", fmt.Errorf("record credential resolution audit: %w", auditErr)
	}
	if err != nil {
		return "", err
	}
	return resolved.Value, nil
}

func sandboxdCommand(arguments []string) error {
	if len(arguments) != 0 {
		return errors.New("sandboxd does not accept arguments")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return sandboxd.Run(ctx)
}

func environmentCommand(arguments []string, stdout io.Writer) error {
	if len(arguments) < 2 {
		return errors.New("usage: interceptor environment <init|validate|seal|inspect> <directory>")
	}
	subcommand, directory := arguments[0], arguments[1]
	jsonOutput := false
	for _, argument := range arguments[2:] {
		if argument == "--json" {
			jsonOutput = true
		} else {
			return fmt.Errorf("unknown argument %q", argument)
		}
	}
	switch subcommand {
	case "init":
		if jsonOutput {
			return errors.New("--json is not supported by environment init")
		}
		if err := initializeBlueprint(directory); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Initialized Environment Blueprint in %s\n", directory)
		return nil
	case "validate":
		report, err := environment.Validate(directory)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(stdout, map[string]any{"valid": true, "name": report.Blueprint.Metadata.Name, "version": report.Blueprint.Metadata.Version, "assets": report.Assets, "warnings": report.Warnings})
		}
		fmt.Fprintf(stdout, "Valid Environment Blueprint %s@%s (%d assets)\n", report.Blueprint.Metadata.Name, report.Blueprint.Metadata.Version, len(report.Assets))
		for _, warning := range report.Warnings {
			fmt.Fprintf(stdout, "warning: %s\n", warning)
		}
		return nil
	case "seal":
		lock, report, err := environment.Seal(directory, time.Now())
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(stdout, map[string]any{"lock": lock, "warnings": report.Warnings})
		}
		fmt.Fprintf(stdout, "Sealed Environment %s@%s %s\n", lock.Name, lock.Version, lock.EnvironmentDigest)
		return nil
	case "inspect":
		lock, report, err := environment.Verify(directory)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(stdout, map[string]any{"lock": lock, "blueprint": report.Blueprint, "warnings": report.Warnings})
		}
		fmt.Fprintf(stdout, "%s@%s\n  digest: %s\n  application: %s\n  assets: %d\n", lock.Name, lock.Version, lock.EnvironmentDigest, report.Blueprint.Application.Image, len(lock.Assets))
		return nil
	default:
		return fmt.Errorf("unknown environment command %q", subcommand)
	}
}

func statusCommand(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: interceptor status <session-id> [--json]")
	}
	root, err := session.DataRoot()
	if err != nil {
		return err
	}
	metadata, _, err := session.Load(root, flags.Arg(0))
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(stdout, metadata)
	}
	fmt.Fprintf(stdout, "%s  %s  %s\n", metadata.ID, metadata.Phase, metadata.EnvironmentDigest)
	return nil
}

func listCommand(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: interceptor list [--json]")
	}
	root, err := session.DataRoot()
	if err != nil {
		return err
	}
	items, err := session.List(root)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(stdout, map[string]any{"sessions": items})
	}
	for _, item := range items {
		fmt.Fprintf(stdout, "%s  %-10s  %s\n", item.ID, item.Phase, item.EnvironmentDigest)
	}
	return nil
}

func logsCommand(arguments []string, stdout io.Writer) error {
	if len(arguments) < 2 {
		return errors.New("usage: interceptor logs <verify|tail|export> <session-id> [options]")
	}
	root, err := session.DataRoot()
	if err != nil {
		return err
	}
	metadata, paths, err := session.Load(root, arguments[1])
	if err != nil {
		return err
	}
	switch arguments[0] {
	case "tail":
		flags := flag.NewFlagSet("logs tail", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		channel := flags.String("channel", "attack", "event channel")
		if err := flags.Parse(arguments[2:]); err != nil || flags.NArg() != 0 {
			return errors.New("usage: interceptor logs tail <session-id> [--channel attack|control|evaluation]")
		}
		if *channel != "attack" && *channel != "control" && *channel != "evaluation" {
			return errors.New("invalid event channel")
		}
		events, err := recording.VerifyFile(paths.Events)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(stdout)
		for _, event := range events {
			if string(event.Channel) == *channel {
				if err := encoder.Encode(event); err != nil {
					return err
				}
			}
		}
		return nil
	case "export":
		flags := flag.NewFlagSet("logs export", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		output := flags.String("output", "", "output tar path")
		evidenceLimit := evidenceLimitFlag(flags)
		if err := flags.Parse(arguments[2:]); err != nil || flags.NArg() != 0 || *output == "" {
			return errors.New("usage: interceptor logs export <session-id> --output <path> [--evidence-max-bytes <bytes>]")
		}
		if metadata.Phase != session.Stopped && metadata.Phase != session.Error {
			return fmt.Errorf("evidence export requires a stopped or error session; current phase is %s", metadata.Phase)
		}
		maximumBytes, err := evidence.ParseLimit(*evidenceLimit)
		if err != nil {
			return err
		}
		if err := exportEvidence(context.Background(), root, metadata, paths, *output, maximumBytes); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Exported verified evidence to %s\n", *output)
		return nil
	case "verify":
		if len(arguments) != 2 {
			return errors.New("usage: interceptor logs verify <session-id>")
		}
	default:
		return fmt.Errorf("unknown logs command %q", arguments[0])
	}
	events, err := recording.VerifyFile(paths.Events)
	if err != nil {
		return err
	}
	transactions, err := statejournal.VerifyFile(paths.Journal)
	if err != nil {
		return err
	}
	if len(events) > 0 && events[0].SessionID != metadata.ID || len(transactions) > 0 && transactions[0].SessionID != metadata.ID {
		return errors.New("log session identity mismatch")
	}
	store, err := blobstore.Open(paths.Blobs, metadata.Limits.MaxBlobBytes)
	if err != nil {
		return err
	}
	if err := hostrun.VerifyRestoreSource(metadata, paths, store); err != nil {
		return err
	}
	for _, event := range events {
		references, err := recording.References(event)
		if err != nil {
			return fmt.Errorf("event %d references: %w", event.Seq, err)
		}
		for _, reference := range references {
			if err := store.Verify(reference); err != nil {
				return fmt.Errorf("event %d blob: %w", event.Seq, err)
			}
		}
	}
	for _, transaction := range transactions {
		for _, operation := range transaction.Operations {
			if operation.Kind == "file_upsert" {
				if err := store.Verify(operation.Blob); err != nil {
					return fmt.Errorf("transaction %d blob: %w", transaction.Seq, err)
				}
			}
		}
	}
	checkpoints, err := snapshot.ListCheckpoints(paths.Checkpoints)
	if err != nil {
		return err
	}
	for _, checkpoint := range checkpoints {
		if checkpoint.SourceSessionID != metadata.ID || checkpoint.EnvironmentDigest != metadata.EnvironmentDigest || checkpoint.AppDigest != metadata.AppDigest || checkpoint.JournalSeq > uint64(len(transactions)) || transactions[checkpoint.JournalSeq-1].Hash != checkpoint.JournalHash || checkpoint.EventSeq > uint64(len(events)) {
			return fmt.Errorf("checkpoint %s has inconsistent session or log references", checkpoint.ID)
		}
	}
	lock, _, err := environment.Verify(metadata.EnvironmentPath)
	if err != nil {
		return err
	}
	if lock.EnvironmentDigest != metadata.EnvironmentDigest || lock.ApplicationDigest != metadata.AppDigest {
		return errors.New("session digest does not match Sealed Environment")
	}
	if metadata.OperationAPIVersion != "" {
		if _, err := adaptive.VerifyExecutionJournal(paths.Root, transactions); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "Verified %d events, %d state transactions, and %d checkpoints for %s\n", len(events), len(transactions), len(checkpoints), metadata.ID)
	return nil
}

func exportEvidence(ctx context.Context, dataRoot string, metadata session.Metadata, paths session.Paths, output string, maximumBytes int64) error {
	if _, err := evidence.ResolveLimit(maximumBytes); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := recording.VerifyFile(paths.Events); err != nil {
		return err
	}
	transactions, err := statejournal.VerifyFile(paths.Journal)
	if err != nil {
		return err
	}
	events, _ := recording.VerifyFile(paths.Events)
	checkpoints, err := snapshot.ListCheckpoints(paths.Checkpoints)
	if err != nil {
		return err
	}
	for _, checkpoint := range checkpoints {
		if checkpoint.SourceSessionID != metadata.ID || checkpoint.EnvironmentDigest != metadata.EnvironmentDigest || checkpoint.AppDigest != metadata.AppDigest || checkpoint.JournalSeq > uint64(len(transactions)) || transactions[checkpoint.JournalSeq-1].Hash != checkpoint.JournalHash || checkpoint.EventSeq > uint64(len(events)) {
			return fmt.Errorf("checkpoint %s has inconsistent references", checkpoint.ID)
		}
	}
	fileManifest, err := snapshot.LoadManifest(paths.FileManifest)
	if err != nil {
		return err
	}
	lock, _, err := environment.Verify(metadata.EnvironmentPath)
	if err != nil || lock.EnvironmentDigest != metadata.EnvironmentDigest || lock.ApplicationDigest != metadata.AppDigest {
		return errors.New("session does not match its Sealed Environment")
	}
	store, err := blobstore.Open(paths.Blobs, metadata.Limits.MaxBlobBytes)
	if err != nil {
		return err
	}
	if err := hostrun.VerifyRestoreSource(metadata, paths, store); err != nil {
		return err
	}
	artifactRegistry, err := adaptive.OpenArtifactRegistry(filepath.Join(paths.Root, "artifacts.json"), store, metadata.Limits.MaxBlobBytes)
	if err != nil {
		return err
	}
	attemptRegistry, err := adaptive.OpenAttemptRegistry(filepath.Join(paths.Root, "attempts.json"), metadata.FeedbackProfile, 100000, artifactRegistry)
	if err != nil {
		return err
	}
	finalReport := evaluation.Report{Oracles: []evaluation.OracleResult{}, Injections: []evaluation.InjectionResult{}, Coverage: []string{}}
	evaluationComplete := false
	if content, readErr := os.ReadFile(paths.Evaluation); readErr == nil {
		decoder := json.NewDecoder(strings.NewReader(string(content)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&finalReport); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
			return errors.New("final evaluation is malformed")
		}
		evaluationComplete = true
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	evidenceBundle, err := adaptive.BuildEvidenceBundle(metadata, attemptRegistry.List(), artifactRegistry.List(), events, checkpoints, finalReport, evaluationComplete)
	if err != nil {
		return err
	}
	references := map[string]struct{}{}
	// Initial files may never change, so neither events nor journal deltas
	// necessarily reference their bytes. The exported manifest still does.
	for _, entry := range fileManifest.Entries {
		if entry.Type == "file" {
			references[entry.Blob] = struct{}{}
		}
	}
	for _, event := range events {
		eventReferences, err := recording.References(event)
		if err != nil {
			return err
		}
		for _, reference := range eventReferences {
			references[reference] = struct{}{}
		}
	}
	for _, transaction := range transactions {
		for _, operation := range transaction.Operations {
			if operation.Kind == "file_upsert" {
				references[operation.Blob] = struct{}{}
			}
		}
	}
	for _, artifact := range artifactRegistry.List() {
		references[artifact.Digest] = struct{}{}
	}
	var executionBytes []byte
	if metadata.OperationAPIVersion != "" {
		executionBytes, err = adaptive.VerifyExecutionJournal(paths.Root, transactions)
		if err != nil {
			return err
		}
		var execution adaptive.ExecutionState
		if err := json.Unmarshal(executionBytes, &execution); err != nil {
			return err
		}
		evidenceBundle, err = adaptive.BindExecutionEvidence(evidenceBundle, execution)
		if err != nil {
			return err
		}
	}
	directory := filepath.Dir(output)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".interceptor-export-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	defer temporary.Close()
	limited, err := evidence.NewWriter(ctx, temporary, maximumBytes)
	if err != nil {
		return err
	}
	archive := tar.NewWriter(limited)
	addReader := func(name string, reader io.Reader, size, mode int64) error {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: size, ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		if err := limited.Check(size); err != nil {
			return err
		}
		written, err := io.Copy(archive, reader)
		if err != nil {
			return err
		}
		if written != size {
			return errors.New("evidence member size changed during export")
		}
		return nil
	}
	addFile := func(name, path string, mode int64) error {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("evidence input is not a regular file")
		}
		return addReader(name, file, info.Size(), mode)
	}
	addBytes := func(name string, content []byte, mode int64) error {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		_, err := archive.Write(content)
		return err
	}
	manifest, _ := json.MarshalIndent(map[string]any{"schema_version": 1, "session_id": metadata.ID, "environment_digest": metadata.EnvironmentDigest, "application_digest": metadata.AppDigest, "interceptor_version": version}, "", "  ")
	if err := addBytes("manifest.json", append(manifest, '\n'), 0o640); err != nil {
		return err
	}
	if executionBytes != nil {
		if err := addBytes("execution.json", executionBytes, 0600); err != nil {
			return err
		}
	}
	bundleBytes, _ := json.MarshalIndent(evidenceBundle, "", "  ")
	if err := addBytes("evidence-bundle.json", append(bundleBytes, '\n'), 0o640); err != nil {
		return err
	}
	for _, file := range []struct{ name, path string }{{"session.json", paths.Metadata}, {"events.jsonl", paths.Events}, {"state.jsonl", paths.Journal}, {"file-manifest.json", paths.FileManifest}} {
		if err := addFile(file.name, file.path, 0o640); err != nil {
			return err
		}
	}
	if evaluationComplete {
		content, err := os.ReadFile(paths.Evaluation)
		if err != nil {
			return err
		}
		if err := addBytes("final-evaluation.json", content, 0o640); err != nil {
			return err
		}
	}
	for _, name := range []string{"artifacts.json", "attempts.json"} {
		content, readErr := os.ReadFile(filepath.Join(paths.Root, name))
		if readErr == nil {
			if err := addBytes(name, content, 0o640); err != nil {
				return err
			}
		} else if !os.IsNotExist(readErr) {
			return readErr
		}
	}
	if metadata.ParentSessionID != "" {
		for _, name := range []string{"checkpoint.json", "state.jsonl"} {
			content, err := os.ReadFile(filepath.Join(paths.Root, "restore-source", name))
			if err != nil {
				return err
			}
			if err := addBytes("restore-source/"+name, content, 0600); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"host-interruption.json", "transport-failure.json", "cleanup-pending.json"} {
		path := filepath.Join(paths.Root, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 4096 {
			return errors.New("invalid host failure record")
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := addBytes(name, content, 0600); err != nil {
			return err
		}
	}
	checkpointFiles, err := os.ReadDir(paths.Checkpoints)
	if err != nil {
		return err
	}
	for _, entry := range checkpointFiles {
		if entry.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(paths.Checkpoints, entry.Name()))
		if err != nil {
			return err
		}
		if err := addBytes("checkpoints/"+entry.Name(), content, 0o640); err != nil {
			return err
		}
	}
	orderedReferences := make([]string, 0, len(references))
	for reference := range references {
		orderedReferences = append(orderedReferences, reference)
	}
	sort.Strings(orderedReferences)
	for _, reference := range orderedReferences {
		reader, object, err := store.Open(reference)
		if err != nil {
			return err
		}
		name := "blobs/sha256/" + strings.TrimPrefix(reference, "sha256:")
		copyErr := addReader(name, reader, object.Size, 0o640)
		closeErr := reader.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, output); err != nil {
		return err
	}
	return fsutil.SyncDir(directory)
}

func snapshotCommand(arguments []string, stdout io.Writer) error {
	if len(arguments) < 2 {
		return errors.New("usage: interceptor snapshot <create|list|inspect> <session-id> [checkpoint-id]")
	}
	subcommand, sessionID := arguments[0], arguments[1]
	dataRoot, err := session.DataRoot()
	if err != nil {
		return err
	}
	_, paths, err := session.Load(dataRoot, sessionID)
	if err != nil {
		return err
	}
	switch subcommand {
	case "create":
		flags := flag.NewFlagSet("snapshot create", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		label := flags.String("name", "", "checkpoint label")
		description := flags.String("description", "", "checkpoint purpose (up to 4096 UTF-8 bytes)")
		campaignID := flags.String("campaign-id", "", "campaign attribution for an administrative snapshot")
		maximumBytes := flags.Int64("maximum-committed-bytes", 0, "optional snapshot byte allowance; zero is exhausted")
		jsonOutput := flags.Bool("json", false, "emit JSON")
		if err := flags.Parse(arguments[2:]); err != nil || flags.NArg() != 0 {
			return errors.New("usage: interceptor snapshot create <session-id> [--name <label>] [--description <text>] [--campaign-id <id>] [--maximum-committed-bytes <bytes>] [--json]")
		}
		if err := snapshot.ValidateMetadata(*campaignID, *description); err != nil {
			return err
		}
		if *maximumBytes < 0 {
			return errors.New("maximum-committed-bytes must be nonnegative")
		}
		input := struct {
			Label                 string                 `json:"label,omitempty"`
			Description           string                 `json:"description,omitempty"`
			CampaignID            string                 `json:"campaign_id,omitempty"`
			MaximumCommittedBytes snapshot.ByteAllowance `json:"maximum_committed_bytes,omitzero"`
		}{Label: *label, Description: *description, CampaignID: *campaignID}
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "maximum-committed-bytes" {
				input.MaximumCommittedBytes = snapshot.ByteAllowance{Value: *maximumBytes, Set: true}
			}
		})
		body, _ := json.Marshal(input)
		result, err := hostrun.CallSession(context.Background(), sessionID, http.MethodPost, "/v1/snapshots", body)
		if err != nil {
			return err
		}
		if *jsonOutput {
			_, err = stdout.Write(result)
			return err
		}
		var checkpoint snapshot.Checkpoint
		if err := json.Unmarshal(result, &checkpoint); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Created snapshot %s at journal sequence %d\n", checkpoint.ID, checkpoint.JournalSeq)
		return nil
	case "list":
		flags := flag.NewFlagSet("snapshot list", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		jsonOutput := flags.Bool("json", false, "emit JSON")
		if err := flags.Parse(arguments[2:]); err != nil || flags.NArg() != 0 {
			return errors.New("usage: interceptor snapshot list <session-id> [--json]")
		}
		items, err := snapshot.ListCheckpoints(paths.Checkpoints)
		if err != nil {
			return err
		}
		if *jsonOutput {
			return writeJSON(stdout, map[string]any{"checkpoints": items})
		}
		for _, item := range items {
			fmt.Fprintf(stdout, "%s  seq=%d  campaign=%q  label=%q  description=%q\n", item.ID, item.JournalSeq, item.CampaignID, item.Label, item.Description)
		}
		return nil
	case "inspect":
		if len(arguments) != 3 {
			return errors.New("usage: interceptor snapshot inspect <session-id> <checkpoint-id>")
		}
		item, err := snapshot.LoadCheckpoint(paths.Checkpoints, arguments[2])
		if err != nil {
			return err
		}
		return writeJSON(stdout, item)
	default:
		return fmt.Errorf("unknown snapshot command %q", subcommand)
	}
}

func identityCommand(arguments []string, stdout io.Writer) error {
	if len(arguments) < 2 || arguments[0] != "caller-token" {
		return errors.New("usage: interceptor identity caller-token <session-id> --principal <id> [--issuer <id>] [--audience <value>] [--scope <scope>]")
	}
	sessionID := arguments[1]
	flags := flag.NewFlagSet("identity caller-token", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	principal := flags.String("principal", "", "synthetic user principal")
	issuer := flags.String("issuer", "", "synthetic issuer ID; defaults to identity.inbound.issuer")
	audience := flags.String("audience", "", "token audience; defaults to identity.inbound.audience")
	var scopes stringList
	flags.Var(&scopes, "scope", "caller token scope (repeatable)")
	if err := flags.Parse(arguments[2:]); err != nil || flags.NArg() != 0 || *principal == "" {
		return errors.New("usage: interceptor identity caller-token <session-id> --principal <id> [--issuer <id>] [--audience <value>] [--scope <scope>]")
	}
	body, _ := json.Marshal(map[string]any{"issuer": *issuer, "principal": *principal, "audience": *audience, "scopes": scopes})
	result, err := hostrun.CallSession(context.Background(), sessionID, http.MethodPost, "/v1/identity/caller-tokens", body)
	if err != nil {
		return err
	}
	_, err = stdout.Write(result)
	return err
}

func stopCommand(arguments []string, stdout io.Writer) error {
	if len(arguments) != 1 {
		return errors.New("usage: interceptor stop <session-id>")
	}
	root, err := session.DataRoot()
	if err != nil {
		return err
	}
	m, paths, err := session.Load(root, arguments[0])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	docker, containerID, err := (ociruntime.Docker{}).ForSession(ctx, root, m.ID)
	if err != nil {
		return err
	}
	present, err := docker.ContainerPresent(ctx, containerID)
	if err != nil {
		return err
	}
	if present {
		cooperative, stop := context.WithTimeout(ctx, 3*time.Second)
		_, _ = hostrun.CallSession(cooperative, m.ID, http.MethodPost, "/v1/stop", []byte(`{}`))
		stop()
	}
	if err := docker.EnsureRemoved(ctx, containerID, false); err != nil {
		return err
	}
	for {
		err = filemanagement.CleanupSpools(ctx, root, m, docker)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("container stopped; spool cleanup pending: %w", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	// Preserve the supervisor's final counters and any terminal fault reason.
	m, _, err = session.Load(root, m.ID)
	if err != nil {
		return err
	}
	if m.Phase != session.Error {
		m.Phase = session.Stopped
	}
	if err := session.Save(paths.Metadata, m); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Stopped session %s; spool directories removed\n", m.ID)
	return nil
}

func deleteCommand(arguments []string, stdout io.Writer) error {
	if len(arguments) != 1 {
		return errors.New("usage: interceptor delete <session-id>")
	}
	return purgeCommand([]string{"--session", arguments[0]}, stdout)
}
func purgeCommand(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("purge", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	id := flags.String("session", "", "session ID")
	campaign := flags.String("campaign", "", "campaign ID")
	all := flags.Bool("all", false, "all retained evidence")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("purge accepts only --session, --campaign, or --all")
	}
	root, err := session.DataRoot()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return filemanagement.Purge(ctx, root, filemanagement.PurgeSelection{SessionID: *id, CampaignID: *campaign, All: *all}, ociruntime.Docker{}, stdout)
}

func sessionAPICommand(arguments []string, stdout io.Writer, apiPath string) error {
	if len(arguments) < 1 || len(arguments) > 2 || len(arguments) == 2 && arguments[1] != "--json" {
		return errors.New("usage: interceptor evaluate <session-id> [--json]")
	}
	result, err := hostrun.CallSession(context.Background(), arguments[0], http.MethodGet, apiPath, nil)
	if err != nil {
		return err
	}
	_, err = stdout.Write(result)
	return err
}

func coverageCommand(arguments []string, stdout io.Writer) error {
	if len(arguments) != 1 {
		return errors.New("usage: interceptor coverage <session-id>")
	}
	result, err := hostrun.CallSession(context.Background(), arguments[0], http.MethodGet, "/v1/evaluation", nil)
	if err != nil {
		return err
	}
	var report struct {
		Coverage []string `json:"coverage_gaps"`
	}
	if err := json.Unmarshal(result, &report); err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"coverage_gaps": report.Coverage, "count": len(report.Coverage)})
}

func initializeBlueprint(directory string) error {
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("create Blueprint directory: %w", err)
	}
	path := filepath.Join(directory, environment.BlueprintFile)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("refusing to overwrite %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	template := strings.ReplaceAll(`api_version: interceptor.dev/v1alpha1
kind: Environment

metadata:
  name: example-agent
  version: 0.1.0
  description: Minimal synthetic environment
  reviewed_by: local-operator
  reviewed_at: REVIEWED_AT

application:
  image: example/agent@sha256:0000000000000000000000000000000000000000000000000000000000000000
  launch:
    command: ["/app/agent"]
    workdir: /app
    environment:
      MODEL_BASE_URL: http://127.0.0.1:18080/v1
  interface:
    kind: http
    operations:
      - id: invoke
        method: POST
        path: /invoke
    http:
      listen: http://127.0.0.1:8080
      health_path: /healthz
    startup_timeout_ms: 30000
    turn_timeout_ms: 120000
  persistence:
    snapshot_capable: false
    quiesce:
      kind: none

time:
  base: 2026-01-01T00:00:00Z
  mode: fixed

limits:
  max_turns: 200
  max_tokens: 5000000
  max_wall_seconds: 3600
  max_snapshots: 20
  max_blob_bytes: 104857600
`, "REVIEWED_AT", now)
	return fsutil.WriteFile(path, []byte(template), 0o640)
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func usage(writer io.Writer) {
	fmt.Fprintln(writer, `Usage: interceptor <command>

Commands:
  environment init|validate|seal|inspect
  credentials check --live --credential-config FILE --credential-id ID --probe-id ID --output FILE
  app build|verify
  run --environment <directory> --model-provider <type> --allow-model <model> [provider options]
  capabilities --environment <directory>
  list
  status <session-id>
  stop <session-id>
  purge --session <session-id> | --campaign <campaign-id> | --all
  delete <session-id>
  snapshot create|list|inspect
  identity caller-token <session-id> --principal <id>
  evaluate <session-id>
  coverage <session-id>
  logs verify|tail|export <session-id>
  version`)
}
