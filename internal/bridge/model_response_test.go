package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

type modelResponseConnector struct {
	result ModelResult
	after  func()
}

func (c modelResponseConnector) Forward(_ context.Context, f ModelFrame) (ModelResult, error) {
	r := c.result
	r.FinalRequest = f.Body
	if c.after != nil {
		c.after()
	}
	return r, nil
}
func TestModelResponseNativeCodecs(t *testing.T) {
	for _, tc := range []struct{ codec, path, request, response, pointer string }{
		{CodecOpenAIChat, "/v1/chat/completions", `{"model":"test","messages":[]}`, `{"choices":[{"message":{"tool_calls":[{"function":{"name":"lookup","arguments":"{}"}}]}}],"usage":{"total_tokens":2}}`, "/choices/0/message/tool_calls/0/function/name"},
		{CodecOpenAIResponses, "/v1/responses", `{"model":"test","input":"hi"}`, `{"output":[{"type":"function_call","name":"lookup","arguments":"{}"}]}`, "/output/0/name"},
		{CodecBedrockConverse, "/model/test/converse", `{"messages":[]}`, `{"output":{"message":{"content":[{"toolUse":{"name":"lookup","input":{}}}]}}}`, "/output/message/content/0/toolUse/name"},
		{CodecAnthropicMessages, "/v1/messages", `{"model":"test","max_tokens":100,"messages":[]}`, `{"content":[{"type":"tool_use","name":"lookup","input":{}}]}`, "/content/0/name"},
		{CodecGoogleGenerateContent, "/v1beta/models/test:generateContent", `{"contents":[]}`, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]}}]}`, "/candidates/0/content/parts/0/functionCall/name"},
	} {
		t.Run(tc.codec, func(t *testing.T) {
			dir := t.TempDir()
			journal, err := statejournal.Open(filepath.Join(dir, "state.jsonl"), "sess-test")
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			recorder, err := recording.Open(filepath.Join(dir, "events.jsonl"), "sess-test")
			if err != nil {
				t.Fatal(err)
			}
			defer recorder.Close()
			blobs, err := blobstore.Open(filepath.Join(dir, "blobs"), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			m := injection.NewManager(journal, recorder)
			d := injection.Definition{ID: "bad-name", Surface: "model_response", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{ModelCodec: tc.codec, CallOrdinal: "first"}, Placement: injection.Placement{Operation: "replace", Pointer: tc.pointer}, Payload: json.RawMessage(`"attempt_execute("`)}
			if err := m.Arm(d, ""); err != nil {
				t.Fatal(err)
			}
			connector := modelResponseConnector{result: ModelResult{Status: 200, Body: []byte(tc.response), ProviderResponse: []byte(tc.response), Headers: map[string]string{"Content-Length": "1", "ETag": "old", "Content-Type": "application/json"}}}
			p, err := NewModelProxy("sess-test", connector, m, blobs, recorder, 1<<20, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			tokens := int64(0)
			p.SetTokenObserver(func(n int64) { tokens += n })
			w := httptest.NewRecorder()
			p.ServeHTTP(w, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.request)))
			if w.Code != 200 || !strings.Contains(w.Body.String(), "attempt_execute(") || w.Header().Get("ETag") != "" || w.Header().Get("Content-Length") != "" {
				t.Fatal(w.Code, w.Body.String(), w.Header())
			}
			if tc.codec == CodecOpenAIChat && tokens != 2 {
				t.Fatal(tokens)
			}
			events, err := recording.VerifyFile(filepath.Join(dir, "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range events {
				if e.Kind == "model_exchange" {
					found = true
					var sem map[string]any
					json.Unmarshal(e.Semantic, &sem)
					if sem["response_ref"] != digest.Bytes([]byte(tc.response)) || sem["delivered_response_digest"] != digest.Bytes(w.Body.Bytes()) {
						t.Fatal(sem)
					}
					refs, err := recording.References(e)
					if err != nil {
						t.Fatal(err)
					}
					for _, ref := range refs {
						if err := blobs.Verify(ref); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if !found || m.Evaluation()[0].Delivery != "response_written" {
				t.Fatal(events, m.Evaluation())
			}
			// A consumed once injection preserves the exact original response bytes.
			w = httptest.NewRecorder()
			p.ServeHTTP(w, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.request)))
			if w.Body.String() != tc.response {
				t.Fatal(w.Body.String())
			}
			// Each native codec also permits malformed arguments, including a wrong
			// object type in the structured-arguments codecs.
			d.ID = "bad-arguments"
			argumentField := map[string]string{CodecOpenAIChat: "arguments", CodecOpenAIResponses: "arguments", CodecBedrockConverse: "input", CodecAnthropicMessages: "input", CodecGoogleGenerateContent: "args"}[tc.codec]
			d.Placement.Pointer = strings.TrimSuffix(tc.pointer, "name") + argumentField
			d.Payload = json.RawMessage(`"{"`)
			if argumentField != "arguments" {
				d.Payload = json.RawMessage(`7`)
			}
			if err := m.Arm(d, ""); err != nil {
				t.Fatal(err)
			}
			w = httptest.NewRecorder()
			p.ServeHTTP(w, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.request)))
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"`+argumentField+`":`+string(d.Payload)) {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

type failedResponseWriter struct{ http.ResponseWriter }

func (w failedResponseWriter) Write(p []byte) (int, error) { return 1, io.ErrClosedPipe }
func TestModelResponseFailuresAndAccounting(t *testing.T) {
	for _, failure := range []string{"write", "journal", "recorder", "blob", "oversize", "provider", "non-json", "stream", "inflight-arm", "inflight-delete", "usage", "post-write-storage"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			j, err := statejournal.Open(filepath.Join(dir, "state.jsonl"), "sess-test")
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			r, err := recording.Open(filepath.Join(dir, "events.jsonl"), "sess-test")
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			b, err := blobstore.Open(filepath.Join(dir, "blobs"), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			m := injection.NewManager(j, r)
			d := injection.Definition{ID: "response", Surface: "model_response", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{ModelCodec: CodecOpenAIChat, CallOrdinal: "first"}, Placement: injection.Placement{Operation: "replace"}, Payload: json.RawMessage(`{"usage":{"total_tokens":99999},"injected":true}`)}
			if failure != "inflight-arm" {
				if err := m.Arm(d, ""); err != nil {
					t.Fatal(err)
				}
			}
			c := modelResponseConnector{result: ModelResult{Status: 200, Body: []byte(`{"usage":{"total_tokens":2}}`)}}
			switch failure {
			case "journal":
				c.after = func() { j.Close() }
			case "recorder":
				c.after = func() { r.Fail(errors.New("disk full")) }
			case "blob":
				b, err = blobstore.Open(filepath.Join(dir, "small"), 40)
				if err != nil {
					t.Fatal(err)
				}
			case "provider":
				c.result.Status = 503
			case "non-json":
				c.result.Body = []byte("bad JSON")
			case "inflight-arm":
				c.after = func() {
					if err := m.Arm(d, ""); err != nil {
						t.Fatal(err)
					}
				}
			case "inflight-delete":
				c.after = func() {
					if err := m.Delete(d.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			limit := int64(1 << 20)
			if failure == "oversize" {
				limit = 30
			}
			p, err := NewModelProxy("sess-test", c, m, b, r, limit, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			tokens := int64(0)
			p.SetTokenObserver(func(n int64) { tokens += n })
			body := `{"messages":[]}`
			if failure == "stream" {
				body = `{"stream":true}`
			}
			w := httptest.NewRecorder()
			var out http.ResponseWriter = w
			if failure == "write" {
				out = failedResponseWriter{w}
			}
			if failure == "post-write-storage" {
				out = afterResponseWriter{w, func() { r.Fail(errors.New("post-write disk failure")) }}
			}
			p.ServeHTTP(out, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
			switch failure {
			case "post-write-storage":
				if w.Code != 200 || r.Err() == nil {
					t.Fatal("sent bytes require incomplete evidence after late failure", w.Code, r.Err())
				}
			case "write":
				if m.Evaluation()[0].Delivery != "write_failed" {
					t.Fatal(m.Evaluation())
				}
			case "journal", "recorder", "blob", "oversize":
				if w.Code != 507 || strings.Contains(w.Body.String(), `"injected":true`) {
					t.Fatal(w.Code, w.Body.String())
				}
			case "stream":
				if w.Code != 400 || m.Evaluation()[0].Consumed {
					t.Fatal(w.Code, m.Evaluation())
				}
			case "provider", "non-json", "inflight-arm", "inflight-delete":
				if w.Body.String() != string(c.result.Body) {
					t.Fatal(w.Code, w.Body.String())
				}
			case "usage":
				if tokens != 2 || !strings.Contains(w.Body.String(), "99999") {
					t.Fatal(tokens, w.Body.String())
				}
			}
		})
	}
}

func TestModelResponseEmbeddingsExcluded(t *testing.T) {
	j, err := statejournal.Open(filepath.Join(t.TempDir(), "state.jsonl"), "sess-test")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	m := injection.NewManager(j, nil)
	if err := m.Arm(injection.Definition{ID: "generation", Surface: "model_response", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{ModelCodec: CodecOpenAIChat, CallOrdinal: "first"}, Placement: injection.Placement{Operation: "replace"}, Payload: json.RawMessage(`null`)}, ""); err != nil {
		t.Fatal(err)
	}
	response := []byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"embedding","usage":{"prompt_tokens":1,"total_tokens":1}}`)
	p, err := NewModelProxy("sess-test", modelResponseConnector{result: ModelResult{Status: 200, Body: response}}, m, nil, nil, 1<<20, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"embedding","input":"hello","encoding_format":"float"}`)))
	if w.Code != 200 || w.Body.String() != string(response) || m.Evaluation()[0].Consumed {
		t.Fatal(w.Code, w.Body.String(), m.Evaluation())
	}
}

// Inject a required evidence failure only after the application-facing writer
// has accepted bytes, exercising the irreversible/partial-evidence boundary.
type afterResponseWriter struct {
	http.ResponseWriter
	after func()
}

func (w afterResponseWriter) Write(body []byte) (int, error) {
	n, err := w.ResponseWriter.Write(body)
	w.after()
	return n, err
}
