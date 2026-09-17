package localapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
)

func TestCampaignAccessAcrossWorkersAndLifecycleRetries(t *testing.T) {
	s, f := fixture(t, true)
	initial := f.stores["session-1"].State().Owner
	if initial.WorkerInstanceID != "operator-worker-1" || initial.Principal != "instance-1" {
		t.Fatal("Operator worker and Interceptor instance conflated")
	}
	if got := post(t, s, "/v1/attach", AttachRequest{APIVersion: Version, CampaignID: "campaign", AllowTargetStop: true, WorkerInstanceID: "operator-worker-2"}); got.Status != 200 {
		t.Fatal(got)
	}
	raw := json.RawMessage(`{}`)
	m, _ := f.Metadata("session-1")
	q := adaptive.OperationRequest{APIVersion: adaptive.OperationRequestVersion, RequestID: "request", OperationID: "operation", Operation: "application.invoke", SessionID: "session-1", CampaignID: "campaign", WorkerInstanceID: "operator-worker-2", RunRevision: 7, ExpectedSessionRevision: m.Revision, BodyDigest: digest.Bytes(raw), Deadline: time.Now().Add(time.Minute)}
	if got := post(t, s, "/v1/operations", adaptive.OperationEnvelope{Request: q, Body: raw}); got.Status != 200 {
		t.Fatal(got)
	}
	q.WorkerInstanceID, q.RunRevision = "operator-worker-3", 8
	if got := post(t, s, "/v1/operations", adaptive.OperationEnvelope{Request: q, Body: raw}); got.Status != 200 {
		t.Fatal(got)
	}
	saved, _ := f.stores["session-1"].Lookup("operation")
	if saved.Request.WorkerInstanceID != "operator-worker-2" || saved.Request.RunRevision != 7 {
		t.Fatal("original attribution changed")
	}
	restore := LifecycleRequest{APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", OperationID: "restore", Operation: "snapshot.restore", CheckpointID: "checkpoint", WorkerInstanceID: "operator-worker-3", RunRevision: 8}
	first := post(t, s, "/v1/lifecycle", restore)
	if first.Status != 200 {
		t.Fatalf("restore: %s", first.Body)
	}
	restore.WorkerInstanceID, restore.RunRevision = "operator-worker-4", 9
	if got := post(t, s, "/v1/lifecycle", restore); got.Status != 200 || f.restores != 1 {
		t.Fatal("lifecycle retry repeated or rejected")
	}
	if s.state.Active.RunRevision != 2 || s.state.Records["restore"].Request.RunRevision != 8 {
		t.Fatal("attribution changed lifecycle sequence")
	}
	if s.state.Records["restore"].Request.WorkerInstanceID != "operator-worker-3" {
		t.Fatal("lifecycle attribution overwritten")
	}
	q.Operation, q.RequestID, q.OperationID = "session.status", "status", "status"
	if got := post(t, s, "/v1/operations", adaptive.OperationEnvelope{Request: q, Body: raw}); got.Status != 200 {
		t.Fatal("retained same-campaign read rejected")
	}
	q.CampaignID = "different-campaign"
	if got := post(t, s, "/v1/operations", adaptive.OperationEnvelope{Request: q, Body: raw}); got.Status != 403 {
		t.Fatal("other campaign allowed")
	}
	q.CampaignID = "campaign"
	q.Operation = "application.invoke"
	if got := post(t, s, "/v1/operations", adaptive.OperationEnvelope{Request: q, Body: raw}); got.Status != 409 {
		t.Fatal("stale target mutation allowed")
	}
}
