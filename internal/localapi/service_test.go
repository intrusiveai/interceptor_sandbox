package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

type fakeRuntime struct {
	snapshots       map[string][]snapshot.Checkpoint
	snapshotError   bool
	dispatchResult  *adaptive.GatewayResponse
	executeError    error
	statusError     bool
	closeOnValidate bool
	enteredBind     chan struct{}
	releaseBind     chan struct{}
	mu              sync.Mutex
	active          string
	metadata        map[string]session.Metadata
	stores          map[string]*adaptive.ExecutionStore
	root            string
	restores        int
	blockRestore    chan struct{}
	enteredRestore  chan struct{}
	stopError       bool
	calls           []string
}

func fixture(t *testing.T, allow bool) (*Service, *fakeRuntime) {
	t.Helper()
	root := t.TempDir()
	f := &fakeRuntime{active: "session-1", root: root, metadata: map[string]session.Metadata{}, stores: map[string]*adaptive.ExecutionStore{}}
	f.add(t, "session-1")
	s, err := New(t.Context(), f, f.metadata["session-1"], "instance-1", filepath.Join(root, "instance.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := post(t, s, "/v1/attach", AttachRequest{APIVersion: Version, CampaignID: "campaign", AllowTargetStop: allow, WorkerInstanceID: "operator-worker-1"}); got.Status != 200 {
		t.Fatalf("attach: %+v", got)
	}
	return s, f
}
func (f *fakeRuntime) add(t *testing.T, id string) {
	t.Helper()
	store, err := fakeStore(f.root, id)
	if err != nil {
		t.Fatal(err)
	}
	f.stores[id] = store
	f.metadata[id] = session.Metadata{ID: id, Phase: session.Running, Revision: 1}
}
func (f *fakeRuntime) Metadata(id string) (session.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.metadata[id]
	if !ok {
		return m, errors.New("missing")
	}
	return m, nil
}
func (f *fakeRuntime) Execute(ctx context.Context, principal string, r adaptive.OperationRequest, body json.RawMessage) (adaptive.GatewayResponse, error) {
	f.mu.Lock()
	store := f.stores[r.SessionID]
	f.calls = append(f.calls, r.Operation+":"+r.SessionID)
	f.mu.Unlock()
	if r.Operation != "session.owner" && r.Operation != "session.status" && f.executeError != nil {
		return adaptive.GatewayResponse{}, f.executeError
	}
	if r.Operation == "session.status" && f.statusError {
		return adaptive.OperationError(503, "status_unavailable"), nil
	}
	result := store.Execute(principal, r, body, adaptive.ExecutionHooks{
		Check:    func(adaptive.OperationRequest, json.RawMessage, adaptive.ExecutionState) error { return nil },
		Revision: func() uint64 { m, _ := f.Metadata(r.SessionID); return m.Revision },
		OwnerChanged: func(adaptive.ExecutionOwner) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			m := f.metadata[r.SessionID]
			m.Revision++
			f.metadata[r.SessionID] = m
			return nil
		},
		Dispatch: func(context.Context, adaptive.OperationRequest, json.RawMessage) adaptive.GatewayResponse {
			if r.Operation == "session.status" {
				m, _ := f.Metadata(r.SessionID)
				state := store.State()
				return response(200, map[string]any{"session": m, "owner": state.Owner, "execution_available": state.Owner != nil && state.Owner.ClosedAt == nil && m.Phase == session.Running})
			}
			if f.dispatchResult != nil {
				return *f.dispatchResult
			}
			return response(200, map[string]any{"ok": true})
		},
	})
	if r.Operation == "session.owner" && r.RunRevision == 2 && f.enteredBind != nil {
		var owner adaptive.OwnerRequest
		_ = json.Unmarshal(body, &owner)
		if owner.Action == "bind" {
			close(f.enteredBind)
			select {
			case <-f.releaseBind:
			case <-ctx.Done():
			}
		}
	}
	return result, nil
}
func (f *fakeRuntime) ValidateRestore(_ context.Context, _, id string) error {
	if f.closeOnValidate {
		return f.stores[f.active].Close("operation_outcome_unknown")
	}
	if id == "missing" {
		return errors.New("missing")
	}
	return nil
}
func (f *fakeRuntime) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopError {
		return errors.New("unconfirmed")
	}
	if f.active != id {
		return errors.New("wrong active")
	}
	f.active = ""
	m := f.metadata[id]
	m.Phase = session.Stopped
	f.metadata[id] = m
	f.calls = append(f.calls, "stop:"+id)
	return nil
}
func (f *fakeRuntime) Restore(ctx context.Context, _, _ string) (session.Metadata, error) {
	if f.enteredRestore != nil {
		close(f.enteredRestore)
		select {
		case <-f.blockRestore:
		case <-ctx.Done():
			return session.Metadata{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active != "" {
		return session.Metadata{}, errors.New("overlapping containers")
	}
	f.restores++
	id := fmt.Sprintf("session-%d", f.restores+1)
	store, err := fakeStore(f.root, id)
	if err != nil {
		return session.Metadata{}, err
	}
	f.stores[id] = store
	m := session.Metadata{ID: id, Phase: session.Running, Revision: 1}
	f.metadata[id] = m
	f.active = id
	return m, nil
}
func (f *fakeRuntime) Export(_ context.Context, id string) (*os.File, error) {
	m, err := f.Metadata(id)
	if err != nil || m.Phase != session.Stopped {
		return nil, errors.New("not stopped")
	}
	file, err := os.CreateTemp(f.root, "export-")
	if err != nil {
		return nil, err
	}
	_, _ = file.WriteString("protected evidence " + id)
	_, _ = file.Seek(0, 0)
	return file, nil
}
func post(t *testing.T, s *Service, path string, input any) adaptive.GatewayResponse {
	t.Helper()
	raw, _ := json.Marshal(input)
	r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var got adaptive.GatewayResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("%s: %s", path, w.Body.String())
	}
	if got.Status != w.Code {
		t.Fatalf("status disagreement %d: %+v", w.Code, got)
	}
	return got
}
func op(t *testing.T, s *Service, f *fakeRuntime, id, name, sessionID string, revision uint64, body any) adaptive.GatewayResponse {
	t.Helper()
	raw, _ := json.Marshal(body)
	m, _ := f.Metadata(sessionID)
	q := adaptive.OperationRequest{APIVersion: adaptive.OperationRequestVersion, RequestID: id, OperationID: id, Operation: name, SessionID: sessionID, CampaignID: "campaign", WorkerInstanceID: "instance-1", RunRevision: revision, ExpectedSessionRevision: m.Revision, BodyDigest: digest.Bytes(raw), Deadline: time.Now().Add(time.Minute)}
	return post(t, s, "/v1/operations", adaptive.OperationEnvelope{Request: q, Body: raw})
}
func TestLifecycleNewSessionReplayAndEvidence(t *testing.T) {
	s, f := fixture(t, true)
	if got := post(t, s, "/v1/attach", AttachRequest{APIVersion: Version, CampaignID: "campaign", AllowTargetStop: true, WorkerInstanceID: "operator-worker-1"}); got.Status != 200 {
		t.Fatalf("reattach: %+v", got)
	}
	if got := post(t, s, "/v1/attach", AttachRequest{APIVersion: Version, CampaignID: "other", AllowTargetStop: true, WorkerInstanceID: "operator-worker-1"}); got.Status != 409 {
		t.Fatal(got)
	}
	if got := op(t, s, f, "execute-1", "application.invoke", "session-1", 1, map[string]any{}); got.Status != 200 {
		t.Fatal(got)
	}
	if got := op(t, s, f, "snapshot-1", "snapshot.create", "session-1", 1, map[string]any{}); got.Status != 200 {
		t.Fatal(got)
	}
	restore := LifecycleRequest{WorkerInstanceID: "operator-worker-1", RunRevision: 1, APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", OperationID: "restore-1", Operation: "snapshot.restore", CheckpointID: "checkpoint"}
	first := post(t, s, "/v1/lifecycle", restore)
	if first.Status != 200 {
		t.Fatal(first)
	}
	again := post(t, s, "/v1/lifecycle", restore)
	if !bytes.Equal(first.Body, again.Body) || f.restores != 1 {
		t.Fatalf("restore replay: %s %s (%d)", first.Body, again.Body, f.restores)
	}
	restore.CheckpointID = "different"
	if got := post(t, s, "/v1/lifecycle", restore); got.Status != 409 {
		t.Fatal(got)
	}
	if got := op(t, s, f, "delayed", "application.invoke", "session-1", 1, map[string]any{}); got.Status != 409 {
		t.Fatal(got)
	}
	if got := op(t, s, f, "execute-2", "application.invoke", "session-2", 2, map[string]any{}); got.Status != 200 {
		t.Fatal(got)
	}
	stop := LifecycleRequest{WorkerInstanceID: "operator-worker-1", RunRevision: 1, APIVersion: Version, CampaignID: "campaign", SessionID: "session-2", OperationID: "stop-1", Operation: "session.stop"}
	if got := post(t, s, "/v1/lifecycle", stop); got.Status != 200 {
		t.Fatal(got)
	}
	for _, id := range []string{"session-1", "session-2"} {
		raw, _ := json.Marshal(map[string]string{"api_version": Version, "campaign_id": "campaign", "session_id": id})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("POST", "/v1/evidence", bytes.NewReader(raw)))
		if w.Code != 200 || w.Header().Get("X-Content-SHA256") != digest.Bytes(w.Body.Bytes()) {
			t.Fatalf("evidence: %d %s", w.Code, w.Body.String())
		}
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted State
	if err = json.Unmarshal(raw, &persisted); err != nil || len(persisted.Sessions) != 2 || !persisted.Closed {
		t.Fatalf("ledger: %s %v", raw, err)
	}
}
func TestRestoreInProgressAndCloseDuringStartup(t *testing.T) {
	s, f := fixture(t, true)
	f.enteredRestore = make(chan struct{})
	f.blockRestore = make(chan struct{})
	restore := LifecycleRequest{WorkerInstanceID: "operator-worker-1", RunRevision: 1, APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", OperationID: "restore", Operation: "snapshot.restore", CheckpointID: "checkpoint"}
	done := make(chan adaptive.GatewayResponse, 1)
	go func() { done <- post(t, s, "/v1/lifecycle", restore) }()
	<-f.enteredRestore
	if got := post(t, s, "/v1/lifecycle", restore); got.Status != 202 {
		t.Fatal(got)
	}
	body := adaptive.OwnerRequest{Action: "close_execution", SessionID: "session-1", CampaignID: "campaign", WorkerInstanceID: "instance-1"}
	if got := op(t, s, f, "close", "session.owner", "session-1", 1, body); got.Status >= 300 {
		t.Fatal(got)
	}
	close(f.blockRestore)
	if got := <-done; got.Status < 300 {
		t.Fatal("closed campaign returned healthy restore")
	}
	if f.active != "" {
		t.Fatal("replacement survived concurrent close")
	}
}
func TestStopChoiceAndRestoreFailure(t *testing.T) {
	s, f := fixture(t, false)
	stop := LifecycleRequest{WorkerInstanceID: "operator-worker-1", RunRevision: 1, APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", OperationID: "stop", Operation: "session.stop"}
	if got := post(t, s, "/v1/lifecycle", stop); got.Status != 403 {
		t.Fatal(got)
	}
	restore := stop
	restore.Operation = "snapshot.restore"
	restore.OperationID = "restore"
	restore.CheckpointID = "missing"
	if got := post(t, s, "/v1/lifecycle", restore); got.Status != 409 || f.active != "session-1" {
		t.Fatal(got)
	}
	restore.OperationID = "restore-valid"
	restore.CheckpointID = "checkpoint"
	f.stopError = true
	if got := post(t, s, "/v1/lifecycle", restore); got.Status != 503 || f.restores != 0 {
		t.Fatal(got)
	}
	if got := op(t, s, f, "late", "application.invoke", "session-1", 1, map[string]any{}); got.Status != 409 {
		t.Fatal(got)
	}
}
func TestStrictScopeAndParsing(t *testing.T) {
	s, _ := fixture(t, true)
	for _, raw := range []string{`{"api_version":"x","campaign_id":"campaign","campaign_id":"other"}`, `{"api_version":"x","campaign_id":"campaign","unexpected":true}`} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("POST", "/v1/attach", bytes.NewBufferString(raw)))
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	if got := post(t, s, "/v1/status", map[string]string{"api_version": Version, "campaign_id": "other"}); got.Status != 403 {
		t.Fatal(got)
	}
	if got := post(t, s, "/v1/evidence", map[string]string{"api_version": Version, "campaign_id": "campaign", "session_id": "not-owned"}); got.Status != 403 {
		t.Fatal(got)
	}
}

func fakeStore(root, id string) (*adaptive.ExecutionStore, error) {
	path := filepath.Join(root, id)
	if err := fsutil.WriteJSON(filepath.Join(path, "execution.json"), adaptive.ExecutionState{APIVersion: adaptive.ExecutionStateVersion, SessionID: id, Records: map[string]*adaptive.OperationRecord{}}, 0600); err != nil {
		return nil, err
	}
	return adaptive.OpenExecutionStore(path, id)
}

func TestAdmittedRestoreSurvivesClientDisconnect(t *testing.T) {
	s, f := fixture(t, true)
	f.enteredRestore = make(chan struct{})
	f.blockRestore = make(chan struct{})
	input := LifecycleRequest{WorkerInstanceID: "operator-worker-1", RunRevision: 1, APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", OperationID: "disconnected-restore", Operation: "snapshot.restore", CheckpointID: "checkpoint"}
	raw, _ := json.Marshal(input)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/lifecycle", bytes.NewReader(raw)).WithContext(ctx)
	finished := make(chan struct{})
	go func() { defer close(finished); s.ServeHTTP(httptest.NewRecorder(), req) }()
	<-f.enteredRestore
	cancel()
	close(f.blockRestore)
	<-finished
	got := post(t, s, "/v1/lifecycle", input)
	if got.Status != 200 || f.restores != 1 || f.active != "session-2" {
		t.Fatalf("disconnect replay: %+v restores=%d active=%s", got, f.restores, f.active)
	}
}

func TestCloseSourceDuringReplacementBinding(t *testing.T) {
	s, f := fixture(t, true)
	f.enteredBind = make(chan struct{})
	f.releaseBind = make(chan struct{})
	input := LifecycleRequest{WorkerInstanceID: "operator-worker-1", RunRevision: 1, APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", OperationID: "binding-restore", Operation: "snapshot.restore", CheckpointID: "checkpoint"}
	done := make(chan adaptive.GatewayResponse, 1)
	go func() { done <- post(t, s, "/v1/lifecycle", input) }()
	<-f.enteredBind
	closeBody := adaptive.OwnerRequest{Action: "close_execution", SessionID: "session-1", CampaignID: "campaign", WorkerInstanceID: "instance-1"}
	got := op(t, s, f, "terminal-close", "session.owner", "session-1", 1, closeBody)
	if got.Status != 200 {
		t.Fatalf("close against transitioning source rejected: %+v", got)
	}
	close(f.releaseBind)
	if got = <-done; got.Status < 300 || f.active != "" {
		t.Fatalf("replacement continued after close: %+v active=%s", got, f.active)
	}
}

func assertCampaignClosed(t *testing.T, s *Service) {
	t.Helper()
	got := post(t, s, "/v1/status", map[string]string{"api_version": Version, "campaign_id": "campaign"})
	var status struct {
		Closed bool `json:"closed"`
	}
	if got.Status != 200 || json.Unmarshal(got.Body, &status) != nil || !status.Closed {
		t.Fatalf("status not closed: %+v", got)
	}
	raw, err := os.ReadFile(s.path)
	var saved State
	if err != nil || json.Unmarshal(raw, &saved) != nil || !saved.Closed {
		t.Fatalf("closure not persisted: %s %v", raw, err)
	}
}

func TestUncertainMutationClosesCampaignAndRejectsRestore(t *testing.T) {
	for _, kind := range []string{"server_error", "oversized_result", "transport_error"} {
		t.Run(kind, func(t *testing.T) {
			s, f := fixture(t, true)
			switch kind {
			case "server_error":
				result := adaptive.OperationError(503, "deadline_outcome_unknown")
				f.dispatchResult = &result
			case "oversized_result":
				result := adaptive.GatewayResponse{Status: 200, Body: bytes.Repeat([]byte("x"), adaptive.MaxOperationResponse+1)}
				f.dispatchResult = &result
			case "transport_error":
				f.executeError = errors.New("reply lost after dispatch")
			}
			got := op(t, s, f, "uncertain", "application.invoke", "session-1", 1, map[string]any{})
			if got.Status < 400 {
				t.Fatalf("expected uncertain outcome: %+v", got)
			}
			assertCampaignClosed(t, s)
			if kind != "transport_error" && f.stores["session-1"].State().ClosedAt == nil {
				t.Fatal("native execution did not close")
			}
			f.dispatchResult, f.executeError = nil, nil
			restore := LifecycleRequest{WorkerInstanceID: "operator-worker-1", RunRevision: 1, APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", OperationID: "restore", Operation: "snapshot.restore", CheckpointID: "checkpoint", SourceSessionID: ""}
			if got = post(t, s, "/v1/lifecycle", restore); got.Status != 409 || f.restores != 0 || f.active != "session-1" {
				t.Fatalf("restore admitted: %+v", got)
			}
			if got = op(t, s, f, "late", "application.invoke", "session-1", 1, map[string]any{}); got.Status != 409 {
				t.Fatal(got)
			}
			if got = op(t, s, f, "status", "session.status", "session-1", 1, map[string]any{}); got.Status != 200 {
				t.Fatal(got)
			}
			if got = op(t, s, f, "cleanup", "injection.delete", "session-1", 1, map[string]any{}); got.Status != 200 {
				t.Fatal(got)
			}
			stop := LifecycleRequest{WorkerInstanceID: "operator-worker-1", RunRevision: 1, APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", OperationID: "stop", Operation: "session.stop", CheckpointID: "", SourceSessionID: ""}
			if got = post(t, s, "/v1/lifecycle", stop); got.Status != 200 {
				t.Fatal(got)
			}
		})
	}
}

func TestRestoreChecksNativeClosureIndependently(t *testing.T) {
	for _, kind := range []string{"closed", "unavailable", "closure_during_preflight"} {
		t.Run(kind, func(t *testing.T) {
			s, f := fixture(t, false)
			switch kind {
			case "closed":
				if err := f.stores["session-1"].Close("operation_outcome_unknown"); err != nil {
					t.Fatal(err)
				}
			case "unavailable":
				f.statusError = true
			case "closure_during_preflight":
				f.closeOnValidate = true
			}
			if s.state.Closed {
				t.Fatal("test requires stale API flag")
			}
			restore := LifecycleRequest{WorkerInstanceID: "operator-worker-1", RunRevision: 1, APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", OperationID: "restore", Operation: "snapshot.restore", CheckpointID: "checkpoint", SourceSessionID: ""}
			got := post(t, s, "/v1/lifecycle", restore)
			if got.Status < 400 || f.restores != 0 || f.active != "session-1" {
				t.Fatalf("restore admitted: %+v", got)
			}
			for _, call := range f.calls {
				if call == "stop:session-1" {
					t.Fatal("rejected restore stopped source")
				}
			}
			assertCampaignClosed(t, s)
		})
	}
}

func TestKnownRejectionDoesNotCloseCampaign(t *testing.T) {
	s, f := fixture(t, true)
	result := adaptive.OperationError(409, "invalid_attempt")
	f.dispatchResult = &result
	if got := op(t, s, f, "rejected", "application.invoke", "session-1", 1, map[string]any{}); got.Status != 409 {
		t.Fatal(got)
	}
	if s.state.Closed {
		t.Fatal("known rejection closed campaign")
	}
	f.dispatchResult = nil
	restore := LifecycleRequest{WorkerInstanceID: "operator-worker-1", RunRevision: 1, APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", OperationID: "restore", Operation: "snapshot.restore", CheckpointID: "checkpoint", SourceSessionID: ""}
	if got := post(t, s, "/v1/lifecycle", restore); got.Status != 200 {
		t.Fatal(got)
	}
}

func TestClosureLedgerFailureStillBlocksRestore(t *testing.T) {
	s, f := fixture(t, true)
	result := adaptive.OperationError(503, "operation_outcome_unknown")
	f.dispatchResult = &result
	// A regular file cannot be used as the ledger's parent directory.
	s.path = filepath.Join(s.path, "ledger.json")
	if got := op(t, s, f, "uncertain", "application.invoke", "session-1", 1, map[string]any{}); got.Status != 503 {
		t.Fatal(got)
	}
	if !s.state.Closed || !s.failed {
		t.Fatal("ledger failure left admission open")
	}
	restore := LifecycleRequest{WorkerInstanceID: "operator-worker-1", RunRevision: 1, APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", OperationID: "restore", Operation: "snapshot.restore", CheckpointID: "checkpoint", SourceSessionID: ""}
	if got := post(t, s, "/v1/lifecycle", restore); got.Status != 503 || f.restores != 0 {
		t.Fatal(got)
	}
}

func TestSpoolFailureClosesWithoutMutationGateAndBlocksRestore(t *testing.T) {
	s, f := fixture(t, true)
	if !s.lockGate(context.Background()) {
		t.Fatal("gate unavailable")
	}
	// The quota checker must not wait for a stuck native mutation.
	done := make(chan error, 1)
	go func() { done <- s.failTransport() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fault waited for mutation gate")
	}
	s.unlockGate()
	assertCampaignClosed(t, s)
	request := LifecycleRequest{APIVersion: Version, CampaignID: "campaign", SessionID: f.active, OperationID: "after-spool-fault", Operation: "snapshot.restore", CheckpointID: "checkpoint", WorkerInstanceID: "worker", RunRevision: 1}
	if got := post(t, s, "/v1/lifecycle", request); got.Status != 409 {
		t.Fatalf("restore admitted after quota fault: %+v", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.restores != 0 {
		t.Fatal("replacement started")
	}
}
