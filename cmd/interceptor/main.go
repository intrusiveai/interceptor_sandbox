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
	"strings"
	"syscall"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/appimage"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/hostrun"
	"github.com/intrusive-ai/interceptor-sandbox/internal/ociruntime"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/sandboxd"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

const version = buildinfo.Version

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
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, version)
		return nil
	case "environment":
		return environmentCommand(arguments[1:], stdout)
	case "app":
		return appCommand(arguments[1:], stdout)
	case "run":
		return runCommand(arguments[1:], stdout)
	case "sandboxd":
		return sandboxdCommand(arguments[1:])
	case "app-helper":
		return sandboxd.RunHelper()
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
		return restoreCommand(arguments[1:], stdout)
	case "stop":
		return stopCommand(arguments[1:], stdout)
	case "delete":
		return deleteCommand(arguments[1:], stdout)
	case "evaluate":
		return sessionAPICommand(arguments[1:], stdout, "/v1/evaluation")
	case "coverage":
		return coverageCommand(arguments[1:], stdout)
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", arguments[0])
	}
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

func runCommand(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	directory := flags.String("environment", "", "Environment Blueprint directory")
	applicationPort := flags.Int("app-port", 0, "application bridge port")
	controlPort := flags.Int("control-port", 0, "control bridge port")
	bridgeTransport := flags.String("bridge-transport", "spool", "host/container bridge transport: spool or unix")
	providerType := flags.String("model-provider", bridge.ProviderOpenAIChat, "host provider: openai-chat, openai-responses, bedrock-converse, or litellm")
	providerURL := flags.String("provider-base-url", os.Getenv("INTERCEPTOR_PROVIDER_BASE_URL"), "fixed provider base URL")
	credentialEnvironment := flags.String("model-api-key-env", "", "host environment variable containing provider credential (defaults to OPENAI_API_KEY for OpenAI providers)")
	awsRegion := flags.String("aws-region", "", "AWS Region override for bedrock-converse")
	awsProfile := flags.String("aws-profile", "", "AWS shared configuration profile for bedrock-converse")
	var allowedModels stringList
	flags.Var(&allowedModels, "allow-model", "allowed model identifier (repeatable)")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *directory == "" || len(allowedModels) == 0 {
		return errors.New("usage: interceptor run --environment <directory> --model-provider <type> --allow-model <model> [provider options]")
	}
	apiKey, err := validateProviderOptions(*providerType, *providerURL, *credentialEnvironment)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	_, err = hostrun.Run(ctx, hostrun.Config{
		EnvironmentDirectory: *directory, ApplicationPort: *applicationPort, ControlPort: *controlPort,
		ProviderType: *providerType, ProviderURL: *providerURL, ProviderAPIKey: apiKey,
		AWSRegion: *awsRegion, AWSProfile: *awsProfile, AllowedModels: allowedModels,
		BridgeTransport: *bridgeTransport, Docker: ociruntime.Docker{}, Output: stdout,
	})
	return err
}

func restoreCommand(arguments []string, stdout io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: interceptor restore <session-id> --checkpoint <checkpoint-id> [provider options]")
	}
	sourceSessionID := arguments[0]
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	checkpointID := flags.String("checkpoint", "", "source checkpoint ID")
	applicationPort := flags.Int("app-port", 0, "application bridge port")
	controlPort := flags.Int("control-port", 0, "control bridge port")
	bridgeTransport := flags.String("bridge-transport", "spool", "host/container bridge transport: spool or unix")
	providerType := flags.String("model-provider", bridge.ProviderOpenAIChat, "host provider: openai-chat, openai-responses, bedrock-converse, or litellm")
	providerURL := flags.String("provider-base-url", os.Getenv("INTERCEPTOR_PROVIDER_BASE_URL"), "fixed provider base URL")
	credentialEnvironment := flags.String("model-api-key-env", "", "host environment variable containing provider credential (defaults to OPENAI_API_KEY for OpenAI providers)")
	awsRegion := flags.String("aws-region", "", "AWS Region override for bedrock-converse")
	awsProfile := flags.String("aws-profile", "", "AWS shared configuration profile for bedrock-converse")
	stopSource := flags.Bool("stop-source", false, "stop source after restored session becomes healthy")
	var allowedModels stringList
	flags.Var(&allowedModels, "allow-model", "allowed model identifier (repeatable)")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *checkpointID == "" || len(allowedModels) == 0 {
		return errors.New("usage: interceptor restore <session-id> --checkpoint <id> --model-provider <type> --allow-model <model> [provider options]")
	}
	apiKey, err := validateProviderOptions(*providerType, *providerURL, *credentialEnvironment)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	config := hostrun.Config{
		ApplicationPort: *applicationPort, ControlPort: *controlPort, ProviderType: *providerType, ProviderURL: *providerURL,
		ProviderAPIKey: apiKey, AWSRegion: *awsRegion, AWSProfile: *awsProfile,
		AllowedModels: allowedModels, BridgeTransport: *bridgeTransport, Docker: ociruntime.Docker{}, Output: stdout,
	}
	if *stopSource {
		config.OnReady = func(session.Metadata) error { return stopCommand([]string{sourceSessionID}, stdout) }
	}
	_, err = hostrun.Restore(ctx, config, sourceSessionID, *checkpointID)
	if err != nil {
		return err
	}
	return nil
}

func validateProviderOptions(providerType, providerURL, credentialEnvironment string) (string, error) {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	switch providerType {
	case bridge.ProviderBedrockConverse:
		return "", nil
	case bridge.ProviderOpenAIChat, bridge.ProviderOpenAIResponses, bridge.ProviderLiteLLM:
		if strings.TrimSpace(providerURL) == "" {
			return "", fmt.Errorf("--provider-base-url is required for model provider %s", providerType)
		}
	default:
		return "", fmt.Errorf("unsupported model provider %q", providerType)
	}
	if credentialEnvironment == "" && providerType == bridge.ProviderLiteLLM {
		return "", nil
	}
	if credentialEnvironment == "" {
		credentialEnvironment = "OPENAI_API_KEY"
	}
	apiKey := os.Getenv(credentialEnvironment)
	if apiKey == "" {
		return "", fmt.Errorf("provider credential environment variable %s is empty", credentialEnvironment)
	}
	return apiKey, nil
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
		if err := flags.Parse(arguments[2:]); err != nil || flags.NArg() != 0 || *output == "" {
			return errors.New("usage: interceptor logs export <session-id> --output <path>")
		}
		if metadata.Phase != session.Stopped && metadata.Phase != session.Error {
			return fmt.Errorf("evidence export requires a stopped or error session; current phase is %s", metadata.Phase)
		}
		if err := exportEvidence(root, metadata, paths, *output); err != nil {
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
	store, err := blobstore.Open(filepath.Join(root, "blobs"), metadata.Limits.MaxBlobBytes)
	if err != nil {
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
	fmt.Fprintf(stdout, "Verified %d events, %d state transactions, and %d checkpoints for %s\n", len(events), len(transactions), len(checkpoints), metadata.ID)
	return nil
}

func exportEvidence(dataRoot string, metadata session.Metadata, paths session.Paths, output string) error {
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
	if _, err := snapshot.LoadManifest(paths.FileManifest); err != nil {
		return err
	}
	lock, _, err := environment.Verify(metadata.EnvironmentPath)
	if err != nil || lock.EnvironmentDigest != metadata.EnvironmentDigest || lock.ApplicationDigest != metadata.AppDigest {
		return errors.New("session does not match its Sealed Environment")
	}
	store, err := blobstore.Open(filepath.Join(dataRoot, "blobs"), metadata.Limits.MaxBlobBytes)
	if err != nil {
		return err
	}
	references := map[string]struct{}{}
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
	archive := tar.NewWriter(temporary)
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
	for name, path := range map[string]string{"session.json": paths.Metadata, "events.jsonl": paths.Events, "state.jsonl": paths.Journal, "file-manifest.json": paths.FileManifest} {
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := addBytes(name, content, 0o640); err != nil {
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
	for reference := range references {
		reader, object, err := store.Open(reference)
		if err != nil {
			return err
		}
		content, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || int64(len(content)) != object.Size {
			return errors.New("read evidence blob failed or size changed")
		}
		name := "blobs/sha256/" + strings.TrimPrefix(reference, "sha256:")
		if err := addBytes(name, content, 0o640); err != nil {
			return err
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
		jsonOutput := flags.Bool("json", false, "emit JSON")
		if err := flags.Parse(arguments[2:]); err != nil || flags.NArg() != 0 {
			return errors.New("usage: interceptor snapshot create <session-id> [--name <label>] [--json]")
		}
		body, _ := json.Marshal(map[string]string{"label": *label})
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
			fmt.Fprintf(stdout, "%s  seq=%d  %s\n", item.ID, item.JournalSeq, item.Label)
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
	dataRoot, err := session.DataRoot()
	if err != nil {
		return err
	}
	metadata, _, err := session.Load(dataRoot, arguments[0])
	if err != nil {
		return err
	}
	if metadata.Phase == session.Stopped {
		fmt.Fprintf(stdout, "Session %s is already stopped\n", metadata.ID)
		return nil
	}
	if metadata.ContainerID == "" {
		return errors.New("session has no container identity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, apiErr := hostrun.CallSession(ctx, metadata.ID, http.MethodPost, "/v1/stop", []byte(`{}`))
	docker := ociruntime.Docker{}
	stopErr := docker.Stop(ctx, metadata.ContainerID, 5*time.Second)
	if apiErr != nil && stopErr != nil {
		return fmt.Errorf("control stop failed (%v) and container stop failed (%v)", apiErr, stopErr)
	}
	_ = docker.Remove(ctx, metadata.ContainerID)
	metadata.Phase = session.Stopped
	if err := session.Save(session.PathsFor(dataRoot, metadata.ID).Metadata, metadata); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Stopped session %s\n", metadata.ID)
	return nil
}

func deleteCommand(arguments []string, stdout io.Writer) error {
	if len(arguments) != 1 {
		return errors.New("usage: interceptor delete <session-id>")
	}
	dataRoot, err := session.DataRoot()
	if err != nil {
		return err
	}
	metadata, paths, err := session.Load(dataRoot, arguments[0])
	if err != nil {
		return err
	}
	if metadata.Phase != session.Stopped && metadata.Phase != session.Error {
		return fmt.Errorf("refusing to delete session in phase %s; stop it first", metadata.Phase)
	}
	if err := os.RemoveAll(paths.Root); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Deleted session directory %s (shared blobs were retained)\n", paths.Root)
	return nil
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
  app build|verify
  run --environment <directory> --model-provider <type> --allow-model <model> [provider options]
  list
  status <session-id>
  stop <session-id>
  delete <session-id>
  snapshot create|list|inspect
  identity caller-token <session-id> --principal <id>
  restore <session-id> --checkpoint <checkpoint-id> [--bridge-transport spool|unix]
  evaluate <session-id>
  coverage <session-id>
  logs verify|tail|export <session-id>
  version`)
}
