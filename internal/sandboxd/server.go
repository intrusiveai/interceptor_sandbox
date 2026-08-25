package sandboxd

import (
	"bufio"
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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evaluation"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/services"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

const (
	containerEnvironment = "/opt/interceptor/environment"
	containerSession     = "/run/interceptor-session"
	containerBlobs       = "/run/interceptor-blobs"
)

type Server struct {
	blueprint   *environment.Blueprint
	metadata    session.Metadata
	paths       session.Paths
	recorder    *recording.Writer
	journal     *statejournal.Writer
	blobs       *blobstore.Store
	injections  *injection.Manager
	services    map[string]*services.StatefulHandler
	sinks       map[string]*services.SinkHandler
	servers     []*http.Server
	listeners   []net.Listener
	app         *exec.Cmd
	helper      *exec.Cmd
	helperIn    io.WriteCloser
	helperOut   *bufio.Reader
	helperMu    sync.Mutex
	appTarget   *url.URL
	appUID      uint32
	appGID      uint32
	hostUID     int
	hostGID     int
	active      atomic.Int64
	activityMu  sync.RWMutex
	metadataMu  sync.RWMutex
	oracleMu    sync.Mutex
	oracleState map[string]evaluation.OracleResult
	virtualMu   sync.RWMutex
	virtualTime time.Time
	controlMu   sync.Mutex
	turnMu      sync.RWMutex
	turns       map[string]Turn
	nextTurn    atomic.Uint64
	done        chan error
	closeOnce   sync.Once
}

type TurnRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

type Turn struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Code     int               `json:"code,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     []byte            `json:"body,omitempty"`
	Error    string            `json:"error,omitempty"`
	Started  time.Time         `json:"started_at"`
	Finished time.Time         `json:"finished_at,omitempty"`
}

func Run(ctx context.Context) error {
	if err := adoptSupervisorIdentity(envInt("INTERCEPTOR_HOST_UID", 0), envInt("INTERCEPTOR_HOST_GID", 0)); err != nil {
		return fmt.Errorf("establish supervisor identity: %w", err)
	}
	server, err := New()
	if err != nil {
		return err
	}
	if err := server.Start(ctx); err != nil {
		server.Close(context.Background())
		return err
	}
	select {
	case <-ctx.Done():
		return server.Close(context.Background())
	case err := <-server.done:
		if err != nil {
			server.metadataMu.Lock()
			if server.metadata.Phase != session.Error {
				server.metadata.Phase = session.Error
				server.metadata.TerminationReason = "APPLICATION_EXIT: " + err.Error()
				_ = session.Save(server.paths.Metadata, server.metadata)
			}
			server.metadataMu.Unlock()
		}
		server.Close(context.Background())
		if err != nil {
			return err
		}
		return nil
	}
}

func New() (*Server, error) {
	lock, report, err := environment.Verify(containerEnvironment)
	if err != nil {
		return nil, fmt.Errorf("verify mounted Sealed Environment: %w", err)
	}
	metadataData, err := os.ReadFile(filepath.Join(containerSession, "session.json"))
	if err != nil {
		return nil, err
	}
	var metadata session.Metadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		return nil, err
	}
	if metadata.EnvironmentDigest != lock.EnvironmentDigest {
		return nil, errors.New("session and Sealed Environment digests differ")
	}
	if metadata.BridgeTransport != "spool" && metadata.BridgeTransport != "unix" {
		return nil, fmt.Errorf("unsupported session bridge transport %q", metadata.BridgeTransport)
	}
	if configured := os.Getenv("INTERCEPTOR_BRIDGE_TRANSPORT"); configured != metadata.BridgeTransport {
		return nil, errors.New("session and container bridge transports differ")
	}
	paths := session.Paths{
		Root: containerSession, Metadata: filepath.Join(containerSession, "session.json"),
		Events: filepath.Join(containerSession, "events.jsonl"), Journal: filepath.Join(containerSession, "state.jsonl"),
		FileManifest: filepath.Join(containerSession, "file-manifest.json"), Checkpoints: filepath.Join(containerSession, "checkpoints"),
		Generations: filepath.Join(containerSession, "generations"), Logs: filepath.Join(containerSession, "logs"), Sockets: filepath.Join(containerSession, "sockets"),
	}
	recorder, err := recording.Open(paths.Events, metadata.ID)
	if err != nil {
		return nil, err
	}
	journal, err := statejournal.Open(paths.Journal, metadata.ID)
	if err != nil {
		recorder.Close()
		return nil, err
	}
	blobs, err := blobstore.Open(containerBlobs, report.Blueprint.Limits.MaxBlobBytes)
	if err != nil {
		journal.Close()
		recorder.Close()
		return nil, err
	}
	appUID := uint32(envInt("INTERCEPTOR_APP_UID", 20000))
	appGID := uint32(envInt("INTERCEPTOR_APP_GID", 20000))
	return &Server{
		blueprint: report.Blueprint, metadata: metadata, paths: paths, recorder: recorder, journal: journal, blobs: blobs,
		injections: injection.NewManager(journal, recorder), services: map[string]*services.StatefulHandler{}, sinks: map[string]*services.SinkHandler{},
		appUID: appUID, appGID: appGID, hostUID: envInt("INTERCEPTOR_HOST_UID", 0), hostGID: envInt("INTERCEPTOR_HOST_GID", 0),
		turns: map[string]Turn{}, oracleState: map[string]evaluation.OracleResult{}, virtualTime: report.Blueprint.Time.Base.UTC(), done: make(chan error, 1),
	}, nil
}

func (s *Server) Start(ctx context.Context) error {
	if err := s.prepareMutableRoots(); err != nil {
		return err
	}
	if err := s.startSyntheticServices(); err != nil {
		return err
	}
	if err := s.startModelProxy(); err != nil {
		return err
	}
	if err := s.startApplication(); err != nil {
		return err
	}
	if err := s.waitForHealth(ctx); err != nil {
		return err
	}
	if err := s.startHostBridge(ctx); err != nil {
		return err
	}
	s.metadataMu.Lock()
	s.metadata.Phase = session.Running
	if err := session.Save(s.paths.Metadata, s.metadata); err != nil {
		s.metadataMu.Unlock()
		return err
	}
	s.metadataMu.Unlock()
	_, _ = s.recorder.Append(recording.Draft{Channel: recording.Control, Layer: "sandboxd", Kind: "application_ready", Provenance: recording.SandboxControl, Semantic: map[string]any{"pid": s.app.Process.Pid}})
	go s.enforceWallLimit()
	return s.recorder.Flush()
}

func (s *Server) startSyntheticServices() error {
	for _, definition := range s.blueprint.Services {
		var handler http.Handler
		switch definition.Kind {
		case "fixture_http":
			fixtures, err := services.LoadFixtureSet(filepath.Join(containerEnvironment, definition.Fixtures))
			if err != nil {
				return err
			}
			handler = services.NewFixtureHandler(definition.ID, fixtures, containerEnvironment, s.recorder)
		case "stateful_http":
			stateful, err := services.LoadStateful(filepath.Join(containerEnvironment, definition.Seed), filepath.Join(containerEnvironment, definition.Routes), definition.ID, s.journal, s.recorder)
			if err != nil {
				return err
			}
			s.services[definition.ID], handler = stateful, stateful
		}
		if err := s.serveTCP(definition.Listen, s.trackActivity(handler)); err != nil {
			return fmt.Errorf("start service %s: %w", definition.ID, err)
		}
	}
	if err := s.replayManagedState(); err != nil {
		return err
	}
	for _, definition := range s.blueprint.AttackerServices {
		methods, paths := optionStrings(definition.Options["methods"]), optionStrings(definition.Options["paths"])
		limit := definition.MaxCaptureBytes
		if limit == 0 {
			limit = 1 << 20
		}
		sink, err := services.NewSinkHandler(definition.ID, methods, paths, services.SinkResponse{Status: definition.Response.Status, JSON: definition.Response.JSON, Body: definition.Response.Body}, limit, s.blobs, s.recorder)
		if err != nil {
			return err
		}
		s.sinks[definition.ID] = sink
		if err := s.serveTCP(definition.Listen, s.trackActivity(sink)); err != nil {
			return fmt.Errorf("start attacker service %s: %w", definition.ID, err)
		}
	}
	return nil
}

func (s *Server) startModelProxy() error {
	baseValue := s.blueprint.Application.Launch.Environment["MODEL_BASE_URL"]
	baseURL, err := url.Parse(baseValue)
	if err != nil || baseURL.Scheme != "http" || baseURL.Host == "" || !isLoopback(baseURL.Hostname()) {
		return errors.New("application MODEL_BASE_URL must identify the loopback inference proxy")
	}
	var connector bridge.Connector
	if s.metadata.BridgeTransport == "unix" {
		connector = bridge.NewUnixConnectorClient(filepath.Join(s.paths.Sockets, "model.sock"))
	} else {
		spoolConnector, err := bridge.NewSpoolConnector(filepath.Join(s.paths.Sockets, "model-spool"))
		if err != nil {
			return err
		}
		connector = spoolConnector
	}
	proxy, err := bridge.NewModelProxy(s.metadata.ID, connector, s.injections, s.blobs, s.recorder, s.blueprint.Limits.MaxBlobBytes, time.Duration(s.blueprint.Application.IO.TurnTimeoutMS)*time.Millisecond)
	if err != nil {
		return err
	}
	proxy.SetTokenObserver(s.consumeTokens)
	return s.serveTCP(baseURL.Host, s.trackActivity(proxy))
}

func (s *Server) startApplication() error {
	stdout, err := os.OpenFile(filepath.Join(s.paths.Logs, "application.stdout.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	stderr, err := os.OpenFile(filepath.Join(s.paths.Logs, "application.stderr.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		stdout.Close()
		return err
	}
	command := s.blueprint.Application.Launch.Command
	s.app = exec.Command(command[0], command[1:]...)
	s.app.Dir, s.app.Stdout, s.app.Stderr = s.blueprint.Application.Launch.Workdir, stdout, stderr
	s.app.Env = []string{}
	for name, value := range s.blueprint.Application.Launch.Environment {
		s.app.Env = append(s.app.Env, name+"="+value)
	}
	for _, credential := range s.blueprint.Credentials {
		s.app.Env = append(s.app.Env, credential.EnvironmentVariable+"="+credential.Value)
	}
	s.virtualMu.RLock()
	s.app.Env = append(s.app.Env, "INTERCEPTOR_VIRTUAL_TIME="+s.virtualTime.Format(time.RFC3339Nano))
	s.virtualMu.RUnlock()
	s.app.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: s.appUID, Gid: s.appGID}, Setpgid: true}
	if s.blueprint.Application.Persistence.Quiesce.Kind == "command" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		s.helper = exec.Command(executable, "app-helper")
		s.helper.Dir, s.helper.Env = s.app.Dir, s.app.Env
		s.helper.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: s.appUID, Gid: s.appGID}, Setpgid: true}
		s.helperIn, err = s.helper.StdinPipe()
		if err != nil {
			return err
		}
		helperOutput, err := s.helper.StdoutPipe()
		if err != nil {
			return err
		}
		s.helperOut = bufio.NewReaderSize(helperOutput, 1<<20)
		if err := s.helper.Start(); err != nil {
			return fmt.Errorf("start application helper: %w", err)
		}
	}
	oldUmask := syscall.Umask(0o007)
	startErr := s.app.Start()
	syscall.Umask(oldUmask)
	if startErr != nil {
		stdout.Close()
		stderr.Close()
		return fmt.Errorf("start application: %w", startErr)
	}
	if err := dropLaunchCapabilities(); err != nil {
		_ = syscall.Kill(-s.app.Process.Pid, syscall.SIGKILL)
		return fmt.Errorf("drop launch capabilities: %w", err)
	}
	go func() {
		err := s.app.Wait()
		stdout.Close()
		stderr.Close()
		s.done <- err
	}()
	return nil
}

func (s *Server) waitForHealth(ctx context.Context) error {
	applicationURL, err := url.Parse(s.blueprint.Application.IO.ApplicationListen)
	if err != nil {
		return err
	}
	s.appTarget = applicationURL
	if s.blueprint.Application.IO.HealthPath == "" {
		return nil
	}
	timeout := time.Duration(s.blueprint.Application.IO.StartupTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-s.done:
			return fmt.Errorf("application exited during startup: %w", err)
		default:
		}
		requestURL := *applicationURL
		requestURL.Path = s.blueprint.Application.IO.HealthPath
		response, err := client.Get(requestURL.String())
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("application health check timed out")
}

func (s *Server) startHostBridge(ctx context.Context) error {
	appProxy := httputil.NewSingleHostReverseProxy(s.appTarget)
	appHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !s.servedRoute(request.Method, request.URL.Path) {
			writeError(writer, http.StatusNotImplemented, "unsupported_route", "route is not declared by the Sealed Environment")
			return
		}
		appProxy.ServeHTTP(writer, request)
	})
	controlHandler := s.controlHandler()
	if s.metadata.BridgeTransport == "unix" {
		if err := s.serveUnix(filepath.Join(s.paths.Sockets, "application.sock"), appHandler); err != nil {
			return err
		}
		return s.serveUnix(filepath.Join(s.paths.Sockets, "control.sock"), controlHandler)
	}
	spoolConfig := bridge.HTTPSpoolServerConfig{
		Root: filepath.Join(s.paths.Sockets, "http-spool"), Application: appHandler, Control: controlHandler,
		MaxBytes: s.blueprint.Limits.MaxBlobBytes, MaxConcurrentApplication: 8,
	}
	if err := os.MkdirAll(spoolConfig.Root, 0o700); err != nil {
		return err
	}
	go func() {
		if err := bridge.ServeHTTPSpool(ctx, spoolConfig); err != nil {
			select {
			case s.done <- fmt.Errorf("HTTP spool: %w", err):
			default:
			}
		}
	}()
	return nil
}

func (s *Server) controlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.statusHandler)
	mux.HandleFunc("POST /v1/turns", s.turnHandler)
	mux.HandleFunc("GET /v1/turns/{id}", s.turnStatusHandler)
	mux.HandleFunc("POST /v1/injections", s.armInjectionHandler)
	mux.HandleFunc("GET /v1/injections", s.listInjectionsHandler)
	mux.HandleFunc("DELETE /v1/injections/{id}", s.deleteInjectionHandler)
	mux.HandleFunc("POST /v1/snapshots", s.createSnapshotHandler)
	mux.HandleFunc("GET /v1/snapshots", s.listSnapshotsHandler)
	mux.HandleFunc("GET /v1/snapshots/{id}", s.inspectSnapshotHandler)
	mux.HandleFunc("GET /v1/attacker/{id}/observations", s.observationsHandler)
	mux.HandleFunc("GET /v1/events", s.eventsHandler)
	mux.HandleFunc("GET /v1/evaluation", s.evaluationHandler)
	mux.HandleFunc("GET /v1/state-journal", s.journalHandler)
	mux.HandleFunc("POST /v1/logs/verify", s.verifyHandler)
	mux.HandleFunc("POST /v1/quiesce", s.quiesceHandler)
	mux.HandleFunc("POST /v1/stop", s.stopHandler)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(writer, request)
	})
}

func (s *Server) statusHandler(writer http.ResponseWriter, _ *http.Request) {
	s.metadataMu.RLock()
	metadata := s.metadata
	s.metadataMu.RUnlock()
	writeJSON(writer, http.StatusOK, metadata)
}

func (s *Server) turnHandler(writer http.ResponseWriter, request *http.Request) {
	s.activityMu.RLock()
	defer s.activityMu.RUnlock()
	s.metadataMu.RLock()
	phase := s.metadata.Phase
	s.metadataMu.RUnlock()
	if phase != session.Running {
		writeError(writer, http.StatusConflict, "not_running", "session is not accepting turns")
		return
	}
	var input TurnRequest
	if err := decodeStrict(request, &input, 2<<20); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_turn", err.Error())
		return
	}
	input.Method = strings.ToUpper(input.Method)
	if !s.servedRoute(input.Method, input.Path) {
		writeError(writer, http.StatusBadRequest, "unsupported_route", "turn route is not declared")
		return
	}
	s.metadataMu.Lock()
	if s.metadata.Consumed.Turns >= s.metadata.Limits.MaxTurns {
		s.metadataMu.Unlock()
		writeError(writer, http.StatusTooManyRequests, "turn_limit", "turn limit is exhausted")
		return
	}
	s.metadata.Consumed.Turns++
	_ = session.Save(s.paths.Metadata, s.metadata)
	s.metadataMu.Unlock()
	id := fmt.Sprintf("turn-%d", s.nextTurn.Add(1))
	turn := Turn{ID: id, Status: "running", Started: time.Now().UTC()}
	s.turnMu.Lock()
	s.turns[id] = turn
	s.turnMu.Unlock()
	s.active.Add(1)
	defer s.active.Add(-1)
	target := *s.appTarget
	target.Path, target.RawQuery = input.Path, ""
	turnContext, cancel := context.WithTimeout(request.Context(), time.Duration(s.blueprint.Application.IO.TurnTimeoutMS)*time.Millisecond)
	defer cancel()
	forward, err := http.NewRequestWithContext(turnContext, input.Method, target.String(), bytes.NewReader(input.Body))
	if err == nil {
		for name, value := range input.Headers {
			if allowedTurnHeader(name) {
				forward.Header.Set(name, value)
			}
		}
		forward.Header.Set("X-Interceptor-Turn-ID", id)
		response, requestErr := http.DefaultClient.Do(forward)
		if requestErr != nil {
			err = requestErr
		} else {
			turn.Code = response.StatusCode
			turn.Headers = selectedHeaders(response.Header)
			turn.Body, err = readBounded(response.Body, 2<<20)
			response.Body.Close()
		}
	}
	turn.Finished = time.Now().UTC()
	if err != nil {
		turn.Status, turn.Error = "error", err.Error()
	} else {
		turn.Status = "complete"
	}
	s.turnMu.Lock()
	s.turns[id] = turn
	s.turnMu.Unlock()
	_, _ = s.recorder.Append(recording.Draft{Channel: recording.Attack, Layer: "application_bridge", Kind: "turn_completed", Provenance: recording.Application, TurnID: id, Semantic: map[string]any{"status": turn.Status, "code": turn.Code, "body": string(turn.Body), "error": turn.Error}})
	_ = s.recorder.Flush()
	writeJSON(writer, http.StatusOK, turn)
}

func (s *Server) turnStatusHandler(writer http.ResponseWriter, request *http.Request) {
	s.turnMu.RLock()
	turn, exists := s.turns[request.PathValue("id")]
	s.turnMu.RUnlock()
	if !exists {
		writeError(writer, http.StatusNotFound, "not_found", "turn not found")
		return
	}
	writeJSON(writer, http.StatusOK, turn)
}

func (s *Server) armInjectionHandler(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Definition   injection.Definition `json:"definition"`
		EligibleTurn string               `json:"eligible_turn,omitempty"`
	}
	if err := decodeStrict(request, &input, 1<<20); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_injection", err.Error())
		return
	}
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	extra, apply, err := s.fidelityMutation(request.Context(), input.Definition)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_injection", err.Error())
		return
	}
	if err := s.injections.ArmTransaction(input.Definition, input.EligibleTurn, extra, apply); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_injection", err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, injection.PublicState{Definition: input.Definition})
}

func (s *Server) fidelityMutation(ctx context.Context, definition injection.Definition) ([]statejournal.Operation, func() error, error) {
	if err := injection.Validate(definition); err != nil {
		return nil, nil, err
	}
	if definition.Surface != "environment_state" {
		return nil, nil, nil
	}
	selector := definition.Selector
	if selector.Service != "" {
		handler := s.services[selector.Service]
		if handler == nil {
			return nil, nil, fmt.Errorf("fidelity target service %q is not stateful", selector.Service)
		}
		current := handler.Snapshot()[selector.Collection][selector.Key]
		if len(current) == 0 {
			return nil, nil, errors.New("fidelity target resource does not exist")
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(current))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, nil, err
		}
		updated, err := injection.Apply(value, definition.Placement, definition.Payload)
		if err != nil {
			return nil, nil, err
		}
		encoded, err := json.Marshal(updated)
		if err != nil {
			return nil, nil, err
		}
		operation := statejournal.Operation{Kind: "service_put", Service: selector.Service, Collection: selector.Collection, Key: selector.Key, Value: encoded}
		return []statejournal.Operation{operation}, func() error { return handler.Apply(operation) }, nil
	}
	hostPath, ok := resolveMutablePath(s.containerRoots(), selector.Path)
	if !ok {
		return nil, nil, fmt.Errorf("fidelity path %q is outside declared mutable roots", selector.Path)
	}
	info, err := os.Lstat(hostPath)
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil, errors.New("fidelity file target must be an existing regular file")
	}
	content, err := os.ReadFile(hostPath)
	if err != nil {
		return nil, nil, err
	}
	var original any = string(content)
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	var decoded any
	if decoder.Decode(&decoded) == nil {
		original = decoded
	}
	updated, err := injection.Apply(original, definition.Placement, definition.Payload)
	if err != nil {
		return nil, nil, err
	}
	var encoded []byte
	if text, ok := updated.(string); ok {
		encoded = []byte(text)
	} else {
		encoded, err = json.Marshal(updated)
		if err != nil {
			return nil, nil, err
		}
	}
	object, err := s.blobs.Put(ctx, bytes.NewReader(encoded))
	if err != nil {
		return nil, nil, err
	}
	operation := statejournal.Operation{Kind: "file_upsert", Path: selector.Path, Mode: uint32(info.Mode().Perm()), Blob: object.Digest, Size: object.Size}
	return []statejournal.Operation{operation}, func() error {
		if err := fsutil.WriteFile(hostPath, encoded, info.Mode().Perm()); err != nil {
			return err
		}
		return nil
	}, nil
}

func (s *Server) listInjectionsHandler(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"injections": s.injections.ListPublic()})
}

func (s *Server) deleteInjectionHandler(writer http.ResponseWriter, request *http.Request) {
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if err := s.injections.Delete(request.PathValue("id")); err != nil {
		writeError(writer, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) createSnapshotHandler(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Label string `json:"label,omitempty"`
	}
	if err := decodeStrict(request, &input, 64<<10); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_snapshot", err.Error())
		return
	}
	checkpoint, err := s.createSnapshot(request.Context(), input.Label)
	if err != nil {
		writeError(writer, http.StatusConflict, "snapshot_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, checkpoint)
}

func (s *Server) createSnapshot(ctx context.Context, label string) (snapshot.Checkpoint, error) {
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if !s.blueprint.Application.Persistence.SnapshotCapable {
		return snapshot.Checkpoint{}, errors.New("application is not snapshot capable")
	}
	s.metadataMu.RLock()
	snapshotAttempts, maxSnapshots := s.metadata.Consumed.SnapshotAttempts, s.metadata.Limits.MaxSnapshots
	s.metadataMu.RUnlock()
	if snapshotAttempts >= maxSnapshots {
		return snapshot.Checkpoint{}, errors.New("snapshot limit is exhausted")
	}
	s.metadataMu.Lock()
	s.metadata.Phase = session.Quiescing
	_ = session.Save(s.paths.Metadata, s.metadata)
	s.metadataMu.Unlock()
	defer func() {
		s.metadataMu.Lock()
		if s.metadata.Phase == session.Quiescing {
			s.metadata.Phase = session.Running
			_ = session.Save(s.paths.Metadata, s.metadata)
		}
		s.metadataMu.Unlock()
	}()
	if err := s.quiesce(ctx); err != nil {
		return snapshot.Checkpoint{}, err
	}
	if err := s.recorder.Flush(); err != nil {
		return snapshot.Checkpoint{}, err
	}
	roots := s.containerRoots()
	first, err := s.scanMutable(ctx, roots)
	if err != nil {
		return snapshot.Checkpoint{}, err
	}
	time.Sleep(100 * time.Millisecond)
	second, err := s.scanMutable(ctx, roots)
	if err != nil || second.Digest != first.Digest {
		return snapshot.Checkpoint{}, errors.New("mutable filesystem did not reach a stable boundary")
	}
	if err := syscall.Kill(-s.app.Process.Pid, syscall.SIGSTOP); err != nil {
		return snapshot.Checkpoint{}, fmt.Errorf("pause application: %w", err)
	}
	defer syscall.Kill(-s.app.Process.Pid, syscall.SIGCONT)
	current, err := s.scanMutable(ctx, roots)
	if err != nil {
		return snapshot.Checkpoint{}, err
	}
	previous, err := snapshot.LoadManifest(s.paths.FileManifest)
	if err != nil {
		return snapshot.Checkpoint{}, err
	}
	operations, err := snapshot.Delta(previous, current)
	if err != nil {
		return snapshot.Checkpoint{}, err
	}
	if len(operations) > 0 {
		if _, err := s.journal.Append("snapshot_filesystem_delta", operations); err != nil {
			return snapshot.Checkpoint{}, err
		}
	}
	if err := snapshot.WriteManifest(s.paths.FileManifest, current); err != nil {
		return snapshot.Checkpoint{}, err
	}
	s.metadataMu.Lock()
	s.metadata.Consumed.SnapshotAttempts++
	budget, _ := json.Marshal(s.metadata.Consumed)
	s.metadataMu.Unlock()
	report, err := s.evaluate()
	if err != nil {
		return snapshot.Checkpoint{}, err
	}
	oracleState, _ := json.Marshal(report.Oracles)
	s.virtualMu.RLock()
	clock, _ := json.Marshal(s.virtualTime.Format(time.RFC3339Nano))
	s.virtualMu.RUnlock()
	if _, err := s.journal.Append("snapshot_state", []statejournal.Operation{{Kind: "budget_set", Value: budget}, {Kind: "oracle_state_set", Value: oracleState}, {Kind: "clock_set", Value: clock}}); err != nil {
		return snapshot.Checkpoint{}, err
	}
	journalSeq, journalHash := s.journal.Head()
	eventSeq, _ := s.recorder.Head()
	s.metadataMu.RLock()
	metadataForCheckpoint := s.metadata
	s.metadataMu.RUnlock()
	checkpoint, err := snapshot.NewCheckpoint(metadataForCheckpoint.ID, metadataForCheckpoint.ParentCheckpoint, metadataForCheckpoint.EnvironmentDigest, metadataForCheckpoint.AppDigest, journalSeq, journalHash, current.Digest, eventSeq, label, buildinfo.Version, time.Now())
	if err != nil {
		return snapshot.Checkpoint{}, err
	}
	if err := snapshot.WriteCheckpoint(s.paths.Checkpoints, checkpoint); err != nil {
		return snapshot.Checkpoint{}, err
	}
	_, _ = s.recorder.Append(recording.Draft{Channel: recording.Control, Layer: "snapshot", Kind: "snapshot_ready", Provenance: recording.SandboxControl, Semantic: checkpoint})
	if err := s.recorder.Flush(); err != nil {
		return snapshot.Checkpoint{}, err
	}
	s.metadataMu.RLock()
	metadata := s.metadata
	s.metadataMu.RUnlock()
	if err := session.Save(s.paths.Metadata, metadata); err != nil {
		return snapshot.Checkpoint{}, err
	}
	return checkpoint, nil
}

func (s *Server) listSnapshotsHandler(writer http.ResponseWriter, _ *http.Request) {
	items, err := snapshot.ListCheckpoints(s.paths.Checkpoints)
	if err != nil {
		writeError(writer, 500, "checkpoint_error", err.Error())
		return
	}
	writeJSON(writer, 200, map[string]any{"checkpoints": items})
}

func (s *Server) inspectSnapshotHandler(writer http.ResponseWriter, request *http.Request) {
	item, err := snapshot.LoadCheckpoint(s.paths.Checkpoints, request.PathValue("id"))
	if err != nil {
		writeError(writer, 404, "not_found", err.Error())
		return
	}
	writeJSON(writer, 200, item)
}

func (s *Server) observationsHandler(writer http.ResponseWriter, request *http.Request) {
	sink := s.sinks[request.PathValue("id")]
	if sink == nil {
		writeError(writer, 404, "not_found", "attacker service not found")
		return
	}
	writeJSON(writer, 200, map[string]any{"observations": sink.Observations()})
}

func (s *Server) eventsHandler(writer http.ResponseWriter, request *http.Request) {
	events, err := recording.VerifyFile(s.paths.Events)
	if err != nil {
		writeError(writer, 500, "verification_failed", err.Error())
		return
	}
	channel := request.URL.Query().Get("channel")
	if channel == "" {
		channel = string(recording.Attack)
	}
	if channel != string(recording.Attack) && channel != string(recording.Control) {
		writeError(writer, 400, "invalid_channel", "cooperative events expose attack or control only")
		return
	}
	filtered := []recording.Event{}
	for _, event := range events {
		if string(event.Channel) == channel {
			filtered = append(filtered, event)
		}
	}
	writeJSON(writer, 200, map[string]any{"events": filtered})
}

func (s *Server) evaluationHandler(writer http.ResponseWriter, _ *http.Request) {
	report, err := s.evaluate()
	if err != nil {
		writeError(writer, 500, "evaluation_failed", err.Error())
		return
	}
	writeJSON(writer, 200, report)
}

func (s *Server) journalHandler(writer http.ResponseWriter, _ *http.Request) {
	items, err := statejournal.VerifyFile(s.paths.Journal)
	if err != nil {
		writeError(writer, 500, "verification_failed", err.Error())
		return
	}
	writeJSON(writer, 200, map[string]any{"transactions": items})
}

func (s *Server) verifyHandler(writer http.ResponseWriter, _ *http.Request) {
	events, eventErr := recording.VerifyFile(s.paths.Events)
	transactions, journalErr := statejournal.VerifyFile(s.paths.Journal)
	if eventErr != nil || journalErr != nil {
		writeError(writer, 500, "verification_failed", errors.Join(eventErr, journalErr).Error())
		return
	}
	writeJSON(writer, 200, map[string]any{"events": len(events), "transactions": len(transactions)})
}

func (s *Server) quiesceHandler(writer http.ResponseWriter, request *http.Request) {
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if err := s.quiesce(request.Context()); err != nil {
		writeError(writer, 409, "quiesce_failed", err.Error())
		return
	}
	writeJSON(writer, 200, map[string]any{"quiescent": true})
}

func (s *Server) stopHandler(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, 202, map[string]any{"stopping": true})
	go func() { time.Sleep(50 * time.Millisecond); s.done <- nil }()
}

func (s *Server) quiesce(ctx context.Context) error {
	config := s.blueprint.Application.Persistence.Quiesce
	timeout := time.Duration(config.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	operationContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch config.Kind {
	case "none":
		return nil
	case "http":
		request, err := http.NewRequestWithContext(operationContext, strings.ToUpper(config.Method), config.URL, nil)
		if err != nil {
			return err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		io.Copy(io.Discard, response.Body)
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return fmt.Errorf("quiesce endpoint returned %d", response.StatusCode)
		}
		return nil
	case "command":
		return s.runHelperCommand(operationContext, config.Command, timeout)
	case "signal":
		signal, err := namedSignal(config.Signal)
		if err != nil {
			return err
		}
		if err := s.app.Process.Signal(signal); err != nil {
			return err
		}
		for {
			if config.ReadyFile != "" {
				if _, err := os.Stat(config.ReadyFile); err == nil {
					return nil
				}
			} else if config.ReadyURL != "" {
				request, _ := http.NewRequestWithContext(operationContext, http.MethodGet, config.ReadyURL, nil)
				if response, err := http.DefaultClient.Do(request); err == nil {
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if response.StatusCode >= 200 && response.StatusCode < 300 {
						return nil
					}
				}
			}
			select {
			case <-operationContext.Done():
				return operationContext.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	default:
		return fmt.Errorf("unsupported quiesce kind %q", config.Kind)
	}
}

func (s *Server) Close(ctx context.Context) error {
	var result error
	s.closeOnce.Do(func() {
		for _, server := range s.servers {
			result = errors.Join(result, server.Shutdown(ctx))
		}
		for _, listener := range s.listeners {
			_ = listener.Close()
		}
		if s.app != nil && s.app.Process != nil {
			_ = syscall.Kill(-s.app.Process.Pid, syscall.SIGTERM)
		}
		if s.helper != nil && s.helper.Process != nil {
			_ = syscall.Kill(-s.helper.Process.Pid, syscall.SIGTERM)
		}
		s.metadataMu.Lock()
		if s.metadata.Phase != session.Error {
			s.metadata.Phase = session.Stopped
		}
		_ = session.Save(s.paths.Metadata, s.metadata)
		s.metadataMu.Unlock()
		_, _ = s.recorder.Append(recording.Draft{Channel: recording.Control, Layer: "sandboxd", Kind: "session_stopped", Provenance: recording.SandboxControl, Semantic: map[string]any{}})
		result = errors.Join(result, s.recorder.Close(), s.journal.Close())
	})
	return result
}

func (s *Server) serveTCP(address string, handler http.Handler) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	s.listeners = append(s.listeners, listener)
	s.servers = append(s.servers, server)
	go server.Serve(listener)
	return nil
}

func (s *Server) trackActivity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		s.activityMu.RLock()
		defer s.activityMu.RUnlock()
		s.active.Add(1)
		defer s.active.Add(-1)
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) consumeTokens(tokens int64) {
	s.metadataMu.Lock()
	s.metadata.Consumed.Tokens += tokens
	exceeded := s.metadata.Consumed.Tokens > s.metadata.Limits.MaxTokens
	if exceeded {
		s.metadata.Phase = session.Error
		s.metadata.TerminationReason = "TOKEN_LIMIT"
	}
	_ = session.Save(s.paths.Metadata, s.metadata)
	s.metadataMu.Unlock()
	if exceeded {
		select {
		case s.done <- errors.New("model token limit exceeded"):
		default:
		}
	}
}

func (s *Server) enforceWallLimit() {
	s.metadataMu.RLock()
	duration := time.Duration(s.metadata.Limits.MaxWallSeconds) * time.Second
	s.metadataMu.RUnlock()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	<-timer.C
	s.metadataMu.Lock()
	if s.metadata.Phase == session.Running || s.metadata.Phase == session.Quiescing {
		s.metadata.Phase, s.metadata.TerminationReason = session.Error, "WALL_TIME_LIMIT"
		_ = session.Save(s.paths.Metadata, s.metadata)
		s.metadataMu.Unlock()
		select {
		case s.done <- errors.New("wall time limit exceeded"):
		default:
		}
		return
	}
	s.metadataMu.Unlock()
}

func (s *Server) replayManagedState() error {
	transactions, err := statejournal.VerifyFile(s.paths.Journal)
	if err != nil {
		return err
	}
	for _, transaction := range transactions {
		for _, operation := range transaction.Operations {
			switch operation.Kind {
			case "service_put", "service_delete":
				handler := s.services[operation.Service]
				if handler == nil {
					return fmt.Errorf("journal references unknown service %q", operation.Service)
				}
				if err := handler.Apply(operation); err != nil {
					return err
				}
			case "injection_put", "injection_delete":
				if err := s.injections.ApplyJournal(operation); err != nil {
					return err
				}
			case "oracle_state_set":
				var values []evaluation.OracleResult
				if err := json.Unmarshal(operation.Value, &values); err != nil {
					return err
				}
				s.oracleMu.Lock()
				for _, value := range values {
					s.oracleState[value.ID] = value
				}
				s.oracleMu.Unlock()
			case "budget_set":
				var consumed session.Consumed
				if err := json.Unmarshal(operation.Value, &consumed); err != nil {
					return err
				}
				s.metadataMu.Lock()
				s.metadata.Consumed = consumed
				s.metadataMu.Unlock()
			case "clock_set":
				var value string
				if err := json.Unmarshal(operation.Value, &value); err != nil {
					return err
				}
				parsed, err := time.Parse(time.RFC3339Nano, value)
				if err != nil {
					return err
				}
				s.virtualMu.Lock()
				s.virtualTime = parsed.UTC()
				s.virtualMu.Unlock()
			}
		}
	}
	return nil
}

func (s *Server) evaluate() (evaluation.Report, error) {
	events, err := recording.VerifyFile(s.paths.Events)
	if err != nil {
		return evaluation.Report{}, err
	}
	serviceState := map[string]map[string]map[string]json.RawMessage{}
	for id, handler := range s.services {
		serviceState[id] = handler.Snapshot()
	}
	report, err := evaluation.Evaluate(evaluation.Input{
		Blueprint: s.blueprint, Events: events, Injections: s.injections.OperatorEvaluation(),
		Blobs: s.blobs, Services: serviceState, MutableRoot: s.containerRoots(),
	})
	if err != nil {
		return evaluation.Report{}, err
	}
	s.oracleMu.Lock()
	for index, result := range report.Oracles {
		if previous, exists := s.oracleState[result.ID]; exists && previous.Fired && !result.Fired {
			report.Oracles[index] = previous
			result = previous
		}
		if result.Fired {
			s.oracleState[result.ID] = result
		}
	}
	s.oracleMu.Unlock()
	evaluation.RecomputeInjections(&report, s.injections.OperatorEvaluation())
	return report, nil
}

func namedSignal(value string) (os.Signal, error) {
	switch strings.ToUpper(strings.TrimPrefix(value, "SIG")) {
	case "USR1":
		return syscall.SIGUSR1, nil
	case "USR2":
		return syscall.SIGUSR2, nil
	case "HUP":
		return syscall.SIGHUP, nil
	case "TERM":
		return syscall.SIGTERM, nil
	default:
		return nil, fmt.Errorf("unsupported quiesce signal %q", value)
	}
}

func resolveMutablePath(roots []snapshot.Root, containerPath string) (string, bool) {
	for _, root := range roots {
		if containerPath == root.ContainerPath || strings.HasPrefix(containerPath, root.ContainerPath+"/") {
			relative := strings.TrimPrefix(strings.TrimPrefix(containerPath, root.ContainerPath), "/")
			return filepath.Join(root.HostPath, filepath.FromSlash(relative)), true
		}
	}
	return "", false
}

func (s *Server) serveUnix(path string, handler http.Handler) error {
	server, listener, err := bridge.ListenUnix(path, handler)
	if err != nil {
		return err
	}
	s.listeners = append(s.listeners, listener)
	s.servers = append(s.servers, server)
	go server.Serve(listener)
	return nil
}

func (s *Server) prepareMutableRoots() error {
	for _, root := range s.blueprint.Application.Persistence.MutableRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
				return nil
			}
			mode := info.Mode().Perm()
			if info.IsDir() {
				mode |= 0o770
			} else if info.Mode().IsRegular() {
				mode |= 0o660
			}
			return os.Chmod(path, mode)
		})
		if err != nil {
			return fmt.Errorf("prepare mutable root %s: %w", root, err)
		}
	}
	return nil
}

func (s *Server) runHelperCommand(ctx context.Context, command []string, timeout time.Duration) error {
	if s.helper == nil || s.helperIn == nil || s.helperOut == nil {
		return errors.New("application command helper is unavailable")
	}
	s.helperMu.Lock()
	defer s.helperMu.Unlock()
	request := HelperRequest{Command: command, TimeoutMS: timeout.Milliseconds()}
	encoded, _ := json.Marshal(request)
	if _, err := s.helperIn.Write(append(encoded, '\n')); err != nil {
		return err
	}
	type result struct {
		response HelperResponse
		err      error
	}
	channel := make(chan result, 1)
	go func() {
		line, err := s.helperOut.ReadBytes('\n')
		var response HelperResponse
		if err == nil {
			err = json.Unmarshal(line, &response)
		}
		channel <- result{response: response, err: err}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case value := <-channel:
		if value.err != nil {
			return value.err
		}
		if !value.response.OK {
			return fmt.Errorf("quiesce command failed: %s", value.response.Error)
		}
		return nil
	}
}

func (s *Server) containerRoots() []snapshot.Root {
	result := []snapshot.Root{}
	for _, root := range s.blueprint.Application.Persistence.MutableRoots {
		result = append(result, snapshot.Root{ContainerPath: root, HostPath: root})
	}
	return result
}

func (s *Server) scanMutable(ctx context.Context, roots []snapshot.Root) (snapshot.Manifest, error) {
	if len(roots) == 0 {
		return snapshot.EmptyManifest(), nil
	}
	return snapshot.Scan(ctx, roots, s.blobs, s.blueprint.Limits.MaxBlobBytes)
}

func (s *Server) servedRoute(method, path string) bool {
	for _, route := range s.blueprint.Application.IO.ServedRoutes {
		if strings.EqualFold(route.Method, method) && route.Path == path {
			return true
		}
	}
	return false
}

func optionStrings(value any) []string {
	raw, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]string); ok {
			return typed
		}
		return nil
	}
	result := []string{}
	for _, item := range raw {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value < 0 {
		return fallback
	}
	return value
}
func allowedTurnHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Content-Type", "Accept", "If-Match", "If-None-Match":
		return true
	}
	return false
}
func selectedHeaders(headers http.Header) map[string]string {
	result := map[string]string{}
	for _, name := range []string{"Content-Type", "ETag", "Location"} {
		if value := headers.Get(name); value != "" {
			result[name] = value
		}
	}
	return result
}
func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("body exceeds limit")
	}
	return data, nil
}
func decodeStrict(request *http.Request, output any, limit int64) error {
	data, err := readBounded(request.Body, limit)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) == nil {
		return errors.New("request contains trailing JSON")
	}
	return nil
}
func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]any{"code": code, "message": message, "request_id": fmt.Sprintf("req-%d", controlErrorSequence.Add(1))}})
}

var controlErrorSequence atomic.Uint64
