package sandboxd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/appimage"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"github.com/intrusive-ai/interceptor-sandbox/internal/buildinfo"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evaluation"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/identity"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/services"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/simnet"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

const (
	containerEnvironment = "/opt/interceptor/environment"
	containerSession     = "/run/interceptor-session"
	containerBlobs       = "/run/interceptor-blobs"
)

type Server struct {
	feedbackMu      sync.Mutex
	execution       *adaptive.ExecutionStore
	blueprint       *environment.Blueprint
	metadata        session.Metadata
	paths           session.Paths
	recorder        *recording.Writer
	journal         *statejournal.Writer
	blobs           *blobstore.Store
	artifacts       *adaptive.ArtifactRegistry
	attempts        *adaptive.AttemptRegistry
	injections      *injection.Manager
	identities      map[string]*identity.Authority
	identityEnv     map[string]string
	identityFiles   []string
	identityStop    chan struct{}
	services        map[string]services.ManagedHandler
	sinks           map[string]*services.SinkHandler
	hostRoutes      map[string]http.Handler
	dnsServer       *simnet.DNSServer
	servers         []*http.Server
	listeners       []net.Listener
	adapter         applicationAdapter
	app             *exec.Cmd
	helper          *exec.Cmd
	helperIn        io.WriteCloser
	helperOut       *bufio.Reader
	helperMu        sync.Mutex
	helperPendingMu sync.Mutex
	helperPending   map[string]chan HelperResponse
	nextHelper      atomic.Uint64
	appTarget       *url.URL
	appUID          uint32
	appGID          uint32
	hostUID         int
	hostGID         int
	active          atomic.Int64
	activityMu      sync.RWMutex
	metadataMu      sync.RWMutex
	oracleMu        sync.Mutex
	oracleState     map[string]evaluation.OracleResult
	virtualMu       sync.RWMutex
	virtualTime     time.Time
	controlMu       sync.Mutex
	turnMu          sync.RWMutex
	turns           map[string]Turn
	nextTurn        atomic.Uint64
	nextInvocation  atomic.Uint64
	done            chan error
	closeOnce       sync.Once
}

type TurnRequest struct {
	Operation       string            `json:"operation,omitempty"`
	Input           []byte            `json:"input,omitempty"`
	MediaType       string            `json:"media_type,omitempty"`
	Method          string            `json:"method,omitempty"`
	Path            string            `json:"path,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Body            []byte            `json:"body,omitempty"`
	CallerPrincipal string            `json:"caller_principal,omitempty"`
	AttemptID       string            `json:"attempt_id,omitempty"`
	PayloadDigest   string            `json:"payload_digest,omitempty"`
	ArtifactDigest  string            `json:"artifact_digest,omitempty"`
}

type Turn struct {
	OutputContractStatus string            `json:"output_contract_status,omitempty"`
	ID                   string            `json:"id"`
	Operation            string            `json:"operation,omitempty"`
	Status               string            `json:"status"`
	Code                 int               `json:"code,omitempty"`
	Headers              map[string]string `json:"headers,omitempty"`
	Body                 []byte            `json:"body,omitempty"`
	MediaType            string            `json:"media_type,omitempty"`
	ExitCode             *int              `json:"exit_code,omitempty"`
	Error                string            `json:"error,omitempty"`
	AttemptID            string            `json:"attempt_id,omitempty"`
	PayloadDigest        string            `json:"payload_digest,omitempty"`
	OutputDigest         string            `json:"output_digest,omitempty"`
	Started              time.Time         `json:"started_at"`
	Finished             time.Time         `json:"finished_at,omitempty"`
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
		Evaluation:   filepath.Join(containerSession, "final-evaluation.json"),
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
	transactions, err := statejournal.VerifyFile(paths.Journal)
	if err != nil {
		journal.Close()
		recorder.Close()
		return nil, err
	}
	if err := adaptive.RestoreRegistries(paths.Root, transactions); err != nil {
		journal.Close()
		recorder.Close()
		return nil, err
	}
	artifacts, err := adaptive.OpenArtifactRegistry(filepath.Join(paths.Root, "artifacts.json"), blobs, report.Blueprint.Limits.MaxBlobBytes)
	if err != nil {
		journal.Close()
		recorder.Close()
		return nil, err
	}
	if err := artifacts.SetMaximumItems(report.Blueprint.Adaptive.MaxArtifacts); err != nil {
		journal.Close()
		recorder.Close()
		return nil, err
	}
	feedback := metadata.FeedbackProfile
	if feedback == "" {
		feedback = "black-box"
	}
	attempts, err := adaptive.OpenAttemptRegistry(filepath.Join(paths.Root, "attempts.json"), feedback, report.Blueprint.Adaptive.MaxAttempts, artifacts)
	if err != nil {
		journal.Close()
		recorder.Close()
		return nil, err
	}
	var execution *adaptive.ExecutionStore
	if metadata.OperationAPIVersion != "" {
		if metadata.OperationAPIVersion != adaptive.OperationRequestVersion {
			journal.Close()
			recorder.Close()
			return nil, errors.New("unsupported operation contract")
		}
		execution, err = adaptive.OpenExecutionStore(paths.Root, metadata.ID, journal)
		if err != nil {
			journal.Close()
			recorder.Close()
			return nil, err
		}
	}
	appUID := uint32(envInt("INTERCEPTOR_APP_UID", 20000))
	appGID := uint32(envInt("INTERCEPTOR_APP_GID", 20000))
	return &Server{
		execution: execution, blueprint: report.Blueprint, metadata: metadata, paths: paths, recorder: recorder, journal: journal, blobs: blobs, artifacts: artifacts, attempts: attempts,
		injections: injection.NewManager(journal, recorder), identities: map[string]*identity.Authority{}, identityEnv: map[string]string{},
		identityStop: make(chan struct{}),
		services:     map[string]services.ManagedHandler{}, sinks: map[string]*services.SinkHandler{}, hostRoutes: map[string]http.Handler{},
		appUID: appUID, appGID: appGID, hostUID: envInt("INTERCEPTOR_HOST_UID", 0), hostGID: envInt("INTERCEPTOR_HOST_GID", 0),
		turns: map[string]Turn{}, oracleState: map[string]evaluation.OracleResult{}, virtualTime: report.Blueprint.Time.Base.UTC(), done: make(chan error, 1),
	}, nil
}

func (s *Server) Start(ctx context.Context) error {
	if err := s.prepareMutableRoots(); err != nil {
		return err
	}
	if err := s.startIdentityAuthorities(); err != nil {
		return err
	}
	if err := s.startSyntheticServices(); err != nil {
		return err
	}
	if err := s.startNetworkSimulation(); err != nil {
		return err
	}
	if err := s.verifyApplicationManagedTLS(ctx); err != nil {
		return err
	}
	if err := s.prepareIdentityDeliveries(ctx); err != nil {
		return err
	}
	if err := s.startModelProxy(); err != nil {
		return err
	}
	adapter, err := s.newApplicationAdapter()
	if err != nil {
		return err
	}
	s.adapter = adapter
	if err := s.adapter.Start(); err != nil {
		return err
	}
	if s.blueprint.Application.Persistence.Quiesce.Kind == "command" && s.helper == nil {
		if err := s.startApplicationHelper(false); err != nil {
			return err
		}
	}
	if err := dropLaunchCapabilities(); err != nil {
		if s.app != nil && s.app.Process != nil {
			_ = syscall.Kill(-s.app.Process.Pid, syscall.SIGKILL)
		}
		return fmt.Errorf("drop launch capabilities: %w", err)
	}
	readyContext, cancel := context.WithTimeout(ctx, s.startupTimeout())
	defer cancel()
	if err := s.adapter.WaitReady(readyContext); err != nil {
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
	pid := 0
	if s.app != nil && s.app.Process != nil {
		pid = s.app.Process.Pid
	}
	_, _ = s.recorder.Append(recording.Draft{Channel: recording.Control, Layer: "sandboxd", Kind: "application_ready", Provenance: recording.SandboxControl, Semantic: map[string]any{"pid": pid, "interface": s.adapter.Kind()}})
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
		case "mcp_http":
			seedPath := ""
			if definition.Seed != "" {
				seedPath = filepath.Join(containerEnvironment, definition.Seed)
			}
			mcp, err := services.LoadMCP(filepath.Join(containerEnvironment, definition.Tools), seedPath, definition.Endpoint, definition.ID, s.journal, s.recorder)
			if err != nil {
				return err
			}
			s.services[definition.ID], handler = mcp, mcp
		}
		handler = services.WithResponseInjection(definition.ID, definition.Kind, handler, s.injections, s.recorder)
		if definition.Authorization != nil {
			authority := s.identities[definition.Authorization.Issuer]
			if authority == nil {
				return fmt.Errorf("service %s references unavailable identity issuer %s", definition.ID, definition.Authorization.Issuer)
			}
			handler = authority.Authorize(definition.ID, *definition.Authorization, handler)
		}
		handler = s.trackActivity(handler)
		s.registerHostRoutes(definition.Hostnames, handler)
		if err := s.serveTCP(definition.Listen, handler); err != nil {
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
		var handler http.Handler = sink
		if definition.Response.Mode == "dynamic" {
			handler = services.WithResponseInjection(definition.ID, definition.Kind, handler, s.injections, s.recorder)
		}
		handler = s.trackActivity(handler)
		s.registerHostRoutes(definition.Hostnames, handler)
		if err := s.serveTCP(definition.Listen, handler); err != nil {
			return fmt.Errorf("start attacker service %s: %w", definition.ID, err)
		}
	}
	return nil
}

func (s *Server) startIdentityAuthorities() error {
	for _, definition := range s.blueprint.Identity.Issuers {
		authority, err := identity.NewAuthority(
			definition, s.blueprint.Identity.Principals, s.blueprint.Identity.Grants,
			s.blueprint.Identity.Delegations, s.blueprint.Identity.Deliveries,
			s.blueprint.Services, s.recorder,
		)
		if err != nil {
			return fmt.Errorf("initialize identity issuer %s: %w", definition.ID, err)
		}
		s.identities[definition.ID] = authority
		s.services[definition.ID] = authority
		handler := s.trackActivity(authority)
		s.registerHostRoutes(definition.Hostnames, handler)
		if err := s.serveTCP(definition.Listen, handler); err != nil {
			return fmt.Errorf("start identity issuer %s: %w", definition.ID, err)
		}
	}
	return nil
}

func (s *Server) registerHostRoutes(hostnames []string, handler http.Handler) {
	for _, hostname := range hostnames {
		s.hostRoutes[strings.ToLower(strings.TrimSuffix(hostname, "."))] = handler
	}
}

func (s *Server) simulatedHostnames() []string {
	hostnames := make([]string, 0, len(s.hostRoutes))
	for hostname := range s.hostRoutes {
		hostnames = append(hostnames, hostname)
	}
	sort.Strings(hostnames)
	return hostnames
}

func (s *Server) startNetworkSimulation() error {
	if s.blueprint.Network.DNS.Enabled {
		server, err := simnet.StartDNS(s.blueprint.Network.DNS.Listen, s.simulatedHostnames(), s.blueprint.Network.DNS.UnknownPolicy, s.blueprint.Network.DNS.MaxQueries, s.recorder)
		if err != nil {
			return fmt.Errorf("start DNS simulation: %w", err)
		}
		s.dnsServer = server
	}
	if s.blueprint.Network.HTTP.Enabled {
		if err := s.serveTCP(s.blueprint.Network.HTTP.Listen, simnet.NewHostRouter(s.hostRoutes, s.recorder)); err != nil {
			return fmt.Errorf("start HTTP simulation front door: %w", err)
		}
	}
	if s.blueprint.Network.TLS.Enabled {
		if err := s.serveTLS(s.blueprint.Network.TLS.Listen, simnet.NewHostRouter(s.hostRoutes, s.recorder)); err != nil {
			return fmt.Errorf("start HTTPS simulation front door: %w", err)
		}
	}
	return nil
}

func (s *Server) verifyApplicationManagedTLS(ctx context.Context) error {
	configuration := s.blueprint.Network.TLS
	if !configuration.Enabled || configuration.TrustMode != "application-managed" {
		return nil
	}
	probeContext, cancel := context.WithTimeout(ctx, s.startupTimeout())
	defer cancel()
	command := exec.CommandContext(probeContext, configuration.ApplicationManagedProbe[0], configuration.ApplicationManagedProbe[1:]...)
	_, port, _ := net.SplitHostPort(configuration.Listen)
	probeURL := "https://" + s.simulatedHostnames()[0]
	if port != "443" {
		probeURL += ":" + port
	}
	probeURL += simnet.TLSProbePath
	s.configureApplicationCommand(command, []string{"INTERCEPTOR_TLS_PROBE_URL=" + probeURL})
	stdout, stderr := &boundedBuffer{limit: 64 << 10}, &boundedBuffer{limit: 64 << 10}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("application-managed TLS trust probe failed: %w: %s", err, strings.TrimSpace(string(stderr.bytes())))
	}
	return nil
}

func (s *Server) prepareIdentityDeliveries(ctx context.Context) error {
	for _, delivery := range s.blueprint.Identity.Deliveries {
		authority := s.identities[delivery.Issuer]
		if authority == nil {
			return fmt.Errorf("identity delivery %s references unavailable issuer", delivery.ID)
		}
		values, err := authority.DeliveryEnvironment(delivery)
		if err != nil {
			return fmt.Errorf("prepare identity delivery %s: %w", delivery.ID, err)
		}
		for name, value := range values {
			s.identityEnv[name] = value
		}
		if delivery.Kind != "projected_jwt" {
			continue
		}
		if err := s.writeProjectedToken(authority, delivery); err != nil {
			return fmt.Errorf("write projected identity %s: %w", delivery.ID, err)
		}
		s.identityFiles = append(s.identityFiles, delivery.Path)
		ttl := time.Duration(15) * time.Minute
		for _, issuer := range s.blueprint.Identity.Issuers {
			if issuer.ID == delivery.Issuer && issuer.AccessTokenTTLSeconds > 0 {
				ttl = time.Duration(issuer.AccessTokenTTLSeconds) * time.Second
			}
		}
		interval := ttl / 2
		if interval < time.Second {
			interval = time.Second
		}
		go func(authority *identity.Authority, delivery environment.IdentityDelivery, interval time.Duration) {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-s.identityStop:
					return
				case <-ticker.C:
					if err := s.writeProjectedToken(authority, delivery); err != nil {
						_, _ = s.recorder.Append(recording.Draft{Channel: recording.Evaluation, Layer: "identity", Kind: "identity_projection_failed", Provenance: recording.SandboxControl, Semantic: map[string]any{"delivery": delivery.ID, "error": err.Error()}})
					}
				}
			}
		}(authority, delivery, interval)
	}
	return nil
}

func (s *Server) writeProjectedToken(authority *identity.Authority, delivery environment.IdentityDelivery) error {
	token, _, err := authority.ProjectedToken(delivery)
	if err != nil {
		return err
	}
	return fsutil.WriteFile(delivery.Path, []byte(token+"\n"), 0o640)
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
	proxy, err := bridge.NewModelProxy(s.metadata.ID, connector, s.injections, s.blobs, s.recorder, s.blueprint.Limits.MaxBlobBytes, time.Duration(s.blueprint.Application.Interface.TurnTimeoutMS)*time.Millisecond)
	if err != nil {
		return err
	}
	proxy.SetTokenObserver(s.consumeTokens)
	return s.serveTCP(baseURL.Host, s.trackActivity(proxy))
}

func (s *Server) startHostBridge(ctx context.Context) error {
	appHandler := s.trackActivity(s.adapter.Handler())
	if a, ok := s.adapter.(*protocolApplicationAdapter); ok && a.stateEnabled() {
		// State adapters admit the entire request before acquiring activity locks.
		appHandler = s.adapter.Handler()
	}
	controlHandler := s.controlHandler()
	if s.metadata.BridgeTransport == "unix" {
		if err := s.serveUnix(filepath.Join(s.paths.Sockets, "application.sock"), appHandler); err != nil {
			return err
		}
		return s.serveUnix(filepath.Join(s.paths.Sockets, "control.sock"), controlHandler)
	}
	spoolConfig := bridge.HTTPSpoolServerConfig{
		Root: filepath.Join(s.paths.Sockets, "http-spool"), Application: appHandler, Control: controlHandler,
		MaxBytes: adaptive.OperationFrameLimit(s.blueprint.Limits.MaxBlobBytes), MaxConcurrentApplication: 8, ControlClass: operationControlClass,
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
	mux.HandleFunc("GET /v1/capabilities", s.capabilitiesHandler)
	mux.HandleFunc("POST /v1/artifacts", s.registerArtifactHandler)
	mux.HandleFunc("GET /v1/artifacts", s.listArtifactsHandler)
	mux.HandleFunc("GET /v1/artifacts/{digest}", s.readArtifactHandler)
	mux.HandleFunc("POST /v1/attempts", s.registerAttemptHandler)
	mux.HandleFunc("GET /v1/attempts", s.listAttemptsHandler)
	mux.HandleFunc("GET /v1/observations/{attempt}", s.observationViewHandler)
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
	mux.HandleFunc("POST /v1/identity/caller-tokens", s.callerTokenHandler)
	mux.HandleFunc("POST /v1/logs/verify", s.verifyHandler)
	mux.HandleFunc("POST /v1/quiesce", s.quiesceHandler)
	mux.HandleFunc("POST /v1/stop", s.stopHandler)
	mux.HandleFunc("POST /v1/operations", func(w http.ResponseWriter, r *http.Request) { s.operationsHandler(w, r, mux) })
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		if s.execution != nil && request.Method != http.MethodGet && request.URL.Path != "/v1/operations" {
			// Explicit local administrator stop remains available. Operator uses session.stop.
			if request.URL.Path == "/v1/stop" {
				if err := s.execution.Close("administrator_stop"); err != nil {
					writeError(writer, 503, "closure_failed", err.Error())
					return
				}
			} else {
				release, err := s.execution.LocalMutation(request.Context())
				if err != nil {
					writeError(writer, 409, "owned_session", err.Error())
					return
				}
				defer release()
			}
		}
		mux.ServeHTTP(writer, request)
	})
}

func (s *Server) statusHandler(writer http.ResponseWriter, _ *http.Request) {
	s.metadataMu.RLock()
	metadata := s.metadata
	s.metadataMu.RUnlock()
	writeJSON(writer, http.StatusOK, metadata)
}

func (s *Server) capabilitiesHandler(writer http.ResponseWriter, _ *http.Request) {
	manifest, err := adaptive.BuildCapabilityManifest(s.blueprint, s.metadata.EnvironmentDigest, s.metadata.AppDigest)
	if err != nil || s.metadata.CapabilityManifestDigest != "" && manifest.Digest != s.metadata.CapabilityManifestDigest {
		writeError(writer, 500, "capability_manifest_failed", "sealed capability binding is inconsistent")
		return
	}
	writeJSON(writer, 200, manifest)
}
func (s *Server) registerArtifactHandler(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Descriptor adaptive.ArtifactDescriptor `json:"descriptor"`
		Content    []byte                      `json:"content"`
	}
	if err := decodeStrict(request, &input, adaptive.ArtifactEnvelopeLimit(s.blueprint.Limits.MaxBlobBytes)); err != nil {
		writeError(writer, 400, "invalid_artifact", err.Error())
		return
	}
	_, alreadyRegistered := s.artifacts.Get(input.Descriptor.Digest)
	item, err := s.artifacts.Register(request.Context(), input.Descriptor, bytes.NewReader(input.Content))
	if err != nil {
		writeError(writer, 400, "invalid_artifact", err.Error())
		return
	}
	if alreadyRegistered {
		writeJSON(writer, 200, item)
		return
	}
	encoded, _ := json.Marshal(item)
	if _, err := s.journal.Append("artifact_register", []statejournal.Operation{{Kind: "artifact_put", ID: item.Digest, Value: encoded}}); err != nil {
		if !alreadyRegistered {
			_ = s.artifacts.Rollback(item)
		}
		writeError(writer, 500, "artifact_failed", "artifact registration could not be journaled")
		return
	}
	s.bumpRevision()
	_, _ = s.recorder.Append(recording.Draft{Channel: recording.Control, Layer: "artifact", Kind: "artifact_registered", Provenance: recording.HarnessInjected, PayloadDigest: item.Digest, Semantic: map[string]any{"digest": item.Digest, "size_bytes": item.SizeBytes, "media_type": item.MediaType, "canonicalization": item.Canonicalization}})
	writeJSON(writer, 201, item)
}
func (s *Server) readArtifactHandler(writer http.ResponseWriter, request *http.Request) {
	digestValue := request.PathValue("digest")
	item, ok := s.artifacts.Get(digestValue)
	if !ok {
		s.turnMu.RLock()
		for _, turn := range s.turns {
			if turn.OutputDigest == digestValue {
				item = adaptive.ArtifactDescriptor{Digest: digestValue, SizeBytes: int64(len(turn.Body)), MediaType: turn.MediaType, Canonicalization: "raw"}
				if item.MediaType == "" {
					item.MediaType = "application/octet-stream"
				}
				ok = true
				break
			}
		}
		s.turnMu.RUnlock()
	}
	if !ok {
		writeError(writer, 404, "not_found", "artifact is not registered or a current target output")
		return
	}
	reader, _, err := s.blobs.Open(item.Digest)
	if err != nil {
		writeError(writer, 500, "artifact_failed", "artifact verification failed")
		return
	}
	defer reader.Close()
	writer.Header().Set("Content-Type", item.MediaType)
	writer.Header().Set("X-Content-Digest", item.Digest)
	writer.WriteHeader(200)
	_, _ = io.Copy(writer, reader)
}
func (s *Server) listArtifactsHandler(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, 200, map[string]any{"artifacts": s.artifacts.List()})
}
func (s *Server) registerAttemptHandler(writer http.ResponseWriter, request *http.Request) {
	var input adaptive.AttemptContext
	if err := decodeStrict(request, &input, 1<<20); err != nil {
		writeError(writer, 400, "invalid_attempt", err.Error())
		return
	}
	item, err := s.attempts.Register(input)
	if err != nil {
		writeError(writer, 400, "invalid_attempt", err.Error())
		return
	}
	encoded, _ := json.Marshal(item)
	if _, err := s.journal.Append("attempt_register", []statejournal.Operation{{Kind: "attempt_put", ID: item.AttemptID, Value: encoded}}); err != nil {
		_ = s.attempts.Rollback(item)
		writeError(writer, 500, "attempt_failed", "attempt registration could not be journaled")
		return
	}
	s.bumpRevision()
	_, _ = s.recorder.Append(recording.Draft{Channel: recording.Control, Layer: "attempt", Kind: "attempt_registered", Provenance: recording.HarnessInjected, AttemptID: item.AttemptID, PayloadDigest: item.Payload.Digest, Semantic: map[string]any{"campaign_id": item.CampaignID, "thread_id": item.ThreadID, "parent_attempt_id": item.ParentAttemptID, "generation": item.Generation, "attempt_index": item.AttemptIndex, "context_digest": item.Digest}})
	writeJSON(writer, 201, item)
}
func (s *Server) listAttemptsHandler(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, 200, map[string]any{"attempts": s.attempts.List()})
}
func (s *Server) bumpRevision() {
	s.metadataMu.Lock()
	s.metadata.Revision++
	_ = session.Save(s.paths.Metadata, s.metadata)
	s.metadataMu.Unlock()
}

func (s *Server) turnHandler(writer http.ResponseWriter, request *http.Request) {
	if a, ok := s.adapter.(*protocolApplicationAdapter); ok && a.stateEnabled() {
		ctx, cancel := context.WithTimeout(request.Context(), s.turnTimeout())
		defer cancel()
		release, err := a.lockState(ctx)
		if err != nil {
			writeError(writer, http.StatusRequestTimeout, "application_busy", err.Error())
			return
		}
		defer release()
		request = request.WithContext(context.WithValue(ctx, stateInvocationKey{}, a))
	}
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
	if err := decodeStrict(request, &input, 3<<20); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_turn", err.Error())
		return
	}
	if input.Input != nil && input.Body != nil {
		writeError(writer, http.StatusBadRequest, "invalid_turn", "input and body may not both be present")
		return
	}
	if len(input.Input) > 2<<20 || len(input.Body) > 2<<20 {
		writeError(writer, http.StatusRequestEntityTooLarge, "invalid_turn", "decoded turn input exceeds 2 MiB")
		return
	}
	if input.AttemptID != "" {
		attempt, ok := s.attempts.Get(input.AttemptID)
		if !ok || attempt.Payload.Digest != input.PayloadDigest {
			writeError(writer, 400, "invalid_attempt", "turn attempt and payload are not registered")
			return
		}
		if input.ArtifactDigest != "" && input.ArtifactDigest != input.PayloadDigest {
			writeError(writer, 400, "invalid_artifact", "turn artifact does not match attempt payload")
			return
		}
	}
	if input.ArtifactDigest != "" {
		if input.Input != nil || input.Body != nil {
			writeError(writer, 400, "invalid_artifact", "artifact_digest may not be combined with inline input")
			return
		}
		reader, _, err := s.blobs.Open(input.ArtifactDigest)
		if err != nil {
			writeError(writer, 400, "invalid_artifact", "registered artifact is unavailable")
			return
		}
		content, err := io.ReadAll(io.LimitReader(reader, 2<<20+1))
		reader.Close()
		if err != nil || len(content) > 2<<20 {
			writeError(writer, 400, "invalid_artifact", "artifact cannot be used as turn input")
			return
		}
		input.Input = content
	}
	if input.Operation != "" && (input.Method != "" || input.Path != "" || input.Body != nil) {
		writeError(writer, http.StatusBadRequest, "invalid_turn", "operation may not be combined with method, path, or body")
		return
	}
	if input.Operation == "" && (input.Input != nil || input.MediaType != "") {
		writeError(writer, http.StatusBadRequest, "invalid_turn", "input and media_type require operation")
		return
	}
	input.Method = strings.ToUpper(input.Method)
	if input.Operation == "" && s.adapter.Kind() != "http" {
		writeError(writer, http.StatusBadRequest, "operation_required", "turn requires an operation for this application interface")
		return
	}
	if input.Operation != "" {
		if _, ok := s.operation(input.Operation); !ok {
			writeError(writer, http.StatusBadRequest, "unsupported_operation", "turn operation is not declared")
			return
		}
	} else if !s.servedRoute(input.Method, input.Path) {
		writeError(writer, http.StatusBadRequest, "unsupported_route", "turn route is not declared")
		return
	}
	deliveryBody := input.Input
	if deliveryBody == nil {
		deliveryBody = input.Body
	}
	operationID := input.Operation
	if operationID == "" {
		if operation, ok := s.routeOperation(input.Method, input.Path); ok {
			operationID = operation.ID
		}
	}
	deliveryMediaType := input.MediaType
	if input.Operation == "" {
		foundMedia := false
		for name, value := range input.Headers {
			if strings.EqualFold(name, "Content-Type") {
				if foundMedia {
					writeError(writer, http.StatusBadRequest, "invalid_delivery_input", "multiple Content-Type fields")
					return
				}
				foundMedia = true
				deliveryMediaType = value
			}
		}
	}
	if err := s.validateDeliveryInput(operationID, deliveryMediaType, deliveryBody); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_delivery_input", err.Error())
		return
	}
	id := fmt.Sprintf("turn-%d", s.nextTurn.Add(1))
	identityToken, err := s.issueInboundCaller(input.CallerPrincipal, id)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_caller_identity", err.Error())
		return
	}
	if !s.reserveTurn() {
		writeError(writer, http.StatusTooManyRequests, "turn_limit", "turn limit is exhausted")
		return
	}
	turn := Turn{ID: id, Operation: input.Operation, Status: "running", Started: time.Now().UTC(), AttemptID: input.AttemptID, PayloadDigest: input.PayloadDigest}
	s.turnMu.Lock()
	s.turns[id] = turn
	s.turnMu.Unlock()
	s.active.Add(1)
	defer s.active.Add(-1)
	turnContext, cancel := context.WithTimeout(request.Context(), time.Duration(s.blueprint.Application.Interface.TurnTimeoutMS)*time.Millisecond)
	defer cancel()
	body := input.Input
	if body == nil {
		body = input.Body
	}
	result, err := s.adapter.Invoke(turnContext, applicationInvocation{TurnID: id, Operation: input.Operation, Method: input.Method, Path: input.Path, Headers: input.Headers, Body: body, MediaType: input.MediaType, IdentityToken: identityToken})
	turn.Code, turn.Headers, turn.Body, turn.MediaType, turn.ExitCode = result.Code, result.Headers, result.Body, result.MediaType, result.ExitCode
	turn.OutputContractStatus = s.outputContractStatus(operationID, result.MediaType, result.Body)
	turn.Finished = time.Now().UTC()
	if err != nil {
		turn.Status, turn.Error = "error", err.Error()
	} else {
		turn.Status = "complete"
	}
	if len(turn.Body) > 0 {
		if object, storeErr := s.blobs.Put(request.Context(), bytes.NewReader(turn.Body)); storeErr == nil {
			turn.OutputDigest = object.Digest
		}
	}
	s.turnMu.Lock()
	s.turns[id] = turn
	s.turnMu.Unlock()
	s.bumpRevision()
	inlineBody := ""
	if len(turn.Body) <= 64<<10 {
		inlineBody = string(turn.Body)
	}
	semantic := map[string]any{"status": turn.Status, "code": turn.Code, "body": inlineBody, "output_ref": turn.OutputDigest, "error": turn.Error}
	if turn.OutputContractStatus != "" {
		semantic["output_contract_status"] = turn.OutputContractStatus
	}
	_, _ = s.recorder.Append(recording.Draft{Channel: recording.Attack, Layer: "application_bridge", Kind: "turn_completed", Provenance: recording.Application, TurnID: id, AttemptID: turn.AttemptID, PayloadDigest: turn.PayloadDigest, RawRef: turn.OutputDigest, Semantic: semantic})
	_ = s.recorder.Flush()
	writeJSON(writer, http.StatusOK, turn)
}

func (s *Server) issueInboundCaller(principalID, turnID string) (string, error) {
	if principalID == "" {
		return "", nil
	}
	configured := s.blueprint.Identity.Inbound
	if configured == nil {
		return "", errors.New("the Environment Blueprint does not configure inbound caller identity")
	}
	authority := s.identities[configured.Issuer]
	if authority == nil {
		return "", errors.New("the configured inbound identity issuer is unavailable")
	}
	token, _, err := authority.IssueCallerToken(principalID, configured.Audience, nil)
	if err != nil {
		return "", err
	}
	_, _ = s.recorder.Append(recording.Draft{
		Channel: recording.Attack, Layer: "identity", Kind: "caller_identity_attached", Provenance: recording.HarnessInjected,
		TurnID: turnID, Semantic: map[string]any{"principal": principalID, "audience": configured.Audience},
	})
	return token, nil
}

func (s *Server) callerTokenHandler(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Issuer    string   `json:"issuer,omitempty"`
		Principal string   `json:"principal"`
		Audience  string   `json:"audience,omitempty"`
		Scopes    []string `json:"scopes,omitempty"`
	}
	if err := decodeStrict(request, &input, 64<<10); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_identity_request", err.Error())
		return
	}
	if input.Issuer == "" && s.blueprint.Identity.Inbound != nil {
		input.Issuer = s.blueprint.Identity.Inbound.Issuer
	}
	if input.Audience == "" && s.blueprint.Identity.Inbound != nil && input.Issuer == s.blueprint.Identity.Inbound.Issuer {
		input.Audience = s.blueprint.Identity.Inbound.Audience
	}
	authority := s.identities[input.Issuer]
	if authority == nil {
		writeError(writer, http.StatusBadRequest, "invalid_identity_request", "identity issuer is not declared")
		return
	}
	token, claims, err := authority.IssueCallerToken(input.Principal, input.Audience, input.Scopes)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_identity_request", err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{
		"access_token": token, "token_type": "Bearer", "expires_at": claims.ExpiresAt,
		"subject": claims.Subject, "audience": claims.Audience, "scopes": claims.Scopes,
	})
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
	if input.Definition.AttemptID != "" {
		attempt, ok := s.attempts.Get(input.Definition.AttemptID)
		if !ok || attempt.Payload.Digest != input.Definition.PayloadDigest {
			writeError(writer, 400, "invalid_injection", "injection attempt and payload are not registered")
			return
		}
	}
	if err := s.validateInjectionTarget(input.Definition); err != nil {
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
	s.bumpRevision()
	writeJSON(writer, http.StatusCreated, injection.PublicState{Definition: input.Definition})
}

func (s *Server) fidelityMutation(ctx context.Context, definition injection.Definition) ([]statejournal.Operation, func() error, error) {
	if err := injection.Validate(definition); err != nil {
		return nil, nil, err
	}
	if definition.Surface != "environment_state" || !definition.Enabled {
		return nil, nil, nil
	}
	selector := definition.Selector
	if selector.Service != "" {
		handler := s.services[selector.Service]
		if handler == nil {
			return nil, nil, fmt.Errorf("fidelity target service %q does not expose managed state", selector.Service)
		}
		state := handler.Snapshot()
		current := state[selector.Collection][selector.Key]
		if len(current) == 0 {
			policy, ok := s.resourceCreationPolicy(selector.Service, selector.Collection)
			if !ok {
				return nil, nil, errors.New("fidelity target resource does not exist and its collection is not creation-enabled")
			}
			if definition.Placement.Operation != "replace" || definition.Placement.Pointer != "" {
				return nil, nil, errors.New("new resources require root replace placement")
			}
			if len(state[selector.Collection]) >= policy.MaxResources {
				return nil, nil, errors.New("fidelity target collection has reached its resource limit")
			}
			var value any
			decoder := json.NewDecoder(bytes.NewReader(definition.Payload))
			decoder.UseNumber()
			if err := decoder.Decode(&value); err != nil {
				return nil, nil, err
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, nil, err
			}
			if len(encoded) > 1<<20 {
				return nil, nil, errors.New("created service resource exceeds 1 MiB")
			}
			operation := statejournal.Operation{Kind: "service_put", Service: selector.Service, Collection: selector.Collection, Key: selector.Key, Value: encoded}
			creating, ok := handler.(services.CreatingManagedHandler)
			if !ok || !creating.SupportsCreate(selector.Collection) {
				return nil, nil, errors.New("fidelity target service does not expose a sealed create contract")
			}
			if err := creating.ValidateCreate(operation); err != nil {
				return nil, nil, err
			}
			return []statejournal.Operation{operation}, func() error { return handler.Apply(operation) }, nil
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
		if validating, ok := handler.(services.ValidatingManagedHandler); ok {
			if err := validating.ValidateApply(operation); err != nil {
				return nil, nil, err
			}
		}
		return []statejournal.Operation{operation}, func() error { return handler.Apply(operation) }, nil
	}
	containerPath, hostPath, namespace, ok := s.resolveInjectionFile(selector)
	if !ok {
		return nil, nil, errors.New("fidelity file target is outside its declared mutable namespace")
	}
	info, err := os.Lstat(hostPath)
	if os.IsNotExist(err) {
		if namespace == nil || !namespace.AllowCreate {
			return nil, nil, errors.New("fidelity file target does not exist and its namespace is not creation-enabled")
		}
		if definition.Placement.Operation != "replace" || definition.Placement.Pointer != "" {
			return nil, nil, errors.New("new files require root replace placement")
		}
		encoded, err := encodeInjectedFile(definition.Payload)
		if err != nil {
			return nil, nil, err
		}
		if int64(len(encoded)) > namespace.MaxFileBytes {
			return nil, nil, fmt.Errorf("created file exceeds namespace limit of %d bytes", namespace.MaxFileBytes)
		}
		if err := secureCreationParent(hostPath, namespace.Root); err != nil {
			return nil, nil, err
		}
		count, err := regularFileCount(namespace.Root)
		if err != nil {
			return nil, nil, err
		}
		if count >= namespace.MaxFiles {
			return nil, nil, errors.New("file injection namespace has reached its file limit")
		}
		object, err := s.blobs.Put(ctx, bytes.NewReader(encoded))
		if err != nil {
			return nil, nil, err
		}
		operation := statejournal.Operation{Kind: "file_upsert", Path: containerPath, Mode: 0o640, Blob: object.Digest, Size: object.Size}
		return []statejournal.Operation{operation}, func() error { return fsutil.WriteFile(hostPath, encoded, 0o640) }, nil
	}
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
	operation := statejournal.Operation{Kind: "file_upsert", Path: containerPath, Mode: uint32(info.Mode().Perm()), Blob: object.Digest, Size: object.Size}
	return []statejournal.Operation{operation}, func() error {
		if err := fsutil.WriteFile(hostPath, encoded, info.Mode().Perm()); err != nil {
			return err
		}
		return nil
	}, nil
}

func (s *Server) validateInjectionTarget(definition injection.Definition) error {
	if err := injection.Validate(definition); err != nil {
		return err
	}
	switch definition.Surface {
	case "model_tool_result":
		return nil
	case "service_response":
		for _, service := range s.blueprint.Services {
			if service.ID == definition.Selector.Service {
				return nil
			}
		}
		for _, service := range s.blueprint.AttackerServices {
			if service.ID == definition.Selector.Service {
				if service.Response.Mode != "dynamic" {
					return errors.New("attacker service response is static")
				}
				return nil
			}
		}
		return errors.New("service response target is not declared")
	case "mcp_tool_result":
		for _, service := range s.blueprint.Services {
			if service.ID == definition.Selector.Service && service.Kind == "mcp_http" {
				return nil
			}
		}
		return errors.New("MCP result target is not a declared MCP service")
	case "environment_state":
		if definition.Selector.Service != "" && s.services[definition.Selector.Service] == nil {
			return errors.New("environment state target is not a managed service")
		}
		if definition.Selector.FileNamespace != "" {
			if _, ok := s.fileInjectionNamespace(definition.Selector.FileNamespace); !ok {
				return errors.New("file injection namespace is not declared")
			}
		}
		return nil
	default:
		return errors.New("unsupported injection surface")
	}
}

func (s *Server) resourceCreationPolicy(serviceID, collection string) (environment.ResourceCreationPolicy, bool) {
	for _, service := range s.blueprint.Services {
		if service.ID != serviceID {
			continue
		}
		for _, policy := range service.Injections.CreateCollections {
			if policy.Collection == collection {
				return policy, true
			}
		}
	}
	return environment.ResourceCreationPolicy{}, false
}

func (s *Server) fileInjectionNamespace(id string) (*environment.FileInjectionNamespace, bool) {
	for index := range s.blueprint.Filesystem.InjectionNamespaces {
		if s.blueprint.Filesystem.InjectionNamespaces[index].ID == id {
			return &s.blueprint.Filesystem.InjectionNamespaces[index], true
		}
	}
	return nil, false
}

func (s *Server) resolveInjectionFile(selector injection.Selector) (string, string, *environment.FileInjectionNamespace, bool) {
	containerPath := selector.Path
	var namespace *environment.FileInjectionNamespace
	if selector.FileNamespace != "" {
		var exists bool
		namespace, exists = s.fileInjectionNamespace(selector.FileNamespace)
		if !exists {
			return "", "", nil, false
		}
		containerPath = pathpkg.Join(namespace.Root, selector.RelativePath)
	}
	if code := s.blueprint.Application.Interface.Code; code != nil && code.State != nil {
		clean := pathpkg.Clean(containerPath)
		state := code.State.Path
		if clean == state || strings.HasPrefix(state, clean+"/") || strings.HasPrefix(clean, state+"/") {
			return "", "", nil, false
		}
	}
	hostPath, ok := resolveMutablePath(s.containerRoots(), containerPath)
	return containerPath, hostPath, namespace, ok
}

func encodeInjectedFile(payload json.RawMessage) ([]byte, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if text, ok := value.(string); ok {
		return []byte(text), nil
	}
	return json.Marshal(value)
}

func secureCreationParent(target, namespaceRoot string) error {
	root, err := filepath.EvalSymlinks(namespaceRoot)
	if err != nil {
		return fmt.Errorf("resolve file injection namespace: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return errors.New("new file parent must be an existing real directory")
	}
	relative, err := filepath.Rel(root, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("new file parent escapes its injection namespace")
	}
	return nil
}

func regularFileCount(root string) (int, error) {
	count := 0
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type().IsRegular() {
			count++
		}
		return nil
	})
	return count, err
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
		if errors.Is(err, injection.ErrNotFound) {
			writeError(writer, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(writer, http.StatusInternalServerError, "injection_delete_failed", "could not durably delete injection")
		}
		return
	}
	s.bumpRevision()
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) createSnapshotHandler(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Label                 string                 `json:"label,omitempty"`
		CampaignID            string                 `json:"campaign_id,omitempty"`
		Description           string                 `json:"description,omitempty"`
		MaximumCommittedBytes snapshot.ByteAllowance `json:"maximum_committed_bytes,omitzero"`
	}
	if err := decodeStrict(request, &input, 64<<10); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_snapshot", err.Error())
		return
	}
	if s.execution != nil {
		if association := s.execution.State().Owner; association != nil {
			if input.CampaignID != "" && input.CampaignID != association.CampaignID {
				writeError(writer, 403, "campaign_mismatch", "snapshot campaign differs from session")
				return
			}
			input.CampaignID = association.CampaignID
		}
	}
	if err := snapshot.ValidateMetadata(input.CampaignID, input.Description); err != nil {
		writeError(writer, 400, "invalid_snapshot", err.Error())
		return
	}
	checkpoint, err := s.createSnapshot(request.Context(), input.Label, input.MaximumCommittedBytes, snapshot.Metadata{CampaignID: input.CampaignID, Description: input.Description})
	if err != nil {
		if errors.Is(err, snapshot.ErrBytesExhausted) {
			writeError(writer, http.StatusTooManyRequests, "snapshot_bytes_exhausted", err.Error())
		} else {
			writeError(writer, http.StatusConflict, "snapshot_failed", err.Error())
		}
		return
	}
	writeJSON(writer, http.StatusCreated, checkpoint)
}

func (s *Server) createSnapshot(ctx context.Context, label string, maximumCommittedBytes snapshot.ByteAllowance, checkpointMetadata snapshot.Metadata) (snapshot.Checkpoint, error) {
	stateAdapter, hasState := s.adapter.(*protocolApplicationAdapter)
	hasState = hasState && stateAdapter.stateEnabled()
	if hasState {
		timeout := time.Duration(s.blueprint.Application.Persistence.Quiesce.TimeoutMS) * time.Millisecond
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	// Drain invocations before taking the service/activity lock: a running
	// turn may still need to call a managed service or the model proxy.
	if hasState {
		release, err := stateAdapter.lockState(ctx)
		if err != nil {
			return snapshot.Checkpoint{}, err
		}
		defer release()
	}
	if err := lockContext(ctx, s.controlMu.TryLock); err != nil {
		return snapshot.Checkpoint{}, err
	}
	defer s.controlMu.Unlock()
	if err := lockContext(ctx, s.activityMu.TryLock); err != nil {
		return snapshot.Checkpoint{}, err
	}
	defer s.activityMu.Unlock()
	if !s.blueprint.Application.Persistence.SnapshotCapable {
		return snapshot.Checkpoint{}, errors.New("application is not snapshot capable")
	}
	s.metadataMu.RLock()
	snapshotAttempts, maxSnapshots := s.metadata.Consumed.SnapshotAttempts, s.metadata.Limits.MaxSnapshots
	maxBlob, usedBytes := s.metadata.Limits.MaxBlobBytes, s.metadata.Consumed.SnapshotBytes
	s.metadataMu.RUnlock()
	if snapshotAttempts >= maxSnapshots {
		return snapshot.Checkpoint{}, errors.New("snapshot limit is exhausted")
	}
	allowance, err := snapshot.RemainingBytes(maxBlob, maxSnapshots, usedBytes, maximumCommittedBytes)
	if err != nil {
		return snapshot.Checkpoint{}, err
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
	if hasState {
		if err := stateAdapter.saveState(ctx); err != nil {
			return snapshot.Checkpoint{}, err
		}
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
	canonicalSize := int64(0)
	for _, entry := range current.Entries {
		if entry.Type == "file" {
			if entry.Size < 0 || entry.Size > allowance-canonicalSize {
				return snapshot.Checkpoint{}, snapshot.ErrBytesExhausted
			}
			canonicalSize += entry.Size
		}
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
	s.metadata.Consumed.SnapshotBytes += canonicalSize
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
	checkpoint, err := snapshot.NewCheckpoint(metadataForCheckpoint.ID, metadataForCheckpoint.ParentCheckpoint, metadataForCheckpoint.EnvironmentDigest, metadataForCheckpoint.AppDigest, journalSeq, journalHash, current.Digest, canonicalSize, eventSeq, label, buildinfo.Version, time.Now(), checkpointMetadata)
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
	s.bumpRevision()
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
	s.bumpRevision()
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
		if s.execution != nil {
			result = errors.Join(result, s.execution.Close("session_stopped"))
		}
		close(s.identityStop)
		if s.dnsServer != nil {
			result = errors.Join(result, s.dnsServer.Close())
		}
		for _, server := range s.servers {
			result = errors.Join(result, server.Shutdown(ctx))
		}
		for _, listener := range s.listeners {
			_ = listener.Close()
		}
		if s.helperIn != nil {
			_ = s.helperIn.Close()
			s.helperIn = nil
		}
		if adapter, ok := s.adapter.(*protocolApplicationAdapter); ok {
			result = errors.Join(result, adapter.Close(ctx))
		}
		if s.app != nil && s.app.Process != nil {
			_ = syscall.Kill(-s.app.Process.Pid, syscall.SIGTERM)
		}
		if s.helper != nil && s.helper.Process != nil {
			_ = syscall.Kill(-s.helper.Process.Pid, syscall.SIGTERM)
		}
		for _, path := range s.identityFiles {
			_ = os.Remove(path)
		}
		if len(s.identityFiles) > 0 {
			_ = os.Remove(filepath.Dir(s.identityFiles[0]))
		}
		if report, err := s.evaluate(); err != nil {
			result = errors.Join(result, fmt.Errorf("persist final evaluation: %w", err))
		} else if err := fsutil.WriteJSON(s.paths.Evaluation, report, 0o640); err != nil {
			result = errors.Join(result, fmt.Errorf("persist final evaluation: %w", err))
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

func (s *Server) serveTLS(address string, handler http.Handler) error {
	certificate, err := tls.LoadX509KeyPair(appimage.ContainerTLSCertificate, appimage.ContainerTLSPrivateKey)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}}
	s.listeners = append(s.listeners, listener)
	s.servers = append(s.servers, server)
	go server.ServeTLS(listener, "", "")
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

func (s *Server) reserveTurn() bool {
	s.metadataMu.Lock()
	defer s.metadataMu.Unlock()
	if s.metadata.Consumed.Turns >= s.metadata.Limits.MaxTurns {
		return false
	}
	s.metadata.Consumed.Turns++
	_ = session.Save(s.paths.Metadata, s.metadata)
	return true
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

func (s *Server) startApplicationHelper(asApplication bool) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	helper := exec.Command(executable, "app-helper")
	s.configureApplicationCommand(helper, nil)
	s.helperIn, err = helper.StdinPipe()
	if err != nil {
		return err
	}
	helperOutput, err := helper.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := os.OpenFile(filepath.Join(s.paths.Logs, "application.stderr.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	helper.Stderr = stderr
	s.helperOut = bufio.NewReaderSize(helperOutput, 16<<20)
	s.helperPending = map[string]chan HelperResponse{}
	oldUmask := syscall.Umask(0o007)
	err = helper.Start()
	syscall.Umask(oldUmask)
	if err != nil {
		_ = stderr.Close()
		return fmt.Errorf("start application helper: %w", err)
	}
	s.helper = helper
	go s.readHelperResponses()
	if asApplication {
		s.app = helper
		go func() {
			err := helper.Wait()
			_ = stderr.Close()
			select {
			case s.done <- err:
			default:
			}
		}()
	} else {
		go func() {
			_ = helper.Wait()
			_ = stderr.Close()
		}()
	}
	return nil
}

func (s *Server) readHelperResponses() {
	for {
		line, err := s.helperOut.ReadBytes('\n')
		if err != nil {
			s.helperPendingMu.Lock()
			for _, channel := range s.helperPending {
				select {
				case channel <- HelperResponse{Error: err.Error()}:
				default:
				}
			}
			s.helperPendingMu.Unlock()
			return
		}
		var response HelperResponse
		if json.Unmarshal(line, &response) != nil {
			continue
		}
		s.helperPendingMu.Lock()
		channel := s.helperPending[response.ID]
		s.helperPendingMu.Unlock()
		if channel != nil {
			select {
			case channel <- response:
			default:
			}
		}
	}
}

func (s *Server) callHelper(ctx context.Context, request HelperRequest) (HelperResponse, error) {
	if s.helper == nil || s.helperIn == nil || s.helperOut == nil {
		return HelperResponse{}, errors.New("application command helper is unavailable")
	}
	request.ID = fmt.Sprintf("helper-%d", s.nextHelper.Add(1))
	request.Type = "invoke"
	channel := make(chan HelperResponse, 1)
	s.helperPendingMu.Lock()
	s.helperPending[request.ID] = channel
	s.helperPendingMu.Unlock()
	defer func() {
		s.helperPendingMu.Lock()
		delete(s.helperPending, request.ID)
		s.helperPendingMu.Unlock()
	}()
	encoded, _ := json.Marshal(request)
	s.helperMu.Lock()
	if _, err := s.helperIn.Write(append(encoded, '\n')); err != nil {
		s.helperMu.Unlock()
		return HelperResponse{}, err
	}
	s.helperMu.Unlock()
	select {
	case <-ctx.Done():
		cancel, _ := json.Marshal(HelperRequest{ID: request.ID, Type: "cancel"})
		s.helperMu.Lock()
		_, _ = s.helperIn.Write(append(cancel, '\n'))
		s.helperMu.Unlock()
		select {
		case <-channel:
		case <-time.After(5 * time.Second):
		}
		return HelperResponse{}, ctx.Err()
	case response := <-channel:
		if response.ID == "" && response.Error != "" {
			return HelperResponse{}, errors.New(response.Error)
		}
		return response, nil
	}
}

func (s *Server) runHelperCommand(ctx context.Context, command []string, timeout time.Duration) error {
	response, err := s.callHelper(ctx, HelperRequest{Command: command, TimeoutMS: timeout.Milliseconds()})
	if err != nil {
		return err
	}
	s.appendApplicationLogs(response.Stdout, response.Stderr)
	if !response.OK {
		return fmt.Errorf("quiesce command failed: %s", response.Error)
	}
	return nil
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
	_, ok := s.routeOperation(method, path)
	return ok
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
