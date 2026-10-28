package gatewayhost

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func TestRetainedLookupDoesNotRestartSession(t *testing.T) {
	root := t.TempDir()
	paths := session.Paths{Root: root}
	r := adaptive.OperationRequest{APIVersion: adaptive.OperationRequestVersion, SessionID: "session-1", CampaignID: "campaign", WorkerInstanceID: "worker", RunRevision: 1, OperationID: "read", RequestID: "read", Operation: "operation.status", Deadline: time.Now().Add(time.Minute)}
	state := adaptive.ExecutionState{APIVersion: adaptive.ExecutionStateVersion, SessionID: r.SessionID, Owner: &adaptive.ExecutionOwner{Principal: "host", CampaignID: r.CampaignID, WorkerInstanceID: r.WorkerInstanceID, RunRevision: 1, BoundAt: time.Now().UTC()}, Records: map[string]*adaptive.OperationRecord{"work": {Request: adaptive.OperationRequest{APIVersion: adaptive.OperationRequestVersion, SessionID: r.SessionID, OperationID: "work", RequestID: "work", BodyDigest: digest.Bytes([]byte(`{}`)), RunRevision: 1}, Fingerprint: digest.Bytes([]byte("work")), State: "running"}}}
	state.Records["work"].Fingerprint = adaptive.OperationFingerprint(state.Records["work"].Request)
	if err := fsutil.WriteJSON(filepath.Join(root, "execution.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	journal, err := statejournal.Open(filepath.Join(root, "state.jsonl"), r.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	raw, err := os.ReadFile(filepath.Join(root, "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := json.Marshal(map[string]string{"session_id": r.SessionID, "execution_state_digest": digest.Bytes(raw)})
	if _, err = journal.Append("execution_state", []statejournal.Operation{{Kind: "idempotency_commit", Value: value}}); err != nil {
		t.Fatal(err)
	}
	metadata := session.Metadata{ID: r.SessionID, Phase: session.Stopped}
	result, err := retainedOperation(metadata, paths, "host", r, []byte(`{"operation_id":"work"}`))
	if err != nil || result.Status != 200 || !strings.Contains(string(result.Body), `"state":"unknown"`) {
		t.Fatalf("lookup: %+v %v", result, err)
	}
	result, _ = retainedOperation(metadata, paths, "wrong-host", r, []byte(`{"operation_id":"work"}`))
	if result.Status != 200 {
		t.Fatal("same-campaign retained lookup rejected")
	}
	r.WorkerInstanceID = "another-worker"
	r.RunRevision = 7
	result, _ = retainedOperation(metadata, paths, "another-host", r, []byte(`{"operation_id":"work"}`))
	if result.Status != 200 {
		t.Fatal("cross-revision reporting rejected")
	}
	r.CampaignID = "another-campaign"
	result, _ = retainedOperation(metadata, paths, "host", r, []byte(`{"operation_id":"work"}`))
	if result.Status != 403 {
		t.Fatal("cross-campaign reporting accepted")
	}
	r.CampaignID = "campaign"
	r.Operation = "session.status"
	result, err = retainedOperation(metadata, paths, "host", r, []byte(`{}`))
	var status map[string]json.RawMessage
	_ = json.Unmarshal(result.Body, &status)
	if err != nil || string(status["execution_available"]) != "false" {
		t.Fatalf("status: %+v %v", result, err)
	}
	r.Operation = "observation.read"
	metadata.Taints = []string{"evidence_incomplete"}
	result, err = retainedOperation(metadata, paths, "host", r, []byte(`{"turn_id":"turn-2"}`))
	if err != nil || result.Status != 503 || !strings.Contains(string(result.Body), "evidence_storage_error") {
		t.Fatalf("lost evidence not explicit: %+v %v", result, err)
	}
	r.Operation = "application.invoke"
	result, _ = retainedOperation(metadata, paths, "host", r, []byte(`{}`))
	if result.Status != 409 {
		t.Fatal("retained state granted execution")
	}
}

func TestRetainedCloseRequiresConfirmedNativeClosure(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	state := adaptive.ExecutionState{APIVersion: adaptive.ExecutionStateVersion, SessionID: "session-1", Owner: &adaptive.ExecutionOwner{Principal: "host", CampaignID: "campaign", WorkerInstanceID: "worker", RunRevision: 1, BoundAt: now}, Records: map[string]*adaptive.OperationRecord{}}
	journal, err := statejournal.Open(filepath.Join(root, "state.jsonl"), state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	q := adaptive.OperationRequest{APIVersion: adaptive.OperationRequestVersion, SessionID: state.SessionID, CampaignID: "campaign", WorkerInstanceID: "worker", RunRevision: 1, OperationID: "close", RequestID: "close", Operation: "session.owner", Deadline: now.Add(time.Minute)}
	body, _ := json.Marshal(adaptive.OwnerRequest{Action: "close_execution", SessionID: q.SessionID, CampaignID: q.CampaignID, WorkerInstanceID: q.WorkerInstanceID})
	for _, closed := range []bool{false, true} {
		if closed {
			state.Owner.ClosedAt = &now
			state.ClosedAt = &now
		}
		if err := fsutil.WriteJSON(filepath.Join(root, "execution.json"), state, 0600); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(root, "execution.json"))
		if err != nil {
			t.Fatal(err)
		}
		value, _ := json.Marshal(map[string]string{"session_id": state.SessionID, "execution_state_digest": digest.Bytes(raw)})
		if _, err = journal.Append("execution_state", []statejournal.Operation{{Kind: "idempotency_commit", Value: value}}); err != nil {
			t.Fatal(err)
		}
		got, err := retainedOperation(session.Metadata{ID: q.SessionID, Phase: session.Stopped}, session.Paths{Root: root}, "host", q, body)
		want := 409
		if closed {
			want = 200
		}
		if err != nil || got.Status != want {
			t.Fatalf("closed=%v: %+v %v", closed, got, err)
		}
	}
}

// A target exit between closure and confirmation must not change the body schema.
func TestLiveAndRetainedClosureUseSameResponse(t *testing.T) {
	root := t.TempDir()
	const sessionID = "session-1"
	journal, err := statejournal.Open(filepath.Join(root, "state.jsonl"), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	initial := adaptive.ExecutionState{APIVersion: adaptive.ExecutionStateVersion, SessionID: sessionID, Records: map[string]*adaptive.OperationRecord{}}
	if err := fsutil.WriteJSON(filepath.Join(root, "execution.json"), initial, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := adaptive.OpenExecutionStore(root, sessionID, journal)
	if err != nil {
		t.Fatal(err)
	}
	revision := uint64(1)
	hooks := adaptive.ExecutionHooks{
		Revision:     func() uint64 { return revision },
		OwnerChanged: func(adaptive.ExecutionOwner) error { revision++; return nil },
		Check:        func(adaptive.OperationRequest, json.RawMessage, adaptive.ExecutionState) error { return nil },
	}
	request := func(action string) (adaptive.OperationRequest, []byte) {
		t.Helper()
		body, err := json.Marshal(adaptive.OwnerRequest{Action: action, SessionID: sessionID, CampaignID: "campaign", WorkerInstanceID: "worker"})
		if err != nil {
			t.Fatal(err)
		}
		return adaptive.OperationRequest{APIVersion: adaptive.OperationRequestVersion, SessionID: sessionID, CampaignID: "campaign", WorkerInstanceID: "worker", RunRevision: 1, OperationID: action, RequestID: action, Operation: "session.owner", ExpectedSessionRevision: revision, BodyDigest: digest.Bytes(body), Deadline: time.Now().Add(time.Minute)}, body
	}
	bind, body := request("bind")
	if got := store.Execute("host", bind, body, hooks); got.Status != 200 {
		t.Fatalf("bind: %+v", got)
	}
	closeRequest, closeBody := request("close_execution")
	live := store.Execute("host", closeRequest, closeBody, hooks)
	decode := func(t *testing.T, response adaptive.GatewayResponse) {
		t.Helper()
		// Decode both the actual wire envelope and its body through the same strict types.
		raw, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		var envelope adaptive.GatewayResponse
		if err := adaptive.StrictJSON(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Status != 200 || envelope.SessionRevision != revision {
			t.Fatalf("closure envelope: %+v", envelope)
		}
		var owner adaptive.ExecutionOwner
		if err := adaptive.StrictJSON(envelope.Body, &owner); err != nil {
			t.Fatalf("closure body: %v; %s", err, envelope.Body)
		}
		if owner.CampaignID != "campaign" || owner.WorkerInstanceID != "worker" || owner.RunRevision != 1 || owner.ClosedAt == nil || owner.ClosureReason != "controller_closed_execution" {
			t.Fatalf("closure identity/state: %+v", owner)
		}
	}
	decode(t, live)
	for _, phase := range []session.Phase{session.Stopped, session.Error} {
		t.Run(string(phase), func(t *testing.T) {
			got, err := retainedOperation(session.Metadata{ID: sessionID, Phase: phase, Revision: revision}, session.Paths{Root: root}, "host", closeRequest, closeBody)
			if err != nil {
				t.Fatal(err)
			}
			decode(t, got)
			if !bytes.Equal(got.Body, live.Body) {
				t.Fatalf("closure body changed after target exit: live=%s retained=%s", live.Body, got.Body)
			}
		})
	}
}
