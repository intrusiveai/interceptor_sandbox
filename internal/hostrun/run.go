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
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/intrusive-ai/interceptor-sandbox/internal/credentials"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/appimage"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/filemanagement"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/hostlifetime"
	"github.com/intrusive-ai/interceptor-sandbox/internal/ociruntime"
	"github.com/intrusive-ai/interceptor-sandbox/internal/postgresql"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

type Config struct {
	EvidenceMaxBytes       int64
	SpoolMaxBytes          int64
	OnFault                func(string, error)
	InternalOnly           bool // Integrated local API owns all public target access.
	EnvironmentDirectory   string
	ApplicationPort        int
	ControlPort            int
	ProviderType           string
	ProviderURL            string
	ProviderAPIKey         string
	EmbeddingsProviderType string
	EmbeddingsURL          string
	EmbeddingsAPIKey       string
	AllowedEmbeddingModels []string
	AWSRegion              string
	AWSProfile             string
	ProviderAPIVersion     string
	ProviderAuthMode       string
	AllowedModels          []string
	BridgeTransport        string
	Docker                 ociruntime.Docker
	Output                 io.Writer
	OnReady                func(session.Metadata) error
	FeedbackProfile        string
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
	config.Docker, err = config.Docker.Pin(ctx)
	if err != nil {
		return session.Metadata{}, err
	}
	if err := config.Docker.Available(ctx); err != nil {
		return session.Metadata{}, err
	}
	if err := appimage.Verify(ctx, applicationLock, lock, config.Docker); err != nil {
		return session.Metadata{}, err
	}
	if err := appimage.VerifyTLSMaterial(dataRoot, report.Blueprint, applicationLock); err != nil {
		return session.Metadata{}, err
	}
	metadata, err := session.NewMetadata(config.EnvironmentDirectory, lock, report.Blueprint, time.Now())
	if err != nil {
		return session.Metadata{}, err
	}
	metadata.BridgeTransport = transport
	metadata.SpoolMaxBytes = config.SpoolMaxBytes
	if metadata.SpoolMaxBytes == 0 {
		metadata.SpoolMaxBytes = filemanagement.DefaultSpoolMaxBytes
	}
	if metadata.SpoolMaxBytes < 0 {
		return metadata, errors.New("spool byte limit must be positive")
	}
	if err := bindAdaptiveMetadata(&metadata, report.Blueprint, config.FeedbackProfile); err != nil {
		return session.Metadata{}, err
	}
	config.Docker, err = config.Docker.PrepareBinding(dataRoot, metadata.ID)
	if err != nil {
		return metadata, err
	}
	paths := session.PathsFor(dataRoot, metadata.ID)
	// Make even a failed preparation discoverable and safely purgeable.
	if err := session.Save(paths.Metadata, metadata); err != nil {
		return metadata, err
	}
	for _, directory := range []string{paths.Root, paths.Checkpoints, paths.Generations, paths.Logs, paths.Sockets} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return session.Metadata{}, err
		}
	}
	roots, err := session.Materialize(config.EnvironmentDirectory, report.Blueprint, paths, metadata.Generation)
	if err != nil {
		return session.Metadata{}, err
	}
	blobs, err := blobstore.Open(paths.Blobs, report.Blueprint.Limits.MaxBlobBytes)
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
	config.Docker, err = config.Docker.Pin(ctx)
	if err != nil {
		return session.Metadata{}, err
	}
	if err := config.Docker.Available(ctx); err != nil {
		return session.Metadata{}, err
	}
	if err := appimage.Verify(ctx, applicationLock, environmentLock, config.Docker); err != nil {
		return session.Metadata{}, err
	}
	if err := appimage.VerifyTLSMaterial(dataRoot, report.Blueprint, applicationLock); err != nil {
		return session.Metadata{}, err
	}
	transactions, err := statejournal.VerifyFile(sourcePaths.Journal)
	if err != nil {
		return session.Metadata{}, err
	}
	if checkpoint.JournalSeq == 0 || checkpoint.JournalSeq > uint64(len(transactions)) || transactions[checkpoint.JournalSeq-1].Hash != checkpoint.JournalHash {
		return session.Metadata{}, errors.New("checkpoint journal head is missing or inconsistent")
	}
	databaseCheckpoints := map[string]bool{}
	for _, tx := range transactions[:checkpoint.JournalSeq] {
		for _, op := range tx.Operations {
			if op.Kind == "database_checkpoint" {
				if err := postgresql.ValidateCheckpoint(op); err != nil {
					return session.Metadata{}, err
				}
				databaseCheckpoints[op.Service] = true
			}
		}
	}
	for _, service := range report.Blueprint.Services {
		if service.Kind == "postgresql" && !databaseCheckpoints[service.ID] {
			return session.Metadata{}, fmt.Errorf("checkpoint lacks database %s", service.ID)
		}
	}
	blobs, err := blobstore.Open(sourcePaths.Blobs, report.Blueprint.Limits.MaxBlobBytes)
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
	metadata.CampaignID = source.CampaignID
	metadata.SpoolMaxBytes = source.SpoolMaxBytes
	if metadata.SpoolMaxBytes == 0 {
		metadata.SpoolMaxBytes = filemanagement.DefaultSpoolMaxBytes
	}
	metadata.ParentCheckpoint = checkpoint.ID
	metadata.ParentSessionID = source.ID
	metadata.BridgeTransport = transport
	if config.FeedbackProfile == "" {
		config.FeedbackProfile = source.FeedbackProfile
	}
	if err := bindAdaptiveMetadata(&metadata, report.Blueprint, config.FeedbackProfile); err != nil {
		return session.Metadata{}, err
	}
	config.Docker, err = config.Docker.PrepareBinding(dataRoot, metadata.ID)
	if err != nil {
		return metadata, err
	}
	paths := session.PathsFor(dataRoot, metadata.ID)
	// Make even a failed preparation discoverable and safely purgeable.
	if err := session.Save(paths.Metadata, metadata); err != nil {
		return metadata, err
	}
	for _, directory := range []string{paths.Root, paths.Checkpoints, paths.Generations, paths.Logs, paths.Sockets} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return metadata, err
		}
	}
	destination, err := blobstore.Open(paths.Blobs, report.Blueprint.Limits.MaxBlobBytes)
	if err != nil {
		return metadata, err
	}
	if err := blobs.CopyTo(ctx, destination); err != nil {
		return metadata, err
	}
	blobs = destination
	if err := preserveRestoreSource(paths, checkpoint, transactions[:checkpoint.JournalSeq]); err != nil {
		return metadata, err
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
	if err := adaptive.RestoreRegistries(paths.Root, transactions[:checkpoint.JournalSeq]); err != nil {
		return metadata, err
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

func runPrepared(ctx context.Context, config Config, dataRoot string, blueprint *environment.Blueprint, applicationLock appimage.Lock, metadata session.Metadata, paths session.Paths, roots []snapshot.Root) (result session.Metadata, runErr error) {
	var err error
	config.Docker, err = config.Docker.Pin(ctx)
	if err != nil {
		return metadata, err
	}
	if config.Docker.SessionID != metadata.ID {
		config.Docker, err = config.Docker.PrepareBinding(dataRoot, metadata.ID)
		if err != nil {
			return metadata, err
		}
	}
	var launchedID atomic.Pointer[string]
	killTarget := func(killCtx context.Context) {
		if id := launchedID.Load(); id != nil {
			_ = config.Docker.Kill(killCtx, *id)
		}
	}
	writer, err := filemanagement.Lock(dataRoot, "writers-"+metadata.ID, false)
	if err != nil {
		return metadata, err
	}
	ctx, interruptRun := context.WithCancelCause(ctx)
	cancelRun := func() { interruptRun(nil) }
	defer cancelRun()
	var fault atomic.Pointer[filemanagement.SpoolFault]
	var workers sync.WaitGroup
	var stopBridge context.CancelFunc = func() {}
	var closeServers = func() {}
	var quotaDone chan struct{}
	var dockerWatchDone chan struct{}
	sessionID := metadata.ID
	var faultOnce sync.Once
	notifyFault := func(err error) {
		faultOnce.Do(func() {
			if config.OnFault != nil {
				config.OnFault(sessionID, err)
			}
		})
	}
	// Termination starts before notifying the API or attempting any persistence.
	terminateFailed := func(err error) {
		go func() {
			killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			killTarget(killCtx)
		}()
		notifyFault(err)
	}
	interruptionDone := make(chan struct{})
	go func() {
		defer close(interruptionDone)
		<-ctx.Done()
		cause := context.Cause(ctx)
		if !hostlifetime.IsInterruption(cause) {
			return
		}
		// Termination must not wait for API persistence, model work, or a spool reply.
		go func() {
			killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			killTarget(killCtx)
		}()
		notifyFault(cause)
	}()
	defer func() {
		cancelRun()
		if quotaDone != nil {
			<-quotaDone
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		interrupted := hostlifetime.IsInterruption(context.Cause(ctx))
		bound, id, err := config.Docker.ForSession(cleanup, dataRoot, metadata.ID)
		if known := launchedID.Load(); known != nil {
			bound, id, err = config.Docker, *known, nil
		}
		if err == nil {
			err = bound.EnsureRemoved(cleanup, id, fault.Load() != nil || interrupted)
		}
		if dockerWatchDone != nil {
			select {
			case <-dockerWatchDone:
			case <-cleanup.Done():
				err = errors.Join(err, cleanup.Err())
			}
		}
		select {
		case <-interruptionDone:
		case <-cleanup.Done():
			err = errors.Join(err, cleanup.Err())
		}
		stopBridge()
		closeServers()
		done := make(chan struct{})
		go func() { workers.Wait(); writer.Close(); close(done) }()
		select {
		case <-done:
			if cleanupErr := filemanagement.CleanupSpools(cleanup, dataRoot, metadata, config.Docker); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		case <-cleanup.Done():
			err = errors.Join(err, errors.New("host spool writers did not stop; cleanup pending"))
		}
		if f := fault.Load(); f != nil {
			if latest, _, e := session.Load(dataRoot, metadata.ID); e == nil {
				metadata = latest
			}
			metadata.Phase = session.Error
			metadata.TerminationReason = f.Error()
			_ = session.Save(paths.Metadata, metadata)
			_ = fsutil.WriteJSON(filepath.Join(paths.Root, "transport-failure.json"), f, 0600)
			result = metadata
			runErr = errors.Join(runErr, f)
		}
		if interrupted {
			if latest, _, e := session.Load(dataRoot, metadata.ID); e == nil {
				metadata = latest
			}
			cause := context.Cause(ctx)
			metadata.Phase = session.Error
			metadata.TerminationReason = cause.Error()
			_ = session.Save(paths.Metadata, metadata)
			_ = fsutil.WriteJSON(filepath.Join(paths.Root, "host-interruption.json"), map[string]any{"reason": cause.Error(), "session_id": metadata.ID, "observed_at": time.Now().UTC()}, 0600)
			result = metadata
			runErr = errors.Join(runErr, cause)
		}
		if err != nil {
			_ = fsutil.WriteJSON(filepath.Join(paths.Root, "cleanup-pending.json"), map[string]string{"reason": "container or writer cleanup unconfirmed"}, 0600)
			runErr = errors.Join(runErr, fmt.Errorf("shutdown cleanup pending: %w", err))
		}
		if latest, _, err := session.Load(dataRoot, sessionID); err == nil {
			result = latest
			if nativeErr := terminalError(latest); nativeErr != nil {
				notifyFault(nativeErr)
				runErr = errors.Join(nativeErr, runErr)
			}
		}
		if runErr != nil {
			notifyFault(runErr)
		}
	}()
	if config.BridgeTransport == "spool" {
		for _, root := range filemanagement.SpoolRoots(paths) {
			if err := os.MkdirAll(root, 0700); err != nil {
				return metadata, err
			}
		}
		if err := filemanagement.CheckSpools(ctx, filemanagement.SpoolRoots(paths), metadata.SpoolMaxBytes); err != nil {
			var f *filemanagement.SpoolFault
			if errors.As(err, &f) {
				fault.Store(f)
			}
			notifyFault(err)
			return metadata, err
		}
		limit := metadata.SpoolMaxBytes
		quotaDone = make(chan struct{})
		go func() {
			defer close(quotaDone)
			filemanagement.WatchSpools(ctx, filemanagement.SpoolRoots(paths), limit, func(err error) {
				var f *filemanagement.SpoolFault
				if !errors.As(err, &f) {
					f = &filemanagement.SpoolFault{Reason: filemanagement.SpoolInspectionFailure, Limit: limit}
				}
				fault.Store(f)
				// Docker termination is independent of API ledger persistence and request dispatch.
				go func() {
					killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					killTarget(killCtx)
				}()
				cancelRun()
				notifyFault(f)
			})
		}()
	}
	var applicationListener, controlListener net.Listener
	if !config.InternalOnly {
		var err error
		applicationListener, err = listenLoopback(config.ApplicationPort)
		if err != nil {
			return metadata, err
		}
		defer applicationListener.Close()
		controlListener, err = listenLoopback(config.ControlPort)
		if err != nil {
			return metadata, err
		}
		defer controlListener.Close()
		metadata.ApplicationURL = "http://" + applicationListener.Addr().String()
		metadata.ControlURL = "http://" + controlListener.Addr().String()
	}
	containerName := "interceptor-" + metadata.ID
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
	embeddingProvider, err := embeddingsProvider(config)
	if err != nil {
		return metadata, err
	}
	connector, err := bridge.NewHostConnector(bridge.HostConnectorConfig{
		SessionID: metadata.ID, Provider: provider,
		EmbeddingsProvider: embeddingProvider, AllowedEmbeddingModels: config.AllowedEmbeddingModels,
		AllowedModel: config.AllowedModels, MaxBodyBytes: blueprint.Limits.MaxBlobBytes,
	})
	if err != nil {
		return metadata, err
	}
	bridgeContext, bridgeCancel := context.WithCancel(ctx)
	stopBridge = bridgeCancel
	bridgeErrors := make(chan error, 1)
	if config.BridgeTransport == "unix" {
		connectorServer, connectorListener, listenErr := bridge.ListenUnix(filepath.Join(paths.Sockets, "model.sock"), connector)
		if listenErr != nil {
			return metadata, listenErr
		}
		defer connectorServer.Close()
		defer connectorListener.Close()
		workers.Add(1)
		go func() { defer workers.Done(); bridgeErrors <- connectorServer.Serve(connectorListener) }()
	} else {
		workers.Add(1)
		go func() {
			defer workers.Done()
			bridgeErrors <- bridge.ServeSpool(bridgeContext, filepath.Join(paths.Sockets, "model-spool"), connector)
		}()
	}
	mounts := []ociruntime.Mount{
		{Source: absolute(config.EnvironmentDirectory), Target: "/opt/interceptor/environment", ReadOnly: true},
		{Source: paths.Root, Target: "/run/interceptor-session"},
		{Source: paths.Blobs, Target: "/run/interceptor-blobs"},
	}
	if blueprint.Network.TLS.Enabled {
		mounts = append(mounts, ociruntime.Mount{Source: appimage.TLSMaterialDirectory(dataRoot, blueprint), Target: "/run/interceptor-tls", ReadOnly: true})
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
	if !blueprint.Network.DNS.Enabled {
		for _, service := range blueprint.Services {
			hostnames = append(hostnames, service.Hostnames...)
		}
		for _, service := range blueprint.AttackerServices {
			hostnames = append(hostnames, service.Hostnames...)
		}
		for _, issuer := range blueprint.Identity.Issuers {
			hostnames = append(hostnames, issuer.Hostnames...)
		}
	}
	dns, sysctls := []string{}, map[string]string{}
	if blueprint.Network.DNS.Enabled {
		dns = []string{"127.0.0.1"}
	}
	if blueprint.Network.DNS.Enabled || blueprint.Network.HTTP.Enabled || blueprint.Network.TLS.Enabled {
		sysctls["net.ipv4.ip_unprivileged_port_start"] = "53"
	}
	containerID, err := config.Docker.Create(ctx, ociruntime.RunConfig{
		Name: containerName, Image: applicationLock.WrapperImage, User: "0:0",
		Environment: map[string]string{
			"INTERCEPTOR_HOST_UID": strconv.Itoa(os.Getuid()), "INTERCEPTOR_HOST_GID": strconv.Itoa(os.Getgid()),
			"INTERCEPTOR_APP_UID": "20000", "INTERCEPTOR_APP_GID": strconv.Itoa(os.Getgid()),
			"INTERCEPTOR_BRIDGE_TRANSPORT": config.BridgeTransport,
		},
		Mounts: mounts, Hostnames: hostnames, DNS: dns, Sysctls: sysctls, PIDs: metadata.Limits.MaxPIDs, MemoryBytes: metadata.Limits.MemoryBytes,
		CPUs: metadata.Limits.CPUs, ReadOnly: true, NetworkNone: true, NoNewPrivileges: true, DropAllCaps: true,
		AddCaps: []string{"SETUID", "SETGID", "KILL"}, TmpfsBytes: 64 << 20,
	})
	if containerID != "" {
		launchedID.Store(&containerID)
	}
	if err != nil {
		metadata.Phase, metadata.TerminationReason = session.Error, err.Error()
		_ = session.Save(paths.Metadata, metadata)
		return metadata, err
	}
	metadata.ContainerID = containerID
	if err := session.Save(paths.Metadata, metadata); err != nil {
		return metadata, err
	}
	if err := config.Docker.Start(ctx, containerID); err != nil {
		return metadata, err
	}
	dockerWatchDone = make(chan struct{})
	go func() {
		defer close(dockerWatchDone)
		_ = config.Docker.WatchConnection(ctx)
		if ctx.Err() == nil {
			interruptRun(hostlifetime.Interrupted("DOCKER_CONNECTION_LOST"))
		}
	}()
	if err := waitReady(ctx, paths.Metadata, config.Docker, containerID, time.Duration(blueprint.Application.Interface.StartupTimeoutMS)*time.Millisecond); err != nil {
		logsCtx, logsCancel := context.WithTimeout(context.Background(), 5*time.Second)
		logs, _ := config.Docker.Logs(logsCtx, containerID)
		logsCancel()
		metadata.Phase, metadata.TerminationReason = session.Error, err.Error()
		_ = session.Save(paths.Metadata, metadata)
		return metadata, fmt.Errorf("session startup failed: %w\n%s", err, logs)
	}
	metadata, _, _ = session.Load(dataRoot, metadata.ID)
	if config.InternalOnly {
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, probeErr := callControl(probe, metadata, paths, http.MethodGet, "/v1/status", nil)
		cancel()
		if probeErr != nil {
			metadata.Phase = session.Error
			metadata.TerminationReason = "selected private bridge is unavailable: " + probeErr.Error()
			_ = session.Save(paths.Metadata, metadata)
			return metadata, errors.New(metadata.TerminationReason)
		}
	}
	if !config.InternalOnly {
		var applicationServer, controlServer *http.Server
		if config.BridgeTransport == "unix" {
			if _, probeErr := callUnix(ctx, filepath.Join(paths.Sockets, "control.sock"), http.MethodGet, "/v1/status", nil, 1<<20); probeErr != nil {
				metadata.Phase, metadata.TerminationReason = session.Error, "selected Unix bridge is unavailable: "+probeErr.Error()
				_ = session.Save(paths.Metadata, metadata)
				return metadata, errors.New(metadata.TerminationReason)
			}
			applicationServer = unixProxyServer(applicationListener, filepath.Join(paths.Sockets, "application.sock"), nil)
			controlServer = unixProxyServer(controlListener, filepath.Join(paths.Sockets, "control.sock"), cooperativeRoute)
		} else {
			applicationClient, clientErr := bridge.NewHTTPSpoolClient(filepath.Join(paths.Sockets, "http-spool"), blueprint.Limits.MaxBlobBytes, time.Duration(blueprint.Application.Interface.TurnTimeoutMS)*time.Millisecond)
			if clientErr != nil {
				return metadata, clientErr
			}
			controlClient, clientErr := bridge.NewHTTPSpoolClient(filepath.Join(paths.Sockets, "http-spool"), adaptive.ArtifactEnvelopeLimit(blueprint.Limits.MaxBlobBytes), time.Duration(blueprint.Application.Interface.TurnTimeoutMS)*time.Millisecond)
			if clientErr != nil {
				return metadata, clientErr
			}
			if _, probeErr := controlClient.Do(ctx, bridge.ControlChannel, http.MethodGet, "/v1/status", nil, nil); probeErr != nil {
				metadata.Phase, metadata.TerminationReason = session.Error, "default spool bridge is unavailable: "+probeErr.Error()
				_ = session.Save(paths.Metadata, metadata)
				return metadata, errors.New(metadata.TerminationReason)
			}
			applicationServer = spoolProxyServer(applicationListener, applicationClient, bridge.ApplicationChannel, blueprint.Limits.MaxBlobBytes, nil)
			controlServer = spoolProxyServer(controlListener, controlClient, bridge.ControlChannel, adaptive.ArtifactEnvelopeLimit(blueprint.Limits.MaxBlobBytes), cooperativeRoute)
		}
		defer applicationServer.Close()
		defer controlServer.Close()
		closeServers = func() { _ = applicationServer.Close(); _ = controlServer.Close() }
		workers.Add(2)
		go func() { defer workers.Done(); _ = applicationServer.Serve(applicationListener) }()
		go func() { defer workers.Done(); _ = controlServer.Serve(controlListener) }()
		if config.Output != nil {
			fmt.Fprintf(config.Output, "Session %s running\n  application: %s\n  control: %s\n", metadata.ID, metadata.ApplicationURL, metadata.ControlURL)
		}
	}
	if config.OnReady != nil {
		if err := config.OnReady(metadata); err != nil {
			return metadata, fmt.Errorf("post-start operation failed: %w", err)
		}
	}
	requestedStop := false
	var executionErr error
	failExecution := func(err error) {
		executionErr = err
		terminateFailed(err)
		if metadata.Phase != session.Error {
			metadata.Phase, metadata.TerminationReason = session.Error, err.Error()
			_ = session.Save(paths.Metadata, metadata)
		}
	}
	monitor := time.NewTicker(500 * time.Millisecond)
	waiting := true
	for waiting {
		select {
		case <-ctx.Done():
			requestedStop, waiting = true, false
		case bridgeErr := <-bridgeErrors:
			independentFailure := bridgeErr != nil && !errors.Is(bridgeErr, context.Canceled) && !errors.Is(bridgeErr, http.ErrServerClosed)
			if independentFailure || !plannedShutdown(ctx) {
				if bridgeErr == nil {
					bridgeErr = errors.New("connector stopped unexpectedly")
				}
				failExecution(fmt.Errorf("MODEL_CONNECTOR_FAILED: %w", bridgeErr))
			}
			waiting = false
		case <-monitor.C:
			// Native error metadata is sufficient to close execution; do not wait for
			// the container's shutdown/flush to finish before notifying Operator.
			latest, _, loadErr := session.Load(dataRoot, sessionID)
			if loadErr != nil {
				failExecution(fmt.Errorf("TARGET_STATE_UNAVAILABLE: %w", loadErr))
				waiting = false
				continue
			}
			metadata = latest
			if nativeErr := terminalError(latest); nativeErr != nil {
				failExecution(nativeErr)
				waiting = false
				continue
			}
			inspectCtx, inspectCancel := context.WithTimeout(ctx, 2*time.Second)
			running, inspectErr := config.Docker.Running(inspectCtx, containerID)
			inspectCancel()
			if inspectErr == nil && !running {
				// Re-read after inspection in case the supervisor finalized during it.
				latest, _, loadErr = session.Load(dataRoot, sessionID)
				if loadErr == nil {
					metadata = latest
				}
				if nativeErr := terminalError(metadata); nativeErr != nil {
					failExecution(nativeErr)
				} else if !plannedShutdown(ctx) {
					interruptRun(hostlifetime.Interrupted("CONTAINER_INTERRUPTED"))
				}
				waiting = false
			} else if inspectErr != nil {
				if ctx.Err() == nil {
					interruptRun(hostlifetime.Interrupted("DOCKER_UNAVAILABLE"))
				}
				waiting = false
			}
		}
	}
	monitor.Stop()
	stopContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if requestedStop && fault.Load() == nil && !hostlifetime.IsInterruption(context.Cause(ctx)) {
		_, _ = callControl(stopContext, metadata, paths, http.MethodPost, "/v1/stop", []byte(`{}`))
		_ = config.Docker.Stop(stopContext, containerID, 5*time.Second)
	}
	_ = config.Docker.Remove(stopContext, containerID)
	latest, _, loadErr := session.Load(dataRoot, metadata.ID)
	if loadErr == nil {
		metadata = latest
	}
	return metadata, executionErr
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
		return bridge.LoadBedrockConverseProvider(ctx, bridge.BedrockProviderConfig{Region: config.AWSRegion, Profile: config.AWSProfile, Authentication: config.ProviderAuthMode})
	case bridge.ProviderAnthropicMessages:
		if config.ProviderAuthMode == "workload-token" {
			httpConfig.CredentialHeader = "Authorization"
			httpConfig.CredentialPrefix = "Bearer "
		}
		if config.ProviderAPIVersion != "" {
			httpConfig.Headers = map[string]string{"Anthropic-Version": config.ProviderAPIVersion}
		}
		version := config.ProviderAPIVersion
		if version == "" {
			version = "2023-06-01"
		}
		httpConfig.Metadata = map[string]string{"api_version": version}
		return bridge.NewAnthropicMessagesProvider(httpConfig)
	case bridge.ProviderGeminiAPI:
		return bridge.NewGeminiAPIProvider(httpConfig)
	case bridge.ProviderVertexGemini:
		if config.ProviderAPIKey == "" {
			httpConfig.Client = &http.Client{Transport: &googleBearerTransport{authentication: config.ProviderAuthMode, base: http.DefaultTransport}}
		} else {
			httpConfig.CredentialHeader = "X-Goog-Api-Key"
		}

		return bridge.NewVertexGeminiProvider(httpConfig)
	case bridge.ProviderAzureOpenAI:
		if config.ProviderAuthMode == "workload-identity" || config.ProviderAuthMode == "azure-cli" || config.ProviderAuthMode == "azure-client-secret" {
			credential, err := credentials.AzureIdentity(config.ProviderAuthMode)
			if err != nil {
				return nil, fmt.Errorf("initialize Azure workload identity: %w", err)
			}
			httpConfig.APIKey = ""
			httpConfig.Client = &http.Client{Transport: &azureBearerTransport{credential: credential, base: http.DefaultTransport}}
		}
		return bridge.NewAzureOpenAIProvider(httpConfig)
	default:
		return nil, fmt.Errorf("unsupported model provider %q", config.ProviderType)
	}
}

type azureBearerTransport struct {
	credential azcore.TokenCredential
	base       http.RoundTripper
}

func (t *azureBearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	token, err := t.credential.GetToken(request.Context(), policy.TokenRequestOptions{Scopes: []string{"https://cognitiveservices.azure.com/.default"}})
	if err != nil {
		return nil, err
	}
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+token.Token)
	return t.base.RoundTrip(clone)
}

// Resolve under each request context so a completed startup timeout cannot poison token refresh.
type googleBearerTransport struct {
	authentication string
	base           http.RoundTripper
}

func (t *googleBearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	identity, err := credentials.GoogleIdentity(request.Context(), t.authentication)
	if err != nil {
		return nil, err
	}
	quota, err := credentials.GoogleQuotaProject(identity)
	if err != nil {
		return nil, err
	}
	token, err := identity.TokenSource.Token()
	if err != nil {
		return nil, errors.New("Google token unavailable")
	}
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+token.AccessToken)
	if quota != "" {
		clone.Header.Set("X-Goog-User-Project", quota)
	}
	return t.base.RoundTrip(clone)
}

func bindAdaptiveMetadata(metadata *session.Metadata, blueprint *environment.Blueprint, requested string) error {
	if requested == "" {
		requested = "black-box"
	}
	supported := false
	for _, profile := range blueprint.Adaptive.FeedbackProfiles {
		if profile == requested {
			supported = true
			break
		}
	}
	if !supported {
		return fmt.Errorf("feedback profile %q is not supported by the sealed environment", requested)
	}
	manifest, err := adaptive.BuildCapabilityManifest(blueprint, metadata.EnvironmentDigest, metadata.AppDigest)
	if err != nil {
		return err
	}
	metadata.CapabilityManifestDigest = manifest.Digest
	metadata.FeedbackProfile = requested
	metadata.Revision = 1
	return nil
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
	writer, err := filemanagement.Lock(dataRoot, "writers-"+sessionID, false)
	if err != nil {
		return nil, err
	}
	defer writer.Close()
	if metadata.ContainerID == "" {
		return nil, errors.New("session has no running container")
	}
	if metadata.Phase != session.Running && metadata.Phase != session.Quiescing {
		return nil, fmt.Errorf("session is not running: %s", metadata.Phase)
	}
	return callControl(ctx, metadata, paths, method, path, body)
}

func callControl(ctx context.Context, metadata session.Metadata, paths session.Paths, method, path string, body []byte) ([]byte, error) {
	limit := adaptive.ArtifactEnvelopeLimit(metadata.Limits.MaxBlobBytes)
	if path == "/v1/operations" {
		limit = adaptive.OperationFrameLimit(metadata.Limits.MaxBlobBytes)
	}
	if metadata.BridgeTransport == "unix" {
		return callUnix(ctx, filepath.Join(paths.Sockets, "control.sock"), method, path, body, limit)
	}
	client, err := bridge.NewHTTPSpoolClient(filepath.Join(paths.Sockets, "http-spool"), limit, defaultControlTimeout(metadata))
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

func spoolProxyServer(listener net.Listener, client *bridge.HTTPSpoolClient, channel string, maxBodyBytes int64, allowed func(*http.Request) bool) *http.Server {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if allowed != nil && !allowed(request) {
			http.NotFound(writer, request)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, maxBodyBytes+1))
		if err != nil || int64(len(body)) > maxBodyBytes {
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

func callUnix(ctx context.Context, socket, method, path string, body []byte, maxResponseBytes int64) ([]byte, error) {
	if maxResponseBytes <= 0 {
		return nil, errors.New("Unix control response limit must be positive")
	}
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
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxResponseBytes {
		return nil, errors.New("control API response exceeds configured limit")
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
	if request.Method == http.MethodGet && (path == "/v1/status" || path == "/v1/capabilities" || path == "/v1/artifacts" || path == "/v1/attempts" || path == "/v1/injections" || path == "/v1/snapshots" || path == "/v1/events" || strings.HasPrefix(path, "/v1/artifacts/") || strings.HasPrefix(path, "/v1/observations/") || strings.HasPrefix(path, "/v1/turns/") || strings.HasPrefix(path, "/v1/snapshots/") || strings.HasPrefix(path, "/v1/attacker/")) {
		return true
	}
	if request.Method == http.MethodPost && (path == "/v1/artifacts" || path == "/v1/attempts" || path == "/v1/turns" || path == "/v1/injections" || path == "/v1/snapshots" || path == "/v1/identity/caller-tokens") {
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

// Keep the original hash-verified prefix as provenance alongside the replacement's
// own replay journal. Its checkpoints never require opening a parent directory.
func preserveRestoreSource(paths session.Paths, checkpoint snapshot.Checkpoint, tx []statejournal.Transaction) error {
	directory := filepath.Join(paths.Root, "restore-source")
	if err := fsutil.WriteJSON(filepath.Join(directory, "checkpoint.json"), checkpoint, 0600); err != nil {
		return err
	}
	var prefix bytes.Buffer
	for _, transaction := range tx {
		if err := json.NewEncoder(&prefix).Encode(transaction); err != nil {
			return err
		}
	}
	return fsutil.WriteFile(filepath.Join(directory, "state.jsonl"), prefix.Bytes(), 0600)
}

func embeddingsProvider(config Config) (bridge.ModelProvider, error) {
	if config.EmbeddingsProviderType == "" {
		return nil, nil
	}
	if config.EmbeddingsProviderType != bridge.ProviderOpenAICompatible {
		return nil, errors.New("unsupported embeddings provider")
	}
	return bridge.NewOpenAIEmbeddingsProvider(bridge.HTTPProviderConfig{BaseURL: config.EmbeddingsURL, APIKey: config.EmbeddingsAPIKey})
}
