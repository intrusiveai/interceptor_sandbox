package sandboxd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/gatewayhost"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

type operationTestAdapter struct{ calls atomic.Int32 }

func (*operationTestAdapter) Start() error                    { return nil }
func (*operationTestAdapter) WaitReady(context.Context) error { return nil }
func (*operationTestAdapter) Handler() http.Handler           { return nil }
func (*operationTestAdapter) Kind() string                    { return "command" }
func (a *operationTestAdapter) Invoke(context.Context, applicationInvocation) (applicationResult, error) {
	a.calls.Add(1)
	return applicationResult{Code: 200, Body: []byte("response"), MediaType: "text/plain"}, nil
}

func nativeOperationServer(t *testing.T, transports ...string) (*Server, http.Handler) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "ix-ops-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("INTERCEPTOR_DATA_DIR", root)
	metadata := session.Metadata{SchemaVersion: 1, ID: "sess-1-000000000000000000000000", OperationAPIVersion: adaptive.OperationRequestVersion, Phase: session.Running, Revision: 1, BridgeTransport: "unix", ContainerID: "test-target", FeedbackProfile: "black-box", Limits: session.Limits{MaxBlobBytes: 1 << 20, MaxSnapshots: 2, MaxTurns: 10, MaxWallSeconds: 60}}
	if len(transports) > 0 {
		metadata.BridgeTransport = transports[0]
	}
	if len(transports) > 1 {
		metadata.ID = transports[1]
	}
	paths, recorder, journal, err := session.Initialize(root, metadata, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close(); _ = journal.Close() })
	blobs, err := blobstore.Open(paths.Blobs, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := adaptive.OpenArtifactRegistry(filepath.Join(paths.Root, "artifacts.json"), blobs, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := adaptive.OpenAttemptRegistry(filepath.Join(paths.Root, "attempts.json"), "black-box", 2, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := adaptive.OpenExecutionStore(paths.Root, metadata.ID, journal)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{metadata: metadata, paths: paths, recorder: recorder, journal: journal, blobs: blobs, artifacts: artifacts, attempts: attempts, execution: execution, injections: injection.NewManager(journal, recorder), turns: map[string]Turn{}, adapter: &operationTestAdapter{}, done: make(chan error, 1), blueprint: &environment.Blueprint{Limits: environment.Limits{MaxBlobBytes: 1 << 20}, Application: environment.Application{Interface: environment.ApplicationInterface{Adapter: "command", TurnTimeoutMS: 1000, Operations: []environment.ApplicationOperation{{ID: "invoke"}}}}, AttackerServices: []environment.AttackerService{{ID: "c2", Kind: "http_sink", Response: environment.SinkResponse{Mode: "dynamic"}}}}}
	handler := s.controlHandler()
	if metadata.BridgeTransport == "spool" {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- bridge.ServeHTTPSpool(ctx, bridge.HTTPSpoolServerConfig{Root: filepath.Join(paths.Sockets, "http-spool"), Application: http.NotFoundHandler(), Control: handler, MaxBytes: adaptive.OperationFrameLimit(metadata.Limits.MaxBlobBytes), ControlClass: operationControlClass})
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(2 * time.Second):
				t.Error("spool failed to stop")
			}
		})
		return s, handler
	}
	listener, err := net.Listen("unix", filepath.Join(paths.Sockets, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return s, handler
}
func nativeGateway(t *testing.T, s *Server) *adaptive.Gateway {
	t.Helper()
	binding := digest.Bytes([]byte("binding"))
	g, err := adaptive.NewGateway(adaptive.GatewayConfig{SessionID: s.metadata.ID, EnvironmentDigest: binding, ApplicationDigest: binding, CapabilityManifestDigest: binding, FeedbackProfile: "black-box", IntegrationPrincipal: "operator", IntegrationToken: []byte("long-enough-secret-integration-token"), Executor: gatewayhost.NewExecutor()})
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func nativeCall(t *testing.T, g http.Handler, s *Server, id, op string, body any, attempt *adaptive.AttemptContext) adaptive.GatewayResponse {
	return nativeCallAs(t, g, s, id, op, body, attempt, "campaign", "worker", 1)
}
func nativeCallAs(t *testing.T, g http.Handler, s *Server, id, op string, body any, attempt *adaptive.AttemptContext, campaign, worker string, revision uint64) adaptive.GatewayResponse {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := adaptive.OperationRequest{APIVersion: adaptive.OperationRequestVersion, SessionID: s.metadata.ID, RequestID: "req-" + id, OperationID: id, Operation: op, CampaignID: campaign, WorkerInstanceID: worker, RunRevision: revision, ExpectedSessionRevision: s.operationRevision(), Deadline: time.Now().Add(time.Minute), BodyDigest: digest.Bytes(raw)}
	if attempt != nil {
		r.AttemptID = attempt.AttemptID
		r.AttemptContextDigest = adaptive.DigestAttemptContext(*attempt)
	}
	request := httptest.NewRequest("POST", "/v1/operations", bytes.NewReader(adaptive.EncodeOperation(r, raw)))
	request.Header.Set("Authorization", "Bearer long-enough-secret-integration-token")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, request)
	var result adaptive.GatewayResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("gateway %d %s: %v", w.Code, w.Body, err)
	}
	return result
}
func TestNativeOperationsOwnershipCleanupAndReplacement(t *testing.T) {
	s, control := nativeOperationServer(t)
	s.metadata.Limits.MaxTurns = 1
	g := nativeGateway(t, s)
	owner := adaptive.OwnerRequest{Action: "bind", SessionID: s.metadata.ID, CampaignID: "campaign", WorkerInstanceID: "worker"}
	if result := nativeCall(t, g, s, "bind", "session.owner", owner, nil); result.Status != 200 {
		t.Fatalf("bind: %+v", result)
	}
	w := httptest.NewRecorder()
	control.ServeHTTP(w, httptest.NewRequest("POST", "/v1/turns", strings.NewReader(`{}`)))
	if w.Code != 409 {
		t.Fatal("local control bypassed owner")
	}
	payload := []byte(`"payload"`)
	descriptor := adaptive.ArtifactDescriptor{Digest: digest.Bytes(payload), SizeBytes: int64(len(payload)), MediaType: "application/json", Canonicalization: "raw"}
	if result := nativeCall(t, g, s, "artifact", "artifact.register", artifactRegistration{Descriptor: descriptor, Content: payload}, nil); result.Status != 201 {
		t.Fatalf("artifact: %+v", result)
	}
	attempt := adaptive.AttemptContext{APIVersion: adaptive.AttemptContextVersion, CampaignID: "campaign", ThreadID: "thread", AttemptID: "attempt", AttemptIndex: 1, Generation: 1, Payload: descriptor, Generator: adaptive.Generator{Kind: "custom-harness"}, FeedbackProfile: "black-box", CreatedAt: time.Now().UTC()}
	if result := nativeCall(t, g, s, "attempt", "attempt.register", attempt, &attempt); result.Status != 201 {
		t.Fatalf("attempt: %+v", result)
	}
	definition := injection.Definition{ID: "owned", Surface: "service_response", Mode: "simulation", Scope: "standing", Enabled: true, Selector: injection.Selector{Service: "c2", Path: "/collect", CallOrdinal: "first"}, Placement: injection.Placement{Operation: "replace"}, Payload: json.RawMessage(payload), AttemptID: attempt.AttemptID, PayloadDigest: descriptor.Digest}
	if result := nativeCall(t, g, s, "arm", "injection.arm", injectionArm{Definition: definition}, &attempt); result.Status != 201 {
		t.Fatalf("arm: %+v", result)
	}
	invocation := TurnRequest{Operation: "invoke", AttemptID: attempt.AttemptID, PayloadDigest: descriptor.Digest, Input: payload, MediaType: "application/json"}
	result := nativeCall(t, g, s, "invoke", "application.invoke", invocation, &attempt)
	if result.Status != 200 {
		t.Fatalf("invoke: %+v", result)
	}
	var turn Turn
	if err := json.Unmarshal(result.Body, &turn); err != nil || turn.Status != "complete" {
		t.Fatalf("turn: %s %v", result.Body, err)
	}
	replacement := nativeGateway(t, s)
	if result := nativeCall(t, replacement, s, "budget", "application.invoke", invocation, &attempt); result.Status != 429 {
		t.Fatalf("gateway replacement reset native turn budget: %+v", result)
	}
	if result := nativeCall(t, replacement, s, "lookup", "operation.status", operationLookup{OperationID: "invoke"}, nil); result.Status != 200 || !strings.Contains(string(result.Body), `"state":"completed"`) {
		t.Fatalf("record lookup: %+v", result)
	}
	if result := nativeCall(t, replacement, s, "receipt", "operation.status", operationLookup{ReceiptID: turn.ID}, nil); result.Status != 200 {
		t.Fatalf("native receipt: %+v", result)
	}
	owner.Action = "close_execution"
	if result := nativeCall(t, replacement, s, "close", "session.owner", owner, nil); result.Status != 200 {
		t.Fatalf("close: %+v", result)
	}
	if result := nativeCall(t, g, s, "blocked", "application.invoke", invocation, &attempt); result.Status != 409 {
		t.Fatal("closed execution invoked")
	}
	if result := nativeCall(t, replacement, s, "wrong-cleanup", "injection.delete", injectionLookup{InjectionID: "not-owned"}, nil); result.Status != 404 {
		t.Fatal("missing injection did not report not found")
	}
	if result := nativeCall(t, replacement, s, "cleanup", "injection.delete", injectionLookup{InjectionID: "owned"}, nil); result.Status != 204 {
		t.Fatalf("owned cleanup: %+v", result)
	}
	if result := nativeCall(t, replacement, s, "stop", "session.stop", struct{}{}, nil); result.Status != 409 {
		t.Fatal("stop without opt-in accepted")
	}
	transactions, err := statejournal.VerifyFile(s.paths.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adaptive.VerifyExecutionJournal(s.paths.Root, transactions); err != nil {
		t.Fatal(err)
	}
	if s.adapter.(*operationTestAdapter).calls.Load() != 1 {
		t.Fatal("replacement duplicated invocation")
	}
}
func TestNativeBodyValidationAndStopOptIn(t *testing.T) {
	s, _ := nativeOperationServer(t)
	g := nativeGateway(t, s)
	owner := adaptive.OwnerRequest{Action: "bind", SessionID: s.metadata.ID, CampaignID: "campaign", WorkerInstanceID: "worker", AllowTargetStop: true}
	if result := nativeCall(t, g, s, "bind", "session.owner", owner, nil); result.Status != 200 {
		t.Fatal(result)
	}
	for i, body := range []any{map[string]any{"label": "safe", "maximum_committed_bytes": 999999}, map[string]any{"label": "safe", "unknown": true}} {
		id := []string{"budget", "unknown"}[i]
		if result := nativeCall(t, g, s, id, "snapshot.create", body, nil); result.Status != 409 {
			t.Fatalf("snapshot schema: %+v", result)
		}
	}
	if result := nativeCall(t, g, s, "stop", "session.stop", struct{}{}, nil); result.Status != 202 {
		t.Fatalf("opt-in stop: %+v", result)
	}
	if s.execution.State().Owner.ClosedAt == nil {
		t.Fatal("stop did not close execution")
	}
}

func TestSnapshotOperationCarriesCampaignAndDescription(t *testing.T) {
	s, _ := nativeOperationServer(t)
	r := adaptive.OperationRequest{CampaignID: "campaign", Operation: "snapshot.create"}
	body := json.RawMessage(`{"label":"baseline","description":"Before branch B — preserve this state"}`)
	if err := s.checkOperation(r, body, adaptive.ExecutionState{}); err != nil {
		t.Fatal(err)
	}
	called := false
	result := s.dispatchOperation(t.Context(), r, body, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		called = true
		var input struct {
			Label                 string `json:"label"`
			Description           string `json:"description"`
			CampaignID            string `json:"campaign_id"`
			MaximumCommittedBytes int64  `json:"maximum_committed_bytes"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input.CampaignID != "campaign" || input.Description != "Before branch B — preserve this state" || input.Label != "baseline" || input.MaximumCommittedBytes <= 0 {
			t.Fatalf("lost metadata: %+v", input)
		}
		writeJSON(w, 201, input)
	}))
	if !called || result.Status != 201 {
		t.Fatal(result)
	}
	for _, invalid := range []json.RawMessage{
		json.RawMessage(`{"campaign_id":"other"}`),
		json.RawMessage(`{"description":42}`),
		json.RawMessage(`{"description":"` + strings.Repeat("x", 4097) + `"}`),
	} {
		if err := s.checkOperation(r, invalid, adaptive.ExecutionState{}); err == nil {
			t.Fatalf("accepted invalid metadata: %.80s", invalid)
		}
	}
}
func TestEncodeOperationPreservesExactBodyThroughHTTP(t *testing.T) {
	// Whitespace inside the digest-covered body survives the gateway-host hop.
	s, _ := nativeOperationServer(t)
	g := nativeGateway(t, s)
	server := httptest.NewServer(g)
	defer server.Close()
	body := []byte(`{ "action": "bind", "session_id": "` + s.metadata.ID + `", "campaign_id": "campaign", "worker_instance_id": "worker" }`)
	r := adaptive.OperationRequest{APIVersion: adaptive.OperationRequestVersion, SessionID: s.metadata.ID, CampaignID: "campaign", WorkerInstanceID: "worker", RunRevision: 1, OperationID: "bind", RequestID: "bind", Operation: "session.owner", ExpectedSessionRevision: 1, BodyDigest: digest.Bytes(body), Deadline: time.Now().Add(time.Minute)}
	req, _ := http.NewRequest("POST", server.URL+"/v1/operations", bytes.NewReader(adaptive.EncodeOperation(r, body)))
	req.Header.Set("Authorization", "Bearer long-enough-secret-integration-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 {
		t.Fatalf("body changed in transport: %d %s", response.StatusCode, raw)
	}
}

type blockedOperationAdapter struct {
	operationTestAdapter
	started chan struct{}
	release chan struct{}
}

func (a *blockedOperationAdapter) Invoke(ctx context.Context, _ applicationInvocation) (applicationResult, error) {
	close(a.started)
	select {
	case <-a.release:
		return applicationResult{Code: 200, Body: []byte("done")}, nil
	case <-ctx.Done():
		return applicationResult{}, ctx.Err()
	}
}
func TestSpoolClosureAndStatusBypassLongInvocation(t *testing.T) {
	s, _ := nativeOperationServer(t, "spool")
	s.blueprint.Application.Interface.TurnTimeoutMS = 10000
	adapter := &blockedOperationAdapter{started: make(chan struct{}), release: make(chan struct{})}
	s.adapter = adapter
	t.Cleanup(func() {
		select {
		case <-adapter.release:
		default:
			close(adapter.release)
		}
	})
	g, replacement := nativeGateway(t, s), nativeGateway(t, s)
	owner := adaptive.OwnerRequest{Action: "bind", SessionID: s.metadata.ID, CampaignID: "campaign", WorkerInstanceID: "worker"}
	if result := nativeCall(t, g, s, "bind", "session.owner", owner, nil); result.Status != 200 {
		t.Fatal(result)
	}
	payload := []byte(`"hello"`)
	artifact := adaptive.ArtifactDescriptor{Digest: digest.Bytes(payload), SizeBytes: int64(len(payload)), MediaType: "application/json", Canonicalization: "raw"}
	if result := nativeCall(t, g, s, "payload", "artifact.register", artifactRegistration{Descriptor: artifact, Content: payload}, nil); result.Status != 201 {
		t.Fatal(result)
	}
	attempt := adaptive.AttemptContext{APIVersion: adaptive.AttemptContextVersion, CampaignID: "campaign", ThreadID: "thread", AttemptID: "attempt", AttemptIndex: 1, Generation: 1, Payload: artifact, Generator: adaptive.Generator{Kind: "custom-harness"}, FeedbackProfile: "black-box", CreatedAt: time.Now().UTC()}
	if result := nativeCall(t, g, s, "attempt", "attempt.register", attempt, &attempt); result.Status != 201 {
		t.Fatal(result)
	}
	done := make(chan adaptive.GatewayResponse, 1)
	go func() {
		done <- nativeCall(t, g, s, "active", "application.invoke", TurnRequest{Operation: "invoke", Input: payload, AttemptID: attempt.AttemptID, PayloadDigest: artifact.Digest}, &attempt)
	}()
	select {
	case <-adapter.started:
	case <-time.After(2 * time.Second):
		t.Fatal("invocation never started")
	}
	lookup := make(chan adaptive.GatewayResponse, 1)
	go func() {
		lookup <- nativeCall(t, replacement, s, "status", "operation.status", operationLookup{OperationID: "active"}, nil)
	}()
	select {
	case result := <-lookup:
		if result.Status != 200 || !strings.Contains(string(result.Body), `"state":"running"`) {
			t.Fatalf("lookup: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("spool serialized status behind invocation")
	}
	owner.Action = "close_execution"
	closed := make(chan adaptive.GatewayResponse, 1)
	go func() { closed <- nativeCall(t, replacement, s, "close", "session.owner", owner, nil) }()
	select {
	case result := <-closed:
		if result.Status != 200 {
			t.Fatal(result)
		}
	case <-time.After(time.Second):
		t.Fatal("spool serialized closure behind invocation")
	}
	close(adapter.release)
	if result := <-done; result.Status != 200 {
		t.Fatalf("admitted operation lost result: %+v", result)
	}
	if result := nativeCall(t, replacement, s, "post-close", "artifact.register", artifactRegistration{Descriptor: artifact, Content: payload}, nil); result.Status != 409 {
		t.Fatal("closed spool admitted mutation")
	}
}

func TestNativeExecutionStateTamperFailsJournalVerification(t *testing.T) {
	s, _ := nativeOperationServer(t, "spool")
	g := nativeGateway(t, s)
	owner := adaptive.OwnerRequest{Action: "bind", SessionID: s.metadata.ID, CampaignID: "campaign", WorkerInstanceID: "worker"}
	if result := nativeCall(t, g, s, "bind", "session.owner", owner, nil); result.Status != 200 {
		t.Fatal(result)
	}
	transactions, err := statejournal.VerifyFile(s.paths.Journal)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.paths.Root, "execution.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"allow_target_stop":false`), []byte(`"allow_target_stop":true`), 1)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := adaptive.VerifyExecutionJournal(s.paths.Root, transactions); err == nil {
		t.Fatal("changed owner policy passed journal verification")
	}
}
