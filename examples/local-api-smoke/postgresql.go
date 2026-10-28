package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/localapi"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

type databaseOutput struct {
	stateOutput
	Body      string   `json:"body"`
	Documents int      `json:"documents"`
	Refunds   int      `json:"refunds"`
	Nearest   []string `json:"nearest"`
}

func databaseTurn(b localapi.Binding, a adaptive.AttemptContext, message string) databaseOutput {
	input, _ := json.Marshal(map[string]string{"message": message})
	raw := success(operation(b, "application.invoke", map[string]any{"operation": "ask", "input": input, "media_type": "application/json", "attempt_id": a.AttemptID, "payload_digest": a.Payload.Digest}, &a))
	var turn struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	must(json.Unmarshal(raw, &turn))
	require(turn.Status == "complete", string(raw))
	var view adaptive.ObservationView
	must(json.Unmarshal(success(operation(b, "observation.read", adaptive.ObservationRequest{TurnID: turn.ID}, &a)), &view))
	for _, entry := range view.Entries {
		if entry.Kind != "target_output" || entry.Availability != "available" {
			continue
		}
		var chunk adaptive.FeedbackChunk
		must(json.Unmarshal(success(operation(b, "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: view.ReceiptID, EntryID: entry.ID, MaxBytes: adaptive.MaxFeedbackChunk}, &a)), &chunk))
		var output databaseOutput
		must(json.Unmarshal(chunk.Content, &output))
		return output
	}
	panic("database target output missing")
}
func qualifyPostgreSQL(first localapi.Binding, output string, limit int64, root string) {
	a := attempt(first)
	seed := databaseTurn(first, a, "continue")
	require(seed.Body == "benign" && seed.Documents == 2 && strings.Join(seed.Nearest, ",") == "one,Alice" && seed.Refunds == 0, "seed/join/vector retrieval failed")
	arm := func(b localapi.Binding, a adaptive.AttemptContext, d injection.Definition) {
		d.AttemptID = a.AttemptID
		d.PayloadDigest = digest.Bytes(d.Payload)
		require(d.PayloadDigest == a.Payload.Digest, "payload must match registered attempt")
		success(operation(b, "injection.arm", map[string]any{"definition": d}, &a))
	}
	response := injection.Definition{ID: "db-response", Surface: "database_response", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{Service: "database", CallOrdinal: "first", RequestBodyPointer: "/query", RequestBodyContain: "SELECT body"}, Placement: injection.Placement{Operation: "append_text", Pointer: "/rows/0/0"}, Payload: json.RawMessage(`"smoke payload"`)}
	arm(first, a, response)
	require(strings.Contains(databaseTurn(first, a, "continue").Body, "smoke payload"), "response rewrite missing")
	require(databaseTurn(first, a, "continue").Body == "benign", "response injection changed database or fired twice")
	persistent := response
	persistent.ID = "db-state"
	persistent.Surface = "environment_state"
	persistent.Mode = "fidelity"
	persistent.Selector = injection.Selector{Service: "database", Collection: "documents", Key: `{"id":"one"}`}
	persistent.Placement.Pointer = "/body"
	arm(first, a, persistent)
	require(strings.Contains(databaseTurn(first, a, "continue").Body, "smoke payload"), "persistent mutation missing")
	rolled := databaseTurn(first, a, "rollback")
	require(rolled.Refunds == 0, "rollback persisted refund")
	committed := databaseTurn(first, a, "refund")
	require(committed.Refunds == 1, "committed refund absent")
	databaseTurn(first, a, "change")
	response.ID = "db-after-checkpoint"
	arm(first, a, response)
	var checkpoint snapshot.Checkpoint
	must(json.Unmarshal(success(operation(first, "snapshot.create", map[string]string{"label": "postgresql"}, nil)), &checkpoint))
	baseline := databaseTurn(first, a, "continue")
	require(strings.Count(baseline.Body, "smoke payload") == 2, "armed response missing after snapshot")
	databaseTurn(first, a, "change")
	var restored struct {
		Binding localapi.Binding `json:"binding"`
	}
	must(json.Unmarshal(success(post("/v1/lifecycle", localapi.LifecycleRequest{WorkerInstanceID: first.WorkerInstanceID, RunRevision: first.RunRevision, APIVersion: localapi.Version, CampaignID: campaign, SessionID: first.SessionID, OperationID: "restore-database", Operation: "snapshot.restore", CheckpointID: checkpoint.ID})), &restored))
	second := restored.Binding
	b := attempt(second)
	resumed := databaseTurn(second, b, "continue")
	x, _ := json.Marshal(baseline)
	y, _ := json.Marshal(resumed)
	require(string(x) == string(y), fmt.Sprintf("database/application/injection restore differs: %s != %s", x, y))
	next := databaseTurn(second, b, "continue")
	require(strings.Count(next.Body, "smoke payload") == 1 && next.ServiceCount == 1 && next.Refunds == 1, "restored state/progress incorrect")
	if root != "" {
		qualifyEvidenceFailure(second, b, root)
	}
	success(post("/v1/lifecycle", localapi.LifecycleRequest{WorkerInstanceID: second.WorkerInstanceID, RunRevision: second.RunRevision, APIVersion: localapi.Version, CampaignID: campaign, SessionID: second.SessionID, OperationID: "stop-database", Operation: "session.stop"}))
	evidence(output, first.SessionID, limit)
	evidence(output, second.SessionID, limit, root != "")
	raw, _ := json.MarshalIndent(map[string]any{"passed": true, "session_id": first.SessionID, "sessions": []string{first.SessionID, second.SessionID}, "target_output": "seeded PostgreSQL/vector retrieval, response and state injection, committed/rolled back refund, database/application restore", "baseline": baseline, "restored": resumed, "evidence_failure": root != ""}, "", "  ")
	must(os.WriteFile(filepath.Join(output, "result.json"), raw, 0600))
	fmt.Println(string(raw))
}
