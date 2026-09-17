package adaptive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
)

const testIntegrationToken = "this-is-a-long-integration-secret-token"

type sessionExecutor struct {
	store *ExecutionStore
	hooks ExecutionHooks
}

func (e *sessionExecutor) Execute(_ *http.Request, p string, r OperationRequest, b json.RawMessage) (GatewayResponse, error) {
	return e.store.Execute(p, r, b, e.hooks), nil
}
func executionFixture(t *testing.T) (*sessionExecutor, *atomic.Uint64) {
	t.Helper()
	root := t.TempDir()
	state := ExecutionState{APIVersion: ExecutionStateVersion, SessionID: "session-1", Records: map[string]*OperationRecord{}}
	if err := fsutil.WriteJSON(filepath.Join(root, "execution.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenExecutionStore(root, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	revision := &atomic.Uint64{}
	revision.Store(1)
	e := &sessionExecutor{store: store, hooks: ExecutionHooks{Revision: revision.Load, OwnerChanged: func(ExecutionOwner) error { revision.Add(1); return nil }, Check: func(OperationRequest, json.RawMessage, ExecutionState) error { return nil }, Dispatch: func(context.Context, OperationRequest, json.RawMessage) GatewayResponse {
		return GatewayResponse{Status: 200, Body: json.RawMessage(`{"ok":true}`), SessionRevision: revision.Add(1)}
	}}}
	return e, revision
}
func testGateway(t *testing.T, e GatewayExecutor) *Gateway {
	t.Helper()
	binding := digest.Bytes([]byte("binding"))
	g, err := NewGateway(GatewayConfig{SessionID: "session-1", EnvironmentDigest: binding, ApplicationDigest: binding, CapabilityManifestDigest: binding, FeedbackProfile: "black-box", IntegrationPrincipal: "operator", IntegrationToken: []byte(testIntegrationToken), Executor: e})
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func operation(id, op string, body []byte, revision uint64) OperationRequest {
	return OperationRequest{APIVersion: OperationRequestVersion, SessionID: "session-1", CampaignID: "campaign-1", WorkerInstanceID: "worker-1", RunRevision: 1, RequestID: "req-" + id, OperationID: id, Operation: op, ExpectedSessionRevision: revision, BodyDigest: digest.Bytes(body), Deadline: time.Now().Add(time.Minute)}
}
func serve(g http.Handler, r OperationRequest, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest("POST", "/v1/operations", bytes.NewReader(EncodeOperation(r, body)))
	request.Header.Set("Authorization", "Bearer "+testIntegrationToken)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, request)
	return w
}
func bind(t *testing.T, g http.Handler) {
	t.Helper()
	body := []byte(`{"action":"bind","session_id":"session-1","campaign_id":"campaign-1","worker_instance_id":"worker-1"}`)
	w := serve(g, operation("bind", "session.owner", body, 1), body)
	if w.Code != 200 {
		t.Fatalf("bind: %d %s", w.Code, w.Body)
	}
}
func TestGatewayReplacementSharesOwnershipAndResults(t *testing.T) {
	e, rev := executionFixture(t)
	g := testGateway(t, e)
	bind(t, g)
	body := []byte(`{}`)
	r := operation("invoke", "application.invoke", body, rev.Load())
	w := serve(g, r, body)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	after := rev.Load()
	replacement := testGateway(t, e)
	if w = serve(replacement, r, body); w.Code != 200 || rev.Load() != after {
		t.Fatalf("replay executed: %d %s", w.Code, w.Body)
	}
	changed := r
	changed.WorkerInstanceID = "worker-2"
	if w = serve(replacement, changed, body); w.Code != 200 || rev.Load() != after {
		t.Fatalf("same-campaign retry failed or repeated effect: %d", w.Code)
	}
	changed = r
	changed.BodyDigest = digest.Bytes([]byte(`{"x":1}`))
	if w = serve(replacement, changed, []byte(`{"x":1}`)); w.Code != 409 {
		t.Fatalf("changed bytes accepted: %d", w.Code)
	}
	changed = r
	changed.OperationID = "different"
	if w = serve(replacement, changed, body); w.Code != 409 {
		t.Fatal("reused request ID accepted")
	}
}
func TestCloseWhileDispatchedAndQueuedAcrossGateways(t *testing.T) {
	e, rev := executionFixture(t)
	first, second := testGateway(t, e), testGateway(t, e)
	bind(t, first)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e.hooks.Dispatch = func(ctx context.Context, _ OperationRequest, _ json.RawMessage) GatewayResponse {
		calls.Add(1)
		close(started)
		<-release
		if ctx.Err() != nil {
			t.Error("gateway cancellation propagated")
		}
		return GatewayResponse{Status: 200, Body: json.RawMessage(`{}`), SessionRevision: rev.Add(1)}
	}
	body := []byte(`{}`)
	running := operation("active", "application.invoke", body, rev.Load())
	done := make(chan *httptest.ResponseRecorder, 1)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("POST", "/v1/operations", bytes.NewReader(EncodeOperation(running, body))).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+testIntegrationToken)
	go func() { w := httptest.NewRecorder(); first.ServeHTTP(w, request); done <- w }()
	<-started
	cancel()
	if w := serve(second, running, body); w.Code != 202 {
		t.Fatalf("inflight duplicate: %d %s", w.Code, w.Body)
	}
	queuedDone := make(chan *httptest.ResponseRecorder, 1)
	queued := operation("queued", "application.invoke", body, rev.Load())
	go func() { queuedDone <- serve(second, queued, body) }()
	deadline := time.After(time.Second)
	for {
		if _, ok := e.store.Lookup("queued"); ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("not queued")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	closeBody := []byte(`{"action":"close_execution","session_id":"session-1","campaign_id":"campaign-1","worker_instance_id":"worker-1"}`)
	closed := make(chan *httptest.ResponseRecorder, 1)
	go func() { closed <- serve(second, operation("close", "session.owner", closeBody, 0), closeBody) }()
	select {
	case w := <-closed:
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("closure blocked on running effect")
	}
	close(release)
	if w := <-done; w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := <-queuedDone; w.Code != 409 || !strings.Contains(w.Body.String(), "execution_closed") {
		t.Fatalf("queued work ran: %d %s", w.Code, w.Body)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate effect")
	}
	bindBody := []byte(`{"action":"bind","session_id":"session-1","campaign_id":"campaign-1","worker_instance_id":"worker-1"}`)
	if w := serve(second, operation("rebind", "session.owner", bindBody, rev.Load()), bindBody); w.Code != 409 {
		t.Fatal("closed session reopened")
	}
}
func TestSupervisorRestartIsTerminalAndRetainsOutcomes(t *testing.T) {
	e, rev := executionFixture(t)
	g := testGateway(t, e)
	bind(t, g)
	body := []byte(`{}`)
	r := operation("done", "artifact.register", body, rev.Load())
	if serve(g, r, body).Code != 200 {
		t.Fatal("dispatch")
	}
	root := filepath.Dir(e.store.path)
	reopened, err := OpenExecutionStore(root, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.State().Owner.ClosedAt == nil {
		t.Fatal("supervisor restart reopened execution")
	}
	if rec, ok := reopened.Lookup("done"); !ok || rec.State != "completed" {
		t.Fatal("lost outcome")
	}
	if err := os.WriteFile(e.store.path, []byte(`{"api_version":"bad"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExecutionStore(root, "session-1"); err == nil {
		t.Fatal("corrupt state accepted")
	}
}
func TestGatewayRejectsLegacyAuthAndMalformedFrames(t *testing.T) {
	e, _ := executionFixture(t)
	g := testGateway(t, e)
	for _, path := range []string{"/v1/cross-vm", "/v1/execution-leases", "/v1/execution-leases/one"} {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(`{}`)))
		if w.Code != 404 {
			t.Fatalf("legacy %s: %d", path, w.Code)
		}
	}
	for _, token := range []string{"", "short", strings.Repeat("x", 32)} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/operations", strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+token)
		g.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("bad auth: %d", w.Code)
		}
	}
	for _, raw := range []string{`{"request":{},"request":{}}`, `{"a":{"x":1,"x":2}}`, `{} {}`, `null`, strings.Repeat(`[`, 65) + strings.Repeat(`]`, 65)} {
		var value any
		if err := StrictJSON([]byte(raw), &value); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	body := []byte(`{}`)
	r := operation("version", "session.status", body, 0)
	r.APIVersion = "interceptor.dev/cross-vm-request/v1alpha1"
	if w := serve(g, r, body); w.Code != 400 {
		t.Fatal("accepted old codec")
	}
}
func TestCapacityDeadlineAndStopPolicyFailBeforeEffects(t *testing.T) {
	e, rev := executionFixture(t)
	g := testGateway(t, e)
	bind(t, g)
	body := []byte(`{}`)
	if w := serve(g, operation("stop", "session.stop", body, rev.Load()), body); w.Code != 409 {
		t.Fatalf("stop accepted: %d", w.Code)
	}
	r := operation("expired", "artifact.register", body, rev.Load())
	r.Deadline = time.Now().Add(-time.Second)
	if w := serve(g, r, body); w.Code != 408 {
		t.Fatalf("expired accepted: %d", w.Code)
	}
	e.store.mu.Lock()
	for len(e.store.state.Records) < MaxOperationRecords {
		id := fmt.Sprintf("filled-%d", len(e.store.state.Records))
		e.store.state.Records[id] = &OperationRecord{State: "completed", Request: OperationRequest{OperationID: id, RequestID: id}}
	}
	e.store.mu.Unlock()
	if w := serve(g, operation("overflow", "artifact.register", body, rev.Load()), body); w.Code != 429 {
		t.Fatalf("capacity: %d", w.Code)
	}
	closeBody := []byte(`{"action":"close_execution","session_id":"session-1","campaign_id":"campaign-1","worker_instance_id":"worker-1"}`)
	if w := serve(g, operation("close-full", "session.owner", closeBody, 0), closeBody); w.Code != 200 {
		t.Fatalf("capacity prevented closure: %d %s", w.Code, w.Body)
	}
}

func TestCommittedOperationCodecFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/operation-request-v1alpha2.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope OperationEnvelope
	if err = StrictJSON(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if err = ValidateOperation(envelope.Request, envelope.Body, "session-1"); err != nil {
		t.Fatal(err)
	}
	var owner OwnerRequest
	if err = StrictJSON(envelope.Body, &owner); err != nil || owner.Action != "bind" {
		t.Fatalf("owner fixture: %+v %v", owner, err)
	}
	var roundtrip OperationEnvelope
	if err = StrictJSON(EncodeOperation(envelope.Request, envelope.Body), &roundtrip); err != nil || roundtrip.Request != envelope.Request || !bytes.Equal(roundtrip.Body, envelope.Body) {
		t.Fatal("codec changed command")
	}
}
func TestPersistenceFailureAndPrebindClosureFailClosed(t *testing.T) {
	e, rev := executionFixture(t)
	g := testGateway(t, e)
	bind(t, g)
	// Replacing the file with a directory makes the next atomic rename fail.
	if err := os.Remove(e.store.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.store.path, 0700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{}`)
	before := rev.Load()
	if w := serve(g, operation("storage-failure", "artifact.register", body, before), body); w.Code != 503 {
		t.Fatalf("persistence failure: %d", w.Code)
	}
	if rev.Load() != before || e.store.State().Owner.ClosedAt == nil {
		t.Fatal("effect admitted after persistence failure")
	}
	other, _ := executionFixture(t)
	if err := other.store.Close("administrator_stop"); err != nil {
		t.Fatal(err)
	}
	ownerBody := []byte(`{"action":"bind","session_id":"session-1","campaign_id":"campaign-1","worker_instance_id":"worker-1"}`)
	if w := serve(testGateway(t, other), operation("bind-after-stop", "session.owner", ownerBody, 1), ownerBody); w.Code != 409 {
		t.Fatal("prebind stop allowed later admission")
	}
}
