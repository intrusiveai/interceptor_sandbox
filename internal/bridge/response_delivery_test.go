package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evaluation"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/services"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

type deliveryConnector func(ModelFrame) (ModelResult, error)

func (f deliveryConnector) Forward(_ context.Context, frame ModelFrame) (ModelResult, error) {
	return f(frame)
}

func responseDefinition(surface string) injection.Definition {
	pointer, tool := "/description", ""
	if surface == "mcp_tool_result" {
		pointer, tool = "/content/0/text", "search"
	}
	return injection.Definition{ID: "ingress", Surface: surface, Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{Service: "docs", ToolName: tool, CallOrdinal: "first"}, Placement: injection.Placement{Operation: "append_text", Pointer: pointer}, Payload: json.RawMessage(`" injected-marker"`), AttemptID: "attempt-1", PayloadDigest: digest.Bytes([]byte("payload"))}
}

func applyResponse(t *testing.T, manager *injection.Manager, surface string) string {
	t.Helper()
	kind := "fixture_http"
	if surface == "mcp_tool_result" {
		kind = "mcp_http"
	}
	handler := services.WithResponseInjection("docs", kind, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kind == "mcp_http" {
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"safe"}]}}`)
		} else {
			fmt.Fprint(w, `{"description":"safe"}`)
		}
	}), manager, nil)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/lookup", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{}}}`))
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "injected-marker") {
		t.Fatalf("real response was not rewritten: %d %s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

func modelDeliveryRequest(codec, content string) (string, string) {
	var body any
	path := ""
	switch codec {
	case CodecOpenAIChat:
		path = "/v1/chat/completions"
		body = map[string]any{"model": "safe-model", "messages": []any{map[string]any{"role": "user", "content": content}}}
	case CodecOpenAIResponses:
		path = "/v1/responses"
		body = map[string]any{"model": "safe-model", "input": content}
	case CodecBedrockConverse:
		path = "/model/safe-model/converse"
		body = map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"text": content}}}}}
	case CodecAnthropicMessages:
		path = "/v1/messages"
		body = map[string]any{"model": "safe-model", "max_tokens": 100, "messages": []any{map[string]any{"role": "user", "content": content}}}
	case CodecGoogleGenerateContent:
		path = "/v1beta/models/safe-model:generateContent"
		body = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": content}}}}}
	}
	data, _ := json.Marshal(body)
	return path, string(data)
}

func TestModelProxyConfirmsAppliedResponseDeliveryAcrossCodecs(t *testing.T) {
	for _, surface := range []string{"service_response", "mcp_tool_result"} {
		for _, codec := range []string{CodecOpenAIChat, CodecOpenAIResponses, CodecBedrockConverse, CodecAnthropicMessages, CodecGoogleGenerateContent} {
			t.Run(surface+"/"+codec, func(t *testing.T) {
				directory := t.TempDir()
				journal, err := statejournal.Open(filepath.Join(directory, "state.jsonl"), "sess-test")
				if err != nil {
					t.Fatal(err)
				}
				defer journal.Close()
				recorder, err := recording.Open(filepath.Join(directory, "events.jsonl"), "sess-test")
				if err != nil {
					t.Fatal(err)
				}
				defer recorder.Close()
				manager := injection.NewManager(journal, recorder)
				if err = manager.Arm(responseDefinition(surface), ""); err != nil {
					t.Fatal(err)
				}
				content := applyResponse(t, manager, surface)
				before := manager.Evaluation()[0]
				if before.Delivery != "not_confirmed" || !before.Fired || !before.Consumed || before.Armed {
					t.Fatalf("rewriting alone established delivery or failed once consumption: %#v", before)
				}
				connector := &captureConnector{}
				proxy, err := NewModelProxy("sess-test", connector, manager, nil, recorder, 1<<20, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				// Application may read a poisoned response but not include it in a
				// model request. That is still not provider delivery.
				path, body := modelDeliveryRequest(codec, "unrelated safe content")
				response := httptest.NewRecorder()
				proxy.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
				if response.Code != 200 || manager.Evaluation()[0].Delivery != "not_confirmed" {
					t.Fatalf("absent payload qualified: %d %#v", response.Code, manager.Evaluation())
				}
				path, body = modelDeliveryRequest(codec, content)
				response = httptest.NewRecorder()
				proxy.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
				if response.Code != 200 || manager.Evaluation()[0].Delivery != "confirmed" {
					t.Fatalf("response delivery not confirmed: %d %s %#v", response.Code, response.Body.String(), manager.Evaluation())
				}
				if string(connector.frame.Body) != body {
					t.Fatal("delivery check modified the native request")
				}
				if err = recorder.Flush(); err != nil {
					t.Fatal(err)
				}
				events, err := recording.VerifyFile(filepath.Join(directory, "events.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				last := events[len(events)-1]
				if last.Kind != "model_exchange" || last.AttemptID != "attempt-1" || last.PayloadDigest != responseDefinition(surface).PayloadDigest {
					t.Fatalf("confirmed exchange lost attempt identity: %#v", last)
				}
				// Evaluation must consume the actual recorded confirmation, not
				// a synthetic pass label. Event oracle keeps causal identity.
				report, err := evaluation.Evaluate(evaluation.Input{Blueprint: &environment.Blueprint{Oracles: []environment.Oracle{{ID: "seen", Kind: "event_match", Match: map[string]any{"kind": "model_exchange", "attempt_id": "attempt-1"}}}}, Events: events, Injections: manager.OperatorEvaluation()})
				if err != nil || len(report.Injections) != 1 || report.Injections[0].Outcome != "DELIVERED_SUCCEEDED" {
					t.Fatalf("incorrect final report: %#v %v", report, err)
				}
			})
		}
	}
}

func TestModelProxyResponseDeliveryRejectsUnprovenCandidates(t *testing.T) {
	for _, mode := range []string{"unapplied", "failed-placement", "provider-error", "provider-removed-content", "applied-during-forward", "journal-error"} {
		t.Run(mode, func(t *testing.T) {
			journal, err := statejournal.Open(filepath.Join(t.TempDir(), "state.jsonl"), "sess-test")
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			manager := injection.NewManager(journal, nil)
			definition := responseDefinition("service_response")
			if mode == "failed-placement" {
				definition.Placement.Pointer = "/missing"
			}
			if err = manager.Arm(definition, ""); err != nil {
				t.Fatal(err)
			}
			if mode == "failed-placement" {
				_, results, err := manager.RewriteResponse("service_response", "docs", "", injection.ResponseContext{}, map[string]any{"description": "safe"})
				if err != nil || len(results) != 1 || results[0].Applied {
					t.Fatalf("expected rejected placement: %#v %v", results, err)
				}
			} else if mode != "unapplied" && mode != "applied-during-forward" {
				applyResponse(t, manager, "service_response")
			}
			connector := deliveryConnector(func(frame ModelFrame) (ModelResult, error) {
				if mode == "provider-error" {
					return ModelResult{}, errors.New("fixture provider failed")
				}
				if mode == "applied-during-forward" {
					applyResponse(t, manager, "service_response")
				}
				if mode == "journal-error" {
					if err := journal.Close(); err != nil {
						t.Fatal(err)
					}
				}
				final := frame.Body
				if mode == "provider-removed-content" {
					final = []byte(`{"messages":[]}`)
				}
				return ModelResult{Status: 200, Body: []byte(`{"choices":[]}`), FinalRequest: final}, nil
			})
			proxy, _ := NewModelProxy("sess-test", connector, manager, nil, nil, 1<<20, time.Second)
			path, body := modelDeliveryRequest(CodecOpenAIChat, " injected-marker")
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
			if mode == "journal-error" {
				if response.Code != 500 {
					t.Fatalf("undurable confirmation did not fail closed: %d", response.Code)
				}
			} else if manager.Evaluation()[0].Delivery != "not_confirmed" {
				t.Fatalf("unproven delivery qualified: %#v", manager.Evaluation())
			}
			if mode == "provider-error" && response.Code != 502 {
				t.Fatal(response.Code)
			}
		})
	}
}
