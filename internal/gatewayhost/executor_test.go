package gatewayhost

import (
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
