package adaptive

import (
	"encoding/json"
	"testing"
)

func TestCampaignAccessAttributionAndDuplicateCommands(t *testing.T) {
	e, rev := executionFixture(t)
	bind(t, testGateway(t, e))
	body := []byte(`{}`)
	r := operation("work", "application.invoke", body, rev.Load())
	r.WorkerInstanceID, r.RunRevision = "different-worker", 8
	first := e.store.Execute("different-service", r, body, e.hooks)
	if first.Status != 200 {
		t.Fatalf("same campaign rejected: %s", first.Body)
	}
	record, _ := e.store.Lookup(r.OperationID)
	if record.Request.WorkerInstanceID != r.WorkerInstanceID || record.Request.RunRevision != 8 {
		t.Fatal("caller attribution lost")
	}
	after := rev.Load()
	retry := r
	retry.WorkerInstanceID, retry.RunRevision = "third-worker", 9
	if got := e.store.Execute("third-service", retry, body, e.hooks); got.Status != 200 || rev.Load() != after {
		t.Fatal("retry rejected or repeated effect")
	}
	record, _ = e.store.Lookup(r.OperationID)
	if record.Request.WorkerInstanceID != r.WorkerInstanceID || record.Fingerprint != OperationFingerprint(r) {
		t.Fatal("retry rewrote original audit")
	}
	retry.CampaignID = "another-campaign"
	if got := e.store.Execute("third-service", retry, body, e.hooks); got.Status < 400 {
		t.Fatal("cross-campaign duplicate accepted")
	}
	// A new mutated command is allowed with fresh IDs; reusing the original ID is not.
	changedBody := []byte(`{"payload":"changed"}`)
	changed := operation("mutated", "application.invoke", changedBody, rev.Load())
	changed.WorkerInstanceID, changed.RunRevision = "third-worker", 9
	if got := e.store.Execute("third-service", changed, changedBody, e.hooks); got.Status != 200 {
		t.Fatal("mutated new operation rejected")
	}
	changed.OperationID = r.OperationID
	if got := e.store.Execute("third-service", changed, changedBody, e.hooks); got.Status != 409 {
		t.Fatal("changed command reused ID")
	}
	// Close from another worker, even with legacy body attribution, stays terminal.
	closeBody, _ := json.Marshal(OwnerRequest{Action: "close_execution", SessionID: r.SessionID, CampaignID: r.CampaignID, WorkerInstanceID: "legacy-worker"})
	closing := operation("close", "session.owner", closeBody, rev.Load())
	closing.WorkerInstanceID, closing.RunRevision = "closing-worker", 12
	if got := e.store.Execute("closing-service", closing, closeBody, e.hooks); got.Status != 200 {
		t.Fatalf("close rejected: %s", got.Body)
	}
	closeRetry := closing
	closeRetry.WorkerInstanceID, closeRetry.RunRevision = "retry-worker", 13
	bodyRetry, _ := json.Marshal(OwnerRequest{Action: "close_execution", SessionID: r.SessionID, CampaignID: r.CampaignID, WorkerInstanceID: "different-legacy-worker"})
	closeRetry.BodyDigest = operation("unused", "session.owner", bodyRetry, 1).BodyDigest
	if got := e.store.Execute("retry-service", closeRetry, bodyRetry, e.hooks); got.Status != 200 {
		t.Fatalf("close retry rejected: %s", got.Body)
	}
	late := operation("late", "application.invoke", body, rev.Load())
	late.WorkerInstanceID, late.RunRevision = "new-worker", 14
	if got := e.store.Execute("new-service", late, body, e.hooks); got.Status != 409 {
		t.Fatal("worker change reopened closure")
	}
}
