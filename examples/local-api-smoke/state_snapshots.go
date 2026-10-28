package main

import (
	"encoding/json"
	"fmt"
	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/localapi"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"os"
	"path/filepath"
	"strings"
)

type stateOutput struct {
	Customer     string `json:"customer"`
	SessionID    string `json:"session_id"`
	Turn         int    `json:"turn"`
	Guard        string `json:"guard"`
	ServiceCount int    `json:"service_count"`
	FileCount    int    `json:"file_count"`
	Messages     int    `json:"messages"`
}

func stateTurn(binding localapi.Binding, a adaptive.AttemptContext, message string) stateOutput {
	input, err := json.Marshal(map[string]string{"message": message})
	must(err)
	raw := success(operation(binding, "application.invoke", map[string]any{"operation": "ask", "input": input, "media_type": "application/json", "attempt_id": a.AttemptID, "payload_digest": a.Payload.Digest}, &a))
	var turn struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	must(json.Unmarshal(raw, &turn))
	require(turn.Status == "complete", string(raw))
	var view adaptive.ObservationView
	must(json.Unmarshal(success(operation(binding, "observation.read", adaptive.ObservationRequest{TurnID: turn.ID}, &a)), &view))
	for _, entry := range view.Entries {
		if entry.Kind != "target_output" || entry.Availability != "available" {
			continue
		}
		var chunk adaptive.FeedbackChunk
		must(json.Unmarshal(success(operation(binding, "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: view.ReceiptID, EntryID: entry.ID, MaxBytes: adaptive.MaxFeedbackChunk}, &a)), &chunk))
		var value stateOutput
		must(json.Unmarshal(chunk.Content, &value))
		require(value.Customer == "alice" && value.SessionID != "" && value.Guard == "refund-review-required", fmt.Sprintf("identity/safeguards missing: %+v", value))
		return value
	}
	panic("state target output missing")
}

func qualifyStateSnapshots(first localapi.Binding, output string, limit int64, storageFailure bool, root string) {
	a := attempt(first)
	stateTurn(first, a, "change")
	boundary := stateTurn(first, a, "change")
	require(boundary.Turn == 2 && boundary.ServiceCount == 2 && boundary.FileCount == 2, "setup failed")
	capture := func(b localapi.Binding) snapshot.Checkpoint {
		var c snapshot.Checkpoint
		must(json.Unmarshal(success(operation(b, "snapshot.create", map[string]string{"label": "context-window"}, nil)), &c))
		return c
	}
	restore := func(b localapi.Binding, c snapshot.Checkpoint) localapi.Binding {
		var result struct {
			Binding localapi.Binding `json:"binding"`
		}
		must(json.Unmarshal(success(post("/v1/lifecycle", localapi.LifecycleRequest{WorkerInstanceID: b.WorkerInstanceID, RunRevision: b.RunRevision, APIVersion: localapi.Version, CampaignID: campaign, SessionID: b.SessionID, OperationID: "restore-" + c.ID, Operation: "snapshot.restore", CheckpointID: c.ID})), &result))
		require(result.Binding.SessionID != b.SessionID, "restore reused old process/session")
		return result.Binding
	}
	c := capture(first)
	baseline := stateTurn(first, a, "continue")
	changed := stateTurn(first, a, "change")
	require(changed.ServiceCount == 3 && changed.FileCount == 3, "post-checkpoint mutation missing")
	second := restore(first, c)
	b := attempt(second)
	resumed := stateTurn(second, b, "continue")
	require(resumed == baseline, fmt.Sprintf("restored continuation differs: %+v != %+v", resumed, baseline))
	after := stateTurn(second, b, "change")
	require(after == changed, "later continuation lost restored context")
	last := stateTurn(second, b, "continue")
	require(last.Turn == 5 && last.ServiceCount == 3, "memory did not continue")
	c2 := capture(second)
	stateTurn(second, b, "change")
	third := restore(second, c2)
	d := attempt(third)
	final := stateTurn(third, d, "continue")
	require(final.Turn == 6 && final.ServiceCount == 3 && final.FileCount == 3 && final.SessionID == baseline.SessionID && final.Messages > last.Messages, "second restore lost context/environment")
	if storageFailure {
		qualifyEvidenceFailure(third, d, root)
	}
	success(post("/v1/lifecycle", localapi.LifecycleRequest{WorkerInstanceID: third.WorkerInstanceID, RunRevision: third.RunRevision, APIVersion: localapi.Version, CampaignID: campaign, SessionID: third.SessionID, OperationID: "stop-state", Operation: "session.stop"}))
	for _, id := range []string{first.SessionID, second.SessionID, third.SessionID} {
		evidence(output, id, limit, storageFailure && id == third.SessionID)
	}
	raw, err := json.MarshalIndent(map[string]any{"passed": true, "evidence_failure": storageFailure, "session_id": first.SessionID, "sessions": []string{first.SessionID, second.SessionID, third.SessionID}, "target_output": "conversation, identity, safeguards, records and files restored", "baseline": baseline, "restored": resumed, "final": final, "checks": []string{"two process replacements", "exact continuation output", "continued memory", "no replayed tool mutations", "service/file rollback", "identity and guard preservation", "complete evidence"}}, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(output, "result.json"), raw, 0600))
	fmt.Println(string(raw))
}

// Test-only fault: remove write permission from this qualification session's
// mounted blob directory. No product fault flags or target modifications.
func qualifyEvidenceFailure(binding localapi.Binding, a adaptive.AttemptContext, root string) {
	require(root != "", "isolated data root is required for fault qualification")
	paths := session.PathsFor(root, binding.SessionID)
	info, err := os.Stat(paths.Blobs)
	must(err)
	must(os.Chmod(paths.Blobs, 0000))
	defer os.Chmod(paths.Blobs, info.Mode().Perm())
	input := []byte(`{"message":"change"}`)
	result := operation(binding, "application.invoke", map[string]any{"operation": "ask", "input": input, "media_type": "application/json", "attempt_id": a.AttemptID, "payload_digest": a.Payload.Digest}, &a)
	must(os.Chmod(paths.Blobs, info.Mode().Perm()))
	require(result.Status >= 400 && strings.Contains(string(result.Body), "evidence_storage_error"), fmt.Sprintf("missing capture failure: %d %s", result.Status, result.Body))
	next := operation(binding, "application.invoke", map[string]any{"operation": "ask", "input": input, "media_type": "application/json", "attempt_id": a.AttemptID, "payload_digest": a.Payload.Digest}, &a)
	require(next.Status >= 400, "next turn admitted after evidence failure")
	snap := operation(binding, "snapshot.create", map[string]string{"label": "incomplete"}, nil)
	require(snap.Status >= 400, "snapshot admitted after evidence failure")
	// Terminal host reporting retains existing receipts. A failed turn without
	// a retained receipt must explicitly report unavailable evidence.
	feedback := operation(binding, "observation.read", adaptive.ObservationRequest{TurnID: "turn-2"}, &a)
	require(feedback.Status == 503 && strings.Contains(string(feedback.Body), "evidence_storage_error"), fmt.Sprintf("failed feedback not unavailable: %d %s", feedback.Status, feedback.Body))
	raw, err := os.ReadFile(paths.Metadata)
	must(err)
	require(strings.Contains(string(raw), "evidence_incomplete"), "session failure not persisted")
}
