package sandboxd

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"github.com/intrusive-ai/interceptor-sandbox/internal/contracttest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

type responseFixture struct{}

func (responseFixture) Forward(_ context.Context, f bridge.ModelFrame) (bridge.ModelResult, error) {
	return bridge.ModelResult{Status: 200, Body: []byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"lookup","arguments":"{}"}}]}}]}`), FinalRequest: f.Body}, nil
}
func TestNativeModelResponseInjection(t *testing.T) {
	for _, transport := range []string{"unix", "spool"} {
		t.Run(transport, func(t *testing.T) {
			s, _ := nativeOperationServer(t, transport)
			_, call := discoveryCampaign(t, s)
			result := call("capabilities.read", struct{}{}, nil)
			if result.Status != 200 {
				t.Fatal(result)
			}
			v, err := contracttest.New()
			if err != nil {
				t.Fatal(err)
			}
			if err := v.Validate("#/components/schemas/adaptive.CapabilityManifest", result.Body); err != nil {
				t.Fatal(err)
			}
			var manifest adaptive.CapabilityManifest
			if err := json.Unmarshal(result.Body, &manifest); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, profile := range manifest.InjectionProfiles {
				if profile.Surface == "model_response" {
					found = len(profile.ModelCodecs) == 5 && profile.PlacementRoot == "response-body"
				}
			}
			if !found {
				t.Fatal("missing model response discovery", string(result.Body))
			}
			a := discoveryAttempt(t, call, "response-attempt", `"lookup("`, 1)
			d := injection.Definition{ID: "model-response", Surface: "model_response", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{ModelCodec: bridge.CodecOpenAIChat, CallOrdinal: "first"}, Placement: injection.Placement{Operation: "replace", Pointer: "/choices/0/message/tool_calls/0/function/name"}, Payload: json.RawMessage(`"lookup("`), AttemptID: a.AttemptID, PayloadDigest: a.Payload.Digest}
			if r := call("injection.arm", injectionArm{Definition: d}, &a); r.Status != 201 {
				t.Fatal(r)
			}
			p, err := bridge.NewModelProxy(s.metadata.ID, responseFixture{}, s.injections, s.blobs, s.recorder, 1<<20, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[]}`)))
			if w.Code != 200 || !strings.Contains(w.Body.String(), "lookup(") {
				t.Fatal(w.Code, w.Body.String())
			}
			for _, codec := range []string{"openai_embeddings", "unknown"} {
				d.ID = codec
				d.Selector.ModelCodec = codec
				r := call("injection.arm", injectionArm{Definition: d}, &a)
				if r.Status != 400 || !strings.Contains(string(r.Body), "invalid_injection") {
					t.Fatal(r)
				}
			}
		})
	}
}

func TestModelResponseFeedbackVisibility(t *testing.T) {
	for _, profile := range []string{"black-box", "diagnostic", "oracle-assisted"} {
		t.Run(profile, func(t *testing.T) {
			s, g, a := feedbackFixture(t, profile, nil)
			now := time.Now().UTC()
			turn := Turn{ID: "turn-model", AttemptID: a.AttemptID, Status: "complete", Body: []byte("rejected invalid tool"), MediaType: "text/plain", Started: now.Add(-time.Second), Finished: now}
			s.turns[turn.ID] = turn
			for _, state := range []string{"response_written", "write_failed"} {
				_, err := s.recorder.Append(recording.Draft{Channel: recording.Evaluation, Layer: "injection", Kind: "model_response_write", Provenance: recording.HarnessInjected, AttemptID: a.AttemptID, InjectionID: state, TurnID: turn.ID, Semantic: map[string]any{"surface": "model_response", "direction": "model_to_application", "write_state": state}})
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err := s.recorder.Append(recording.Draft{Channel: recording.Attack, Layer: "application_bridge", Kind: "turn_completed", Provenance: recording.Application, AttemptID: a.AttemptID, TurnID: turn.ID, Semantic: map[string]any{}})
			if err != nil {
				t.Fatal(err)
			}
			v := collectFeedback(t, s, g, a, adaptive.ObservationRequest{TurnID: turn.ID})
			count := 0
			for _, entry := range v.Entries {
				if entry.Kind != "injection_delivery" {
					continue
				}
				count++
				r := nativeCall(t, g, s, "read-"+entry.ID, "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: v.ReceiptID, EntryID: entry.ID, MaxBytes: adaptive.MaxFeedbackChunk}, &a)
				if r.Status != 200 {
					t.Fatal(r)
				}
				var chunk adaptive.FeedbackChunk
				json.Unmarshal(r.Body, &chunk)
				var value map[string]any
				json.Unmarshal(chunk.Content, &value)
				if value["surface"] != "model_response" || value["direction"] != "model_to_application" {
					t.Fatal(string(chunk.Content))
				}
				validator, err := contracttest.New()
				if err != nil {
					t.Fatal(err)
				}
				if err := validator.Validate("#/components/schemas/ModelResponseDeliveryFact", chunk.Content); err != nil {
					t.Fatal(err)
				}
			}
			if (profile == "black-box" && count != 0) || (profile != "black-box" && count != 2) {
				t.Fatal(count)
			}
		})
	}
}
