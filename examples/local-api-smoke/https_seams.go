package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/localapi"
)

func qualifyHTTPSSeams(binding localapi.Binding, output string, limit int64) {
	a := attempt(binding)
	for _, message := range []string{"allow", "deny"} {
		input, _ := json.Marshal(map[string]string{"message": message})
		raw := success(operation(binding, "application.invoke", map[string]any{"operation": "ask", "input": input, "media_type": "application/json", "attempt_id": a.AttemptID, "payload_digest": a.Payload.Digest}, &a))
		var turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		must(json.Unmarshal(raw, &turn))
		require(turn.Status == "complete", string(raw))
		var view adaptive.ObservationView
		must(json.Unmarshal(success(operation(binding, "observation.read", adaptive.ObservationRequest{TurnID: turn.ID}, &a)), &view))
		found := false
		for _, entry := range view.Entries {
			if entry.Kind != "target_output" || entry.Availability != "available" {
				continue
			}
			var chunk adaptive.FeedbackChunk
			must(json.Unmarshal(success(operation(binding, "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: view.ReceiptID, EntryID: entry.ID, MaxBytes: adaptive.MaxFeedbackChunk}, &a)), &chunk))
			var result struct {
				Executed bool `json:"executed"`
				Blocked  bool `json:"undeclared_blocked"`
			}
			must(json.Unmarshal(chunk.Content, &result))
			require(result.Executed == (message == "allow") && result.Blocked, "guardrail or DNS isolation failed")
			found = true
		}
		require(found, "missing target behavior evidence")
	}
	// Seams are deliberately absent from simulated services and injection selectors.
	definition := injection.Definition{ID: "forbidden-seam", Surface: "service_response", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{Service: "guardrail", CallOrdinal: "first"}, Placement: injection.Placement{Operation: "replace"}, Payload: json.RawMessage(`"smoke payload"`), AttemptID: a.AttemptID, PayloadDigest: a.Payload.Digest}
	rejected := operation(binding, "injection.arm", map[string]any{"definition": definition}, &a)
	require(rejected.Status >= 400, "seam injection accepted")
	success(post("/v1/lifecycle", localapi.LifecycleRequest{WorkerInstanceID: binding.WorkerInstanceID, RunRevision: binding.RunRevision, APIVersion: localapi.Version, CampaignID: campaign, SessionID: binding.SessionID, OperationID: "stop-seams", Operation: "session.stop"}))
	evidence(output, binding.SessionID, limit)
	raw, _ := json.MarshalIndent(map[string]any{"passed": true, "session_id": binding.SessionID, "target_output": "guardrail allow/deny enforced; undeclared DNS blocked", "live_langfuse": false}, "", "  ")
	must(os.WriteFile(filepath.Join(output, "result.json"), raw, 0600))
}
