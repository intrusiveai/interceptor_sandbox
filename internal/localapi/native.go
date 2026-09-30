package localapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evidence"
	"github.com/intrusive-ai/interceptor-sandbox/internal/filemanagement"
	"github.com/intrusive-ai/interceptor-sandbox/internal/gatewayhost"
	"github.com/intrusive-ai/interceptor-sandbox/internal/hostlifetime"
	"github.com/intrusive-ai/interceptor-sandbox/internal/hostrun"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

type EvidenceExporter func(context.Context, string, session.Metadata, session.Paths, string, int64) error
type nativeRun struct {
	sessionID   string // guarded by NativeRuntime.mu until ready
	plannedStop atomic.Bool
	cancel      context.CancelCauseFunc
	done        chan struct{}
	metadata    session.Metadata
	err         error
}
type NativeRuntime struct {
	ctx           context.Context
	config        hostrun.Config
	root          string
	export        EvidenceExporter
	mu            sync.Mutex
	active        *nativeRun
	exportGate    chan struct{}
	faultMu       sync.Mutex
	service       *Service
	terminalFault *ExecutionFailure
	power         *hostlifetime.Manager
}

// Run owns the localhost listener and target lifetime. Closing a target leaves
// the listener available for status and evidence until the parent is cancelled.
func Run(ctx context.Context, config hostrun.Config, export EvidenceExporter) (runErr error) {
	var err error
	config.EvidenceMaxBytes, err = evidence.ResolveLimit(config.EvidenceMaxBytes)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:8080")
	if err != nil {
		return fmt.Errorf("reserve local API (one instance is supported): %w", err)
	}
	defer listener.Close()
	root, err := session.DataRoot()
	if err != nil {
		return err
	}
	maintenance, err := filemanagement.Lock(root, "maintenance", false)
	if err != nil {
		return err
	}
	defer maintenance.Close()
	config.Docker, err = config.Docker.Pin(ctx)
	if err != nil {
		return err
	}
	// Do not silently adopt or run alongside a target left by an earlier service.
	active, err := config.Docker.InterceptorContainers(ctx)
	if err != nil {
		return err
	}
	if len(active) > 0 {
		return fmt.Errorf("existing live Interceptor containers require administrator cleanup before a new run: %v", active)
	}
	if err := filemanagement.CleanupAbandoned(ctx, root, config.Docker); err != nil {
		return err
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	config.InternalOnly = true
	n := &NativeRuntime{ctx: serviceCtx, config: config, root: root, export: export, exportGate: make(chan struct{}, 1)}
	n.power = hostlifetime.New(n.hostInterrupted)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := n.stopActive(cleanup); err != nil && runErr == nil {
			runErr = fmt.Errorf("final target cleanup unconfirmed: %w", err)
		}
	}()
	metadata, err := n.start(ctx, "", "")
	if err != nil {
		return err
	}
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		return err
	}
	instance := "inst-" + hex.EncodeToString(random)
	service, err := New(serviceCtx, n, metadata, instance, filepath.Join(root, "instances", instance+".json"), config.EvidenceMaxBytes)
	if err != nil {
		return err
	}
	n.faultMu.Lock()
	n.service = service
	if n.terminalFault != nil {
		_ = service.failExecution(*n.terminalFault)
	}
	n.faultMu.Unlock()
	server := &http.Server{Handler: service, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-serviceCtx.Done():
			_ = server.Close()
		case <-stopped:
		}
	}()
	defer close(stopped)
	if config.Output != nil {
		fmt.Fprintf(config.Output, "Interceptor local API: http://127.0.0.1:8080\nSession: %s\nCtrl+C stops the service and any active target.\n", metadata.ID)
	}
	err = server.Serve(listener)
	cancel()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func (n *NativeRuntime) start(ctx context.Context, source, checkpoint string) (session.Metadata, error) {
	n.mu.Lock()
	if n.active != nil {
		select {
		case <-n.active.done:
		default:
			n.mu.Unlock()
			return session.Metadata{}, errors.New("target already active")
		}
	}
	release, err := n.holdActivity(ctx)
	if err != nil {
		n.mu.Unlock()
		return session.Metadata{}, err
	}
	life, cancel := context.WithCancelCause(n.ctx)
	run := &nativeRun{cancel: cancel, done: make(chan struct{})}
	n.active = run
	ready := make(chan session.Metadata, 1)
	config := n.config
	config.OnReady = func(m session.Metadata) error {
		n.mu.Lock()
		run.sessionID = m.ID
		n.mu.Unlock()
		ready <- m
		return nil
	}
	config.OnFault = func(id string, err error) { n.targetFailed(run, id, err) }
	n.mu.Unlock()
	go func() {
		defer close(run.done)
		defer release()
		if source == "" {
			run.metadata, run.err = hostrun.Run(life, config)
		} else {
			run.metadata, run.err = hostrun.Restore(life, config, source, checkpoint)
		}
		// Covers every return, including exits during startup and paths which did
		// not report a fault before cleanup. Publish before signaling run.done.
		n.targetEnded(run)
	}()
	select {
	case m := <-ready:
		return m, nil
	case <-run.done:
		cancel(nil)
		if run.err != nil {
			return run.metadata, run.err
		}
		return run.metadata, errors.New("target exited before readiness")
	case <-ctx.Done():
		run.plannedStop.Store(true)
		cancel(hostrun.PlannedStop)
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-run.done:
			return run.metadata, ctx.Err()
		case <-timer.C:
			return session.Metadata{}, errors.New("startup cancellation cleanup unconfirmed")
		}

	}
}
func (n *NativeRuntime) Metadata(id string) (session.Metadata, error) {
	m, _, err := session.Load(n.root, id)
	return m, err
}
func (n *NativeRuntime) Execute(ctx context.Context, principal string, q adaptive.OperationRequest, body json.RawMessage) (adaptive.GatewayResponse, error) {
	if err := n.CheckHost(); err != nil && !readOnly(q.Operation) {
		return adaptive.OperationError(409, "host_interrupted"), nil
	}
	request, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/operations", nil)
	return gatewayhost.NewExecutor().Execute(request, principal, q, body)
}
func (n *NativeRuntime) ValidateRestore(ctx context.Context, source, id string) error {
	if err := n.CheckHost(); err != nil {
		return err
	}
	m, paths, err := session.Load(n.root, source)
	if err != nil {
		return err
	}
	checkpoint, err := snapshot.LoadCheckpoint(paths.Checkpoints, id)
	if err != nil {
		return err
	}
	if checkpoint.CampaignID != "" {
		state, err := adaptive.ReadExecutionState(paths.Root)
		if err != nil || state.Owner == nil || state.Owner.CampaignID != checkpoint.CampaignID {
			return errors.New("checkpoint campaign mismatch")
		}
	}
	if checkpoint.SourceSessionID != m.ID || checkpoint.EnvironmentDigest != m.EnvironmentDigest || checkpoint.AppDigest != m.AppDigest || checkpoint.InterceptorVersion != buildinfo.Version || checkpoint.JournalSchemaVersion != 1 {
		return errors.New("incompatible checkpoint")
	}
	lock, _, err := environment.Verify(m.EnvironmentPath)
	if err != nil {
		return err
	}
	if lock.EnvironmentDigest != m.EnvironmentDigest {
		return errors.New("sealed environment changed")
	}
	tx, err := statejournal.VerifyFile(paths.Journal)
	if err != nil {
		return err
	}
	if checkpoint.JournalSeq == 0 || checkpoint.JournalSeq > uint64(len(tx)) || tx[checkpoint.JournalSeq-1].Hash != checkpoint.JournalHash {
		return errors.New("checkpoint journal unavailable")
	}
	blobs, err := blobstore.Open(paths.Blobs, m.Limits.MaxBlobBytes)
	if err != nil {
		return err
	}
	if err = snapshot.VerifyBlobs(blobs, tx, checkpoint.JournalSeq); err != nil {
		return err
	}
	return ctx.Err()
}
func (n *NativeRuntime) Restore(ctx context.Context, source, checkpoint string) (session.Metadata, error) {
	return n.start(ctx, source, checkpoint)
}
func (n *NativeRuntime) stopActive(ctx context.Context) error {
	n.mu.Lock()
	active := n.active
	n.mu.Unlock()
	if active == nil {
		return nil
	}
	active.plannedStop.Store(true)
	active.cancel(hostrun.PlannedStop)
	select {
	case <-active.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if active.metadata.ID == "" {
		return active.err
	}
	return n.confirmStopped(ctx, active.metadata.ID, active)
}
func (n *NativeRuntime) Stop(ctx context.Context, id string) error {
	n.mu.Lock()
	active := n.active
	n.mu.Unlock()
	// Lifecycle calls are serialized; this check also covers naturally exited runs.
	if active == nil {
		return errors.New("no target managed by this instance")
	}
	n.mu.Lock()
	activeID := active.sessionID
	select {
	case <-active.done:
		if activeID == "" {
			activeID = active.metadata.ID
		}
	default:
	}
	n.mu.Unlock()
	if activeID != id {
		return errors.New("active target mismatch")
	}
	active.plannedStop.Store(true)
	active.cancel(hostrun.PlannedStop)
	select {
	case <-active.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if active.metadata.ID != id {
		return errors.New("active target mismatch")
	}
	return n.confirmStopped(ctx, id, active)
}
func (n *NativeRuntime) confirmStopped(ctx context.Context, id string, run *nativeRun) (resultErr error) {
	defer func() {
		if resultErr != nil {
			n.targetFailed(run, id, resultErr)
		}
	}()
	m, paths, err := session.Load(n.root, id)
	if err != nil {
		return err
	}
	docker, containerID, err := n.config.Docker.ForSession(ctx, n.root, id)
	if err != nil {
		return err
	}
	if err := docker.EnsureRemoved(ctx, containerID, false); err != nil {
		return err
	}
	// Reload after the supervisor's final journal flush; never overwrite final
	// native counters/evaluation with a pre-stop copy of the metadata.
	m, paths, err = session.Load(n.root, id)
	if err != nil {
		return err
	}
	if m.Phase != session.Stopped && m.Phase != session.Error {
		m.Phase = session.Error
		m.TerminationReason = "host confirmed target removal; native finalization incomplete"
		failure := errors.New(m.TerminationReason)
		n.targetFailed(run, id, failure)
		return errors.Join(failure, session.Save(paths.Metadata, m))
	}
	if m.Phase == session.Error {
		reason := m.TerminationReason
		if reason == "" {
			reason = "TARGET_FAILED"
		}
		n.targetFailed(run, id, errors.New(reason))
	}
	if err := filemanagement.CleanupSpools(ctx, n.root, m, n.config.Docker); err != nil {
		n.targetFailed(run, id, err)
		return err
	}
	return nil
}
func (n *NativeRuntime) Export(ctx context.Context, id string, maximumBytes int64) (*os.File, error) {
	select {
	case n.exportGate <- struct{}{}:
		defer func() { <-n.exportGate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	m, paths, err := session.Load(n.root, id)
	if err != nil {
		return nil, err
	}
	if m.Phase != session.Stopped && m.Phase != session.Error {
		return nil, errors.New("target must be stopped")
	}
	docker, containerID, err := n.config.Docker.ForSession(ctx, n.root, id)
	if err != nil {
		return nil, err
	}
	present, err := docker.ContainerPresent(ctx, containerID)
	if err != nil {
		return nil, err
	}
	if present {
		return nil, errors.New("target removal pending")
	}
	directory := paths.Exports
	if err = os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(directory, "evidence-*.tar")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	_ = file.Close()
	if err = n.export(ctx, n.root, m, paths, path, maximumBytes); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	file, err = os.Open(path)
	if err != nil {
		_ = os.Remove(path)
	}
	return file, err
}

// targetFailed scopes delayed callbacks to the actual running target, including
// a replacement whose binding has not yet been published by the API.
func (n *NativeRuntime) targetFailed(run *nativeRun, id string, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.active != run {
		return
	}
	if id == "" {
		id = run.sessionID
	}
	n.transportFailed(id, err)
}

func (n *NativeRuntime) targetEnded(run *nativeRun) {
	err := run.err
	if run.metadata.Phase == session.Error {
		reason := run.metadata.TerminationReason
		if reason == "" {
			reason = "TARGET_FAILED"
		}
		err = errors.New(reason)
	}
	if err == nil && !run.plannedStop.Load() && (n.ctx == nil || n.ctx.Err() == nil) {
		err = errors.New("CONTAINER_INTERRUPTED")
	}
	if err != nil {
		n.targetFailed(run, run.metadata.ID, err)
	}
}

func (n *NativeRuntime) transportFailed(id string, err error) {
	n.faultMu.Lock()
	defer n.faultMu.Unlock()
	if n.terminalFault == nil {
		n.terminalFault = &ExecutionFailure{SessionID: id, Reason: err.Error(), ObservedAt: time.Now().UTC()}
	}
	if n.service != nil {
		_ = n.service.failExecution(*n.terminalFault)
	}
}

func (n *NativeRuntime) holdActivity(ctx context.Context) (func(), error) {
	if n.power == nil {
		return func() {}, nil
	}
	return n.power.Acquire(ctx)
}
func (n *NativeRuntime) HoldRestore(ctx context.Context) (func(), error) { return n.holdActivity(ctx) }
func (n *NativeRuntime) CheckHost() error {
	if n.power == nil {
		return nil
	}
	return n.power.Check()
}
func (n *NativeRuntime) hostInterrupted(err error) {
	// Cancellation carries the reason into hostrun's independent Docker kill and
	// durable failure record; API closure does not wait for a native response.
	n.mu.Lock()
	active := n.active
	n.mu.Unlock()
	if active != nil {
		active.cancel(err)
	}
	n.transportFailed("", err)
}
