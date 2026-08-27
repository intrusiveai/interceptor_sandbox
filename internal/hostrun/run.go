package hostrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/appimage"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/ociruntime"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

type Config struct {
	EnvironmentDirectory string
	ApplicationPort      int
	ControlPort          int
	ProviderType         string
	ProviderURL          string
	ProviderAPIKey       string
	AWSRegion            string
	AWSProfile           string
	AllowedModels        []string
	BridgeTransport      string
	Docker               ociruntime.Docker
	Output               io.Writer
	OnReady              func(session.Metadata) error
}

type Running struct {
	Metadata session.Metadata
	cleanup  func(context.Context) error
}

func Run(ctx context.Context, config Config) (session.Metadata, error) {
	transport, err := normalizeBridgeTransport(config.BridgeTransport)
	if err != nil {
		return session.Metadata{}, err
	}
	config.BridgeTransport = transport
	lock, report, err := environment.Verify(config.EnvironmentDirectory)
	if err != nil {
		return session.Metadata{}, err
	}
	dataRoot, err := session.DataRoot()
	if err != nil {
		return session.Metadata{}, err
	}
	applicationLock, err := appimage.Load(appimage.LockPath(dataRoot, report.Blueprint))
	if err != nil {
		return session.Metadata{}, fmt.Errorf("load application lock; run 'interceptor app build' first: %w", err)
	}
	if err := config.Docker.Available(ctx); err != nil {
		return session.Metadata{}, err
	}
	if err := appimage.Verify(ctx, applicationLock, lock, config.Docker); err != nil {
		return session.Metadata{}, err
	}
	metadata, err := session.NewMetadata(config.EnvironmentDirectory, lock, report.Blueprint, time.Now())
	if err != nil {
		return session.Metadata{}, err
	}
	metadata.BridgeTransport = transport
	paths := session.PathsFor(dataRoot, metadata.ID)
	for _, directory := range []string{paths.Root, paths.Checkpoints, paths.Generations, paths.Logs, paths.Sockets} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return session.Metadata{}, err
		}
	}
	roots, err := session.Materialize(config.EnvironmentDirectory, report.Blueprint, paths, metadata.Generation)
	if err != nil {
		return session.Metadata{}, err
	}
	blobs, err := blobstore.Open(filepath.Join(dataRoot, "blobs"), report.Blueprint.Limits.MaxBlobBytes)
	if err != nil {
		return session.Metadata{}, err
	}
	baseline := snapshot.EmptyManifest()
	if len(roots) > 0 {
		baseline, err = snapshot.Scan(ctx, roots, blobs, report.Blueprint.Limits.MaxBlobBytes)
		if err != nil {
			return session.Metadata{}, err
		}
	}
	if err := snapshot.WriteManifest(paths.FileManifest, baseline); err != nil {
		return session.Metadata{}, err
	}
	_, events, journal, err := session.Initialize(dataRoot, metadata, baseline.Digest)
	if err != nil {
		return session.Metadata{}, err
	}
	_ = events.Close()
	_ = journal.Close()
	return runPrepared(ctx, config, dataRoot, report.Blueprint, applicationLock, metadata, paths, roots)
}

func Restore(ctx context.Context, config Config, sourceSessionID, checkpointID string) (session.Metadata, error) {
	transport, err := normalizeBridgeTransport(config.BridgeTransport)
	if err != nil {
		return session.Metadata{}, err
	}
	config.BridgeTransport = transport
	dataRoot, err := session.DataRoot()
	if err != nil {
		return session.Metadata{}, err
	}
	source, sourcePaths, err := session.Load(dataRoot, sourceSessionID)
	if err != nil {
		return session.Metadata{}, err
	}
	checkpoint, err := snapshot.LoadCheckpoint(sourcePaths.Checkpoints, checkpointID)
	if err != nil {
		return session.Metadata{}, err
	}
	if checkpoint.SourceSessionID != source.ID || checkpoint.EnvironmentDigest != source.EnvironmentDigest || checkpoint.AppDigest != source.AppDigest {
		return session.Metadata{}, errors.New("checkpoint identity does not match source session")
	}
	if checkpoint.InterceptorVersion != buildinfo.Version || checkpoint.JournalSchemaVersion != 1 {
		return session.Metadata{}, fmt.Errorf("checkpoint version %s/schema %d is incompatible with %s", checkpoint.InterceptorVersion, checkpoint.JournalSchemaVersion, buildinfo.Version)
	}
	environmentLock, report, err := environment.Verify(source.EnvironmentPath)
	if err != nil {
		return session.Metadata{}, err
	}
	if environmentLock.EnvironmentDigest != checkpoint.EnvironmentDigest {
		return session.Metadata{}, errors.New("checkpoint Sealed Environment is unavailable or changed")
	}
	applicationLock, err := appimage.Load(appimage.LockPath(dataRoot, report.Blueprint))
	if err != nil {
		return session.Metadata{}, err
	}
	if err := config.Docker.Available(ctx); err != nil {
		return session.Metadata{}, err
	}
	if err := appimage.Verify(ctx, applicationLock, environmentLock, config.Docker); err != nil {
		return session.Metadata{}, err
	}
	transactions, err := statejournal.VerifyFile(sourcePaths.Journal)
	if err != nil {
		return session.Metadata{}, err
	}
	if checkpoint.JournalSeq > uint64(len(transactions)) || transactions[checkpoint.JournalSeq-1].Hash != checkpoint.JournalHash {
		return session.Metadata{}, errors.New("checkpoint journal head is missing or inconsistent")
	}
	blobs, err := blobstore.Open(filepath.Join(dataRoot, "blobs"), report.Blueprint.Limits.MaxBlobBytes)
	if err != nil {
		return session.Metadata{}, err
	}
	if err := snapshot.VerifyBlobs(blobs, transactions, checkpoint.JournalSeq); err != nil {
		return session.Metadata{}, err
	}
	metadata, err := session.NewMetadata(source.EnvironmentPath, environmentLock, report.Blueprint, time.Now())
	if err != nil {
		return session.Metadata{}, err
	}
	metadata.ParentCheckpoint = checkpoint.ID
	metadata.BridgeTransport = transport
	paths := session.PathsFor(dataRoot, metadata.ID)
	for _, directory := range []string{paths.Root, paths.Checkpoints, paths.Generations, paths.Logs, paths.Sockets} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return metadata, err
		}
	}
	roots, err := session.Materialize(source.EnvironmentPath, report.Blueprint, paths, metadata.Generation)
	if err != nil {
		return metadata, err
	}
	baseline := snapshot.EmptyManifest()
	if len(roots) > 0 {
		baseline, err = snapshot.Scan(ctx, roots, blobs, report.Blueprint.Limits.MaxBlobBytes)
		if err != nil {
			return metadata, err
		}
	}
	if err := snapshot.WriteManifest(paths.FileManifest, baseline); err != nil {
		return metadata, err
	}
	_, events, journal, err := session.Initialize(dataRoot, metadata, baseline.Digest)
	if err != nil {
		return metadata, err
	}
	defer events.Close()
	defer journal.Close()
	for _, transaction := range transactions[:checkpoint.JournalSeq] {
		operations := make([]statejournal.Operation, 0, len(transaction.Operations))
		for _, operation := range transaction.Operations {
			if operation.Kind == "environment_materialized" || operation.Kind == "restored_from" {
				continue
			}
			operations = append(operations, operation)
			if operation.Kind == "budget_set" {
				_ = json.Unmarshal(operation.Value, &metadata.Consumed)
			}
		}
		if len(operations) > 0 {
			if _, err := journal.Append(fmt.Sprintf("restore_source_%d", transaction.Seq), operations); err != nil {
				return metadata, err
			}
		}
	}
	if len(roots) > 0 {
		if err := snapshot.ReplayFilesystem(ctx, roots, blobs, transactions, checkpoint.JournalSeq); err != nil {
			return metadata, err
		}
	}
	restoredManifest := snapshot.EmptyManifest()
	if len(roots) > 0 {
		restoredManifest, err = snapshot.Scan(ctx, roots, blobs, report.Blueprint.Limits.MaxBlobBytes)
		if err != nil {
			return metadata, err
		}
	}
	if restoredManifest.Digest != checkpoint.FileManifestDigest {
		return metadata, fmt.Errorf("restored file manifest mismatch: expected %s, got %s", checkpoint.FileManifestDigest, restoredManifest.Digest)
	}
	if err := snapshot.WriteManifest(paths.FileManifest, restoredManifest); err != nil {
		return metadata, err
	}
	restoredFrom, _ := json.Marshal(map[string]any{"checkpoint_id": checkpoint.ID, "source_session_id": source.ID, "journal_seq": checkpoint.JournalSeq, "journal_hash": checkpoint.JournalHash})
	if _, err := journal.Append("restored_from", []statejournal.Operation{{Kind: "restored_from", Value: restoredFrom}}); err != nil {
		return metadata, err
	}
	if err := session.Save(paths.Metadata, metadata); err != nil {
		return metadata, err
	}
	if err := events.Close(); err != nil {
		return metadata, err
	}
	if err := journal.Close(); err != nil {
		return metadata, err
	}
	config.EnvironmentDirectory = source.EnvironmentPath
	return runPrepared(ctx, config, dataRoot, report.Blueprint, applicationLock, metadata, paths, roots)
}

func runPrepared(ctx context.Context, config Config, dataRoot string, blueprint *environment.Blueprint, applicationLock appimage.Lock, metadata session.Metadata, paths session.Paths, roots []snapshot.Root) (session.Metadata, error) {
	applicationListener, err := listenLoopback(config.ApplicationPort)
	if err != nil {
		return metadata, err
	}
	defer applicationListener.Close()
	controlListener, err := listenLoopback(config.ControlPort)
	if err != nil {
		return metadata, err
	}
	defer controlListener.Close()
	metadata.ApplicationURL = "http://" + applicationListener.Addr().String()
	metadata.ControlURL = "http://" + controlListener.Addr().String()
	containerName := "interceptor-" + metadata.ID
	metadata.ContainerID = containerName
	if err := session.Transition(&metadata, session.Starting); err != nil {
		return metadata, err
	}
	if err := session.Save(paths.Metadata, metadata); err != nil {
		return metadata, err
	}
	provider, err := modelProvider(ctx, config)
	if err != nil {
		return metadata, err
	}
	connector, err := bridge.NewHostConnector(bridge.HostConnectorConfig{
		SessionID: metadata.ID, Provider: provider,
		AllowedModel: config.AllowedModels, MaxBodyBytes: blueprint.Limits.MaxBlobBytes,
	})
	if err != nil {
		return metadata, err
	}
	bridgeContext, stopBridge := context.WithCancel(ctx)
	defer stopBridge()
	bridgeErrors := make(chan error, 1)
	if config.BridgeTransport == "unix" {
		connectorServer, connectorListener, listenErr := bridge.ListenUnix(filepath.Join(paths.Sockets, "model.sock"), connector)
		if listenErr != nil {
			return metadata, listenErr
		}
		defer connectorServer.Close()
		defer connectorListener.Close()
		go func() { bridgeErrors <- connectorServer.Serve(connectorListener) }()
	} else {
		go func() {
			bridgeErrors <- bridge.ServeSpool(bridgeContext, filepath.Join(paths.Sockets, "model-spool"), connector)
		}()
	}
	mounts := []ociruntime.Mount{
		{Source: absolute(config.EnvironmentDirectory), Target: "/opt/interceptor/environment", ReadOnly: true},
		{Source: paths.Root, Target: "/run/interceptor-session"},
		{Source: filepath.Join(dataRoot, "blobs"), Target: "/run/interceptor-blobs"},
	}
	for _, root := range roots {
		mounts = append(mounts, ociruntime.Mount{Source: root.HostPath, Target: root.ContainerPath})
	}
	for _, seed := range blueprint.Filesystem.Seed {
		if seed.ReadOnly {
			mounts = append(mounts, ociruntime.Mount{Source: filepath.Join(absolute(config.EnvironmentDirectory), filepath.FromSlash(seed.Source)), Target: seed.Target, ReadOnly: true})
		}
	}
	hostnames := []string{}
	for _, service := range blueprint.Services {
		hostnames = append(hostnames, service.Hostnames...)
	}
	for _, service := range blueprint.AttackerServices {
		hostnames = append(hostnames, service.Hostnames...)
	}
	for _, issuer := range blueprint.Identity.Issuers {
		hostnames = append(hostnames, issuer.Hostnames...)
	}
	containerID, err := config.Docker.Run(ctx, ociruntime.RunConfig{
		Name: containerName, Image: applicationLock.WrapperImage, User: "0:0",
		Environment: map[string]string{
			"INTERCEPTOR_HOST_UID": strconv.Itoa(os.Getuid()), "INTERCEPTOR_HOST_GID": strconv.Itoa(os.Getgid()),
			"INTERCEPTOR_APP_UID": "20000", "INTERCEPTOR_APP_GID": strconv.Itoa(os.Getgid()),
			"INTERCEPTOR_BRIDGE_TRANSPORT": config.BridgeTransport,
		},
		Mounts: mounts, Hostnames: hostnames, PIDs: metadata.Limits.MaxPIDs, MemoryBytes: metadata.Limits.MemoryBytes,
		CPUs: metadata.Limits.CPUs, ReadOnly: true, NetworkNone: true, NoNewPrivileges: true, DropAllCaps: true,
		AddCaps: []string{"SETUID", "SETGID", "KILL"}, TmpfsBytes: 64 << 20,
	})
	if err != nil {
		metadata.Phase, metadata.TerminationReason = session.Error, err.Error()
		_ = session.Save(paths.Metadata, metadata)
		return metadata, err
	}
	if err := waitReady(ctx, paths.Metadata, config.Docker, containerID, time.Duration(blueprint.Application.Interface.StartupTimeoutMS)*time.Millisecond); err != nil {
		logs, _ := config.Docker.Logs(context.Background(), containerID)
		metadata.Phase, metadata.TerminationReason = session.Error, err.Error()
		_ = session.Save(paths.Metadata, metadata)
		_ = config.Docker.Stop(context.Background(), containerID, 5*time.Second)
		return metadata, fmt.Errorf("session startup failed: %w\n%s", err, logs)
	}
	metadata, _, _ = session.Load(dataRoot, metadata.ID)
	var applicationServer, controlServer *http.Server
	var spoolClient *bridge.HTTPSpoolClient
	if config.BridgeTransport == "unix" {
		if _, probeErr := callUnix(ctx, filepath.Join(paths.Sockets, "control.sock"), http.MethodGet, "/v1/status", nil); probeErr != nil {
			metadata.Phase, metadata.TerminationReason = session.Error, "selected Unix bridge is unavailable: "+probeErr.Error()
			_ = session.Save(paths.Metadata, metadata)
			_ = config.Docker.Stop(context.Background(), containerID, 5*time.Second)
			_ = config.Docker.Remove(context.Background(), containerID)
			return metadata, errors.New(metadata.TerminationReason)
		}
		applicationServer = unixProxyServer(applicationListener, filepath.Join(paths.Sockets, "application.sock"), nil)
		controlServer = unixProxyServer(controlListener, filepath.Join(paths.Sockets, "control.sock"), cooperativeRoute)
	} else {
		client, clientErr := bridge.NewHTTPSpoolClient(filepath.Join(paths.Sockets, "http-spool"), blueprint.Limits.MaxBlobBytes, time.Duration(blueprint.Application.Interface.TurnTimeoutMS)*time.Millisecond)
		if clientErr != nil {
			return metadata, clientErr
		}
		spoolClient = client
		if _, probeErr := spoolClient.Do(ctx, bridge.ControlChannel, http.MethodGet, "/v1/status", nil, nil); probeErr != nil {
			metadata.Phase, metadata.TerminationReason = session.Error, "default spool bridge is unavailable: "+probeErr.Error()
			_ = session.Save(paths.Metadata, metadata)
			_ = config.Docker.Stop(context.Background(), containerID, 5*time.Second)
			_ = config.Docker.Remove(context.Background(), containerID)
			return metadata, errors.New(metadata.TerminationReason)
		}
		applicationServer = spoolProxyServer(applicationListener, spoolClient, bridge.ApplicationChannel, nil)
		controlServer = spoolProxyServer(controlListener, spoolClient, bridge.ControlChannel, cooperativeRoute)
	}
	defer applicationServer.Close()
	defer controlServer.Close()
	go applicationServer.Serve(applicationListener)
	go controlServer.Serve(controlListener)
	if config.Output != nil {
		fmt.Fprintf(config.Output, "Session %s running\n  application: %s\n  control: %s\n", metadata.ID, metadata.ApplicationURL, metadata.ControlURL)
	}
	if config.OnReady != nil {
		if err := config.OnReady(metadata); err != nil {
			_ = config.Docker.Stop(context.Background(), containerID, 5*time.Second)
			_ = config.Docker.Remove(context.Background(), containerID)
			return metadata, fmt.Errorf("post-start operation failed: %w", err)
		}
	}
	requestedStop := false
	monitor := time.NewTicker(500 * time.Millisecond)
	waiting := true
	for waiting {
		select {
		case <-ctx.Done():
			requestedStop, waiting = true, false
		case bridgeErr := <-bridgeErrors:
			if bridgeErr != nil && !errors.Is(bridgeErr, http.ErrServerClosed) {
				metadata.Phase, metadata.TerminationReason = session.Error, "model connector: "+bridgeErr.Error()
				_ = session.Save(paths.Metadata, metadata)
			}
			waiting = false
		case <-monitor.C:
			running, inspectErr := config.Docker.Running(context.Background(), containerID)
			if inspectErr == nil && !running {
				waiting = false
			} else if inspectErr != nil {
				if latest, _, loadErr := session.Load(dataRoot, metadata.ID); loadErr == nil && (latest.Phase == session.Stopped || latest.Phase == session.Error) {
					waiting = false
				}
			}
		}
	}
	monitor.Stop()
	stopContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if requestedStop {
		_, _ = callControl(stopContext, metadata, paths, http.MethodPost, "/v1/stop", []byte(`{}`))
		_ = config.Docker.Stop(stopContext, containerID, 5*time.Second)
	}
	_ = config.Docker.Remove(stopContext, containerID)
	latest, _, loadErr := session.Load(dataRoot, metadata.ID)
	if loadErr == nil {
		metadata = latest
	}
	return metadata, nil
}

func modelProvider(ctx context.Context, config Config) (bridge.ModelProvider, error) {
	providerType := strings.ToLower(strings.TrimSpace(config.ProviderType))
	if providerType == "" {
		providerType = bridge.ProviderOpenAIChat
	}
	httpConfig := bridge.HTTPProviderConfig{BaseURL: config.ProviderURL, APIKey: config.ProviderAPIKey}
	switch providerType {
	case bridge.ProviderOpenAIChat:
		return bridge.NewOpenAIChatProvider(httpConfig)
	case bridge.ProviderOpenAIResponses:
		return bridge.NewOpenAIResponsesProvider(httpConfig)
	case bridge.ProviderLiteLLM:
		return bridge.NewLiteLLMProvider(httpConfig)
	case bridge.ProviderBedrockConverse:
		return bridge.LoadBedrockConverseProvider(ctx, bridge.BedrockProviderConfig{Region: config.AWSRegion, Profile: config.AWSProfile})
	default:
		return nil, fmt.Errorf("unsupported model provider %q", config.ProviderType)
	}
}

func CallSession(ctx context.Context, sessionID, method, path string, body []byte) ([]byte, error) {
	dataRoot, err := session.DataRoot()
	if err != nil {
		return nil, err
	}
	metadata, paths, err := session.Load(dataRoot, sessionID)
	if err != nil {
		return nil, err
	}
	if metadata.ContainerID == "" {
		return nil, errors.New("session has no running container")
	}
	if metadata.Phase != session.Running && metadata.Phase != session.Quiescing {
		return nil, fmt.Errorf("session is not running: %s", metadata.Phase)
	}
	return callControl(ctx, metadata, paths, method, path, body)
}

func callControl(ctx context.Context, metadata session.Metadata, paths session.Paths, method, path string, body []byte) ([]byte, error) {
	if metadata.BridgeTransport == "unix" {
		return callUnix(ctx, filepath.Join(paths.Sockets, "control.sock"), method, path, body)
	}
	client, err := bridge.NewHTTPSpoolClient(filepath.Join(paths.Sockets, "http-spool"), metadata.Limits.MaxBlobBytes, defaultControlTimeout(metadata))
	if err != nil {
		return nil, err
	}
	response, err := client.Do(ctx, bridge.ControlChannel, method, path, http.Header{"Content-Type": []string{"application/json"}}, body)
	if err != nil {
		return nil, err
	}
	if response.Status < 200 || response.Status >= 300 {
		return nil, fmt.Errorf("control API returned %d: %s", response.Status, strings.TrimSpace(string(response.Body)))
	}
	return response.Body, nil
}

func defaultControlTimeout(metadata session.Metadata) time.Duration {
	timeout := time.Duration(metadata.Limits.MaxWallSeconds) * time.Second
	if timeout <= 0 || timeout > 10*time.Minute {
		return 10 * time.Minute
	}
	return timeout
}

func listenLoopback(port int) (net.Listener, error) {
	if port < 0 || port > 65535 {
		return nil, errors.New("port must be between 0 and 65535")
	}
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

func spoolProxyServer(listener net.Listener, client *bridge.HTTPSpoolClient, channel string, allowed func(*http.Request) bool) *http.Server {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if allowed != nil && !allowed(request) {
			http.NotFound(writer, request)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, (32<<20)+1))
		if err != nil || len(body) > 32<<20 {
			http.Error(writer, "request body exceeds bridge limit", http.StatusRequestEntityTooLarge)
			return
		}
		path := request.URL.RequestURI()
		response, err := client.Do(request.Context(), channel, request.Method, path, request.Header, body)
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			http.Error(writer, "sandbox bridge failed: "+err.Error(), status)
			return
		}
		for name, values := range response.Headers {
			for _, value := range values {
				writer.Header().Add(name, value)
			}
		}
		writer.WriteHeader(response.Status)
		_, _ = writer.Write(response.Body)
	})
	return &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
}

func unixProxyServer(listener net.Listener, socket string, allowed func(*http.Request) bool) *http.Server {
	target, _ := url.Parse("http://unix")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if allowed != nil && !allowed(request) {
			http.NotFound(writer, request)
			return
		}
		proxy.ServeHTTP(writer, request)
	})
	return &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
}

func callUnix(ctx context.Context, socket, method, path string, body []byte) ([]byte, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("control API returned %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func normalizeBridgeTransport(value string) (string, error) {
	if value == "" {
		return "spool", nil
	}
	switch strings.ToLower(value) {
	case "spool":
		return "spool", nil
	case "unix":
		return "unix", nil
	default:
		return "", fmt.Errorf("unsupported bridge transport %q; use spool or unix", value)
	}
}

func cooperativeRoute(request *http.Request) bool {
	path := request.URL.Path
	if request.Method == http.MethodGet && (path == "/v1/status" || path == "/v1/injections" || path == "/v1/snapshots" || path == "/v1/events" || strings.HasPrefix(path, "/v1/turns/") || strings.HasPrefix(path, "/v1/snapshots/") || strings.HasPrefix(path, "/v1/attacker/")) {
		return true
	}
	if request.Method == http.MethodPost && (path == "/v1/turns" || path == "/v1/injections" || path == "/v1/snapshots" || path == "/v1/identity/caller-tokens") {
		return true
	}
	return request.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/injections/")
}

func waitReady(ctx context.Context, metadataPath string, docker ociruntime.Docker, containerID string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		data, err := os.ReadFile(metadataPath)
		if err == nil {
			var value session.Metadata
			if json.Unmarshal(data, &value) == nil {
				if value.Phase == session.Running {
					return nil
				}
				if value.Phase == session.Error {
					return errors.New(value.TerminationReason)
				}
			}
		}
		running, err := docker.Running(ctx, containerID)
		if err == nil && !running {
			return errors.New("sandbox container exited before becoming ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("sandbox startup timed out")
}

func absolute(path string) string {
	value, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return value
}
