package bridge

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

// Exercise the native HTTP adapter, host connector, application proxy, journal
// and retained blobs together. The provider transport is local and deterministic.
func TestAzureFilteringEvidencePreservesNativeExchange(t *testing.T) {
	tests := []struct {
		name, route, body, finish, safety string
		status                            int
	}{
		{"chat safe", "chat/completions",
			` {"choices":[{"index":0,"finish_reason":"stop","message":{"content":"ready"},"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}}}],"prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"jailbreak":{"detected":false,"filtered":false}}}],"routing":{"serving_pipereplica":"fixture"},"usage":{"total_tokens":4,"latency_checkpoint":{"engine_tbt_ms":0.5}}} `,
			`"stop"`, `{"prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"jailbreak":{"detected":false,"filtered":false}}}],"choices":[{"index":0,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}}}]}`, 200},
		{"chat filtered tool still forwarded", "chat/completions",
			`{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"content_filter_results":{"hate":{"filtered":true,"severity":"high"}}}],"usage":{"total_tokens":4}}`,
			`"tool_calls"`, `{"choices":[{"index":0,"content_filter_results":{"hate":{"filtered":true,"severity":"high"}}}]}`, 200},
		{"chat detected only and multiple choices", "chat/completions",
			`{"choices":[{"index":2,"finish_reason":"stop","content_filter_results":{"jailbreak":{"detected":true,"filtered":false}}},{"index":0,"finish_reason":"content_filter","content_filter_results":{"future_category":{"filtered":true,"detail":"preserved"}}},{"index":1,"finish_reason":"stop"}],"usage":{"total_tokens":4}}`,
			`"stop"`, `{"choices":[{"index":2,"content_filter_results":{"jailbreak":{"detected":true,"filtered":false}}},{"index":0,"content_filter_results":{"future_category":{"filtered":true,"detail":"preserved"}}}]}`, 200},
		{"chat prompt only", "chat/completions",
			`{"choices":[],"prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"jailbreak":{"detected":true,"filtered":true}}}],"usage":{"total_tokens":4}}`,
			`null`, `{"prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"jailbreak":{"detected":true,"filtered":true}}}]}`, 200},
		{"chat absent annotations", "chat/completions",
			`{"choices":[{"finish_reason":"stop"}],"usage":{"total_tokens":4}}`, `"stop"`, `null`, 200},
		{"chat empty and null annotations", "chat/completions",
			`{"choices":[{"finish_reason":"content_filter","content_filter_results":null}],"prompt_filter_results":[],"usage":{"total_tokens":4}}`,
			`"content_filter"`, `{"prompt_filter_results":[],"choices":[{"content_filter_results":null}]}`, 200},
		{"responses safe", "responses",
			`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ready"}]}],"incomplete_details":null,"content_filters":[{"source_type":"prompt","blocked":false,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}},"content_filter_offsets":{"start_offset":0,"end_offset":5,"check_offset":5},"content_filter_raw":[]}],"usage":{"total_tokens":4}}`,
			`"completed"`, `{"content_filters":[{"source_type":"prompt","blocked":false,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}},"content_filter_offsets":{"start_offset":0,"end_offset":5,"check_offset":5},"content_filter_raw":[]}]}`, 200},
		{"responses blocked tool still forwarded", "responses",
			`{"status":"completed","output":[{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"}],"content_filters":[{"source_type":"completion","blocked":true,"content_filter_results":{"protected_material_text":{"detected":true,"filtered":true}}}],"usage":{"total_tokens":4}}`,
			`"completed"`, `{"content_filters":[{"source_type":"completion","blocked":true,"content_filter_results":{"protected_material_text":{"detected":true,"filtered":true}}}]}`, 200},
		{"responses incomplete and filters", "responses",
			`{"status":"incomplete","incomplete_details":{"reason":"content_filter"},"content_filters":[{"source_type":"completion","blocked":true,"content_filter_results":{}}],"usage":{"total_tokens":4}}`,
			`"incomplete"`, `{"incomplete_details":{"reason":"content_filter"},"content_filters":[{"source_type":"completion","blocked":true,"content_filter_results":{}}]}`, 200},
		{"responses legacy incomplete", "responses",
			`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"total_tokens":4}}`,
			`"incomplete"`, `{"reason":"max_output_tokens"}`, 200},
		{"responses absent annotations", "responses",
			`{"status":"completed","incomplete_details":null,"usage":{"total_tokens":4}}`, `"completed"`, `null`, 200},
		{"responses empty annotations", "responses",
			`{"status":"completed","content_filters":[],"usage":{"total_tokens":4}}`, `"completed"`, `{"content_filters":[]}`, 200},
		{"chat rejected input", "chat/completions",
			`{"error":{"code":"content_filter","message":"fixture rejected","innererror":{"content_filter_result":{"jailbreak":{"detected":true,"filtered":true}}}}}`, `null`, `null`, 400},
		{"responses rejected input", "responses",
			`{"error":{"code":"content_filter","message":"fixture rejected"}}`, `null`, `null`, 400},
		{"responses rate limit", "responses",
			`{"error":{"code":"rate_limit_exceeded","message":"fixture limited"}}`, `null`, `null`, 429},
		{"non JSON failure", "chat/completions", "upstream unavailable\n", `null`, `null`, 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Path != "/openai/v1/"+tt.route || request.Header.Get("Api-Key") != "host-only-fixture" {
					t.Fatal("incorrect native route or host authentication")
				}
				return &http.Response{StatusCode: tt.status, Header: http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"fixture-request"}}, Body: io.NopCloser(strings.NewReader(tt.body)), Request: request}, nil
			})}
			provider, err := NewAzureOpenAIProvider(HTTPProviderConfig{BaseURL: "https://azure.example/openai/v1", APIKey: "host-only-fixture", Client: client})
			if err != nil {
				t.Fatal(err)
			}
			connector, err := NewHostConnector(HostConnectorConfig{SessionID: "session-fixture", Provider: provider, AllowedModel: []string{"deployment-fixture"}, MaxBodyBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			journalPath := filepath.Join(root, "events.jsonl")
			recorder, err := recording.Open(journalPath, "session-fixture")
			if err != nil {
				t.Fatal(err)
			}
			defer recorder.Close()
			blobs, err := blobstore.Open(filepath.Join(root, "blobs"), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			proxy, err := NewModelProxy("session-fixture", connector, nil, blobs, recorder, 1<<20, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			tokens := int64(0)
			proxy.SetTokenObserver(func(n int64) { tokens += n })
			body := `{"model":"deployment-fixture","messages":[]}`
			if tt.route == "responses" {
				body = `{"model":"deployment-fixture","input":"hello"}`
			}
			result := httptest.NewRecorder()
			proxy.ServeHTTP(result, httptest.NewRequest(http.MethodPost, "/v1/"+tt.route, strings.NewReader(body)))
			if calls != 1 || result.Code != tt.status || result.Body.String() != tt.body || result.Header().Get("X-Request-ID") != "fixture-request" {
				t.Fatalf("native response changed or retried: calls=%d status=%d body=%q", calls, result.Code, result.Body.String())
			}
			if tt.status == 200 && tokens != 4 || tt.status != 200 && tokens != 0 {
				t.Fatalf("token accounting changed: %d", tokens)
			}
			if err := recorder.Flush(); err != nil {
				t.Fatal(err)
			}
			events, err := recording.VerifyFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			exchanges := 0
			for _, event := range events {
				if event.Kind != "model_exchange" {
					continue
				}
				exchanges++
				var semantic map[string]json.RawMessage
				if err := json.Unmarshal(event.Semantic, &semantic); err != nil {
					t.Fatal(err)
				}
				for key, want := range map[string]string{"finish_status": tt.finish, "safety_status": tt.safety} {
					var gotValue, wantValue any
					if err := json.Unmarshal(semantic[key], &gotValue); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(gotValue, wantValue) {
						t.Fatalf("%s: got %s, want %s", key, semantic[key], want)
					}
				}
				for _, key := range []string{"response_ref", "provider_response_ref", "provider_response_digest"} {
					var ref string
					if err := json.Unmarshal(semantic[key], &ref); err != nil {
						t.Fatal(err)
					}
					if ref != digest.Bytes([]byte(tt.body)) {
						t.Fatalf("%s digest changed", key)
					}
					stored, _, err := blobs.Open(ref)
					if err != nil {
						t.Fatal(err)
					}
					raw, err := io.ReadAll(stored)
					stored.Close()
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(raw, []byte(tt.body)) {
						t.Fatal("retained native response changed")
					}
				}
				if strings.Contains(string(event.Semantic), "host-only-fixture") {
					t.Fatal("host credential leaked into evidence")
				}
			}
			if exchanges != 1 {
				t.Fatalf("got %d model exchange events", exchanges)
			}
		})
	}
}
