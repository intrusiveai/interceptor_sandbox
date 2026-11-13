package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/localapi"
)

func qualifyModelResponse(b localapi.Binding, output string, limit int64) {
	var manifest adaptive.CapabilityManifest
	must(json.Unmarshal(success(operation(b, "capabilities.read", struct{}{}, nil)), &manifest))
	found := false
	for _, p := range manifest.InjectionProfiles {
		if p.Surface == "model_response" {
			found = p.PlacementRoot == "response-body" && len(p.ModelCodecs) == 5
		}
	}
	require(found, "model_response discovery is missing")
	outputs := []map[string]any{}
	for index, tc := range []struct{ name, pointer, payload, want string }{
		{"baseline", "", `"baseline"`, ""},
		{"malformed-name", "/choices/0/message/tool_calls/0/function/name", `"lookup_order("`, "KeyError"},
		{"malformed-arguments", "/choices/0/message/tool_calls/0/function/arguments", `"{"`, "JSONDecodeError"},
	} {
		payload := []byte(tc.payload)
		descriptor := adaptive.ArtifactDescriptor{Digest: digest.Bytes(payload), SizeBytes: int64(len(payload)), MediaType: "application/json", Canonicalization: "raw"}
		success(operation(b, "artifact.register", map[string]any{"descriptor": descriptor, "content": payload}, nil))
		a := adaptive.AttemptContext{APIVersion: adaptive.AttemptContextVersion, CampaignID: campaign, ThreadID: "model-response", AttemptID: tc.name, Generation: 1, AttemptIndex: uint64(index + 1), Payload: descriptor, Generator: adaptive.Generator{Kind: "custom-harness"}, FeedbackProfile: "diagnostic", CreatedAt: time.Now().UTC()}
		success(operation(b, "attempt.register", a, &a))
		if tc.pointer != "" {
			d := injection.Definition{ID: tc.name, Surface: "model_response", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{ModelCodec: "openai_chat_completions", CallOrdinal: "first"}, Placement: injection.Placement{Operation: "replace", Pointer: tc.pointer}, Payload: payload, AttemptID: a.AttemptID, PayloadDigest: a.Payload.Digest}
			success(operation(b, "injection.arm", map[string]any{"definition": d}, &a))
		}
		raw := success(operation(b, "application.invoke", map[string]any{"operation": "ask", "input": []byte(`{"message":"Look up order-1"}`), "media_type": "application/json", "attempt_id": a.AttemptID, "payload_digest": a.Payload.Digest}, &a))
		var turn struct{ ID, Status string }
		must(json.Unmarshal(raw, &turn))
		require(turn.Status == "complete", string(raw))
		var view adaptive.ObservationView
		must(json.Unmarshal(success(operation(b, "observation.read", adaptive.ObservationRequest{TurnID: turn.ID}, &a)), &view))
		gotOutput, wrote := false, false
		for _, entry := range view.Entries {
			if entry.Availability != "available" {
				continue
			}
			var chunk adaptive.FeedbackChunk
			must(json.Unmarshal(success(operation(b, "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: view.ReceiptID, EntryID: entry.ID, MaxBytes: adaptive.MaxFeedbackChunk}, &a)), &chunk))
			var value map[string]any
			must(json.Unmarshal(chunk.Content, &value))
			if entry.Kind == "target_output" {
				if tc.want == "" {
					require(value["error"] == nil && value["result"] != nil, "benign dispatcher failed")
				} else {
					require(value["error"] == tc.want, fmt.Sprint(value))
				}
				if index == 1 {
					require(value["name"] == "lookup_order(", "SDK did not receive malformed name")
				}
				if index == 2 {
					require(value["arguments"] == "{", "SDK did not receive malformed arguments")
				}
				outputs = append(outputs, value)
				gotOutput = true
			}
			if entry.Kind == "injection_delivery" && value["state"] == "response_written" {
				require(value["surface"] == "model_response" && value["direction"] == "model_to_application", "feedback lost direction")
				wrote = true
			}
		}
		require(gotOutput, "missing target output")
		require(index == 0 || wrote, "missing response-write feedback")
	}
	success(post("/v1/lifecycle", localapi.LifecycleRequest{WorkerInstanceID: b.WorkerInstanceID, RunRevision: b.RunRevision, APIVersion: localapi.Version, CampaignID: campaign, SessionID: b.SessionID, OperationID: "stop-model-response", Operation: "session.stop"}))
	evidence(output, b.SessionID, limit)
	result := map[string]any{"passed": true, "session_id": b.SessionID, "target_output": "baseline shipped; malformed name KeyError; malformed arguments JSONDecodeError", "observations": outputs}
	raw, err := json.MarshalIndent(result, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(output, "result.json"), raw, 0600))
	fmt.Println(string(raw))
}
