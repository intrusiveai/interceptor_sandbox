package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

const embeddingResponse = `{"object":"list","model":"reported-model","data":[{"object":"embedding","index":0,"embedding":[0.25,-0.5]}],"usage":{"prompt_tokens":3,"total_tokens":3}}`

func TestEmbeddingRequestContract(t *testing.T) {
	for _, input := range []string{`"hello"`, `["hello","world"]`, `[12,34]`, `[[12,34],[56]]`} {
		body := []byte(`{"model":"embed","input":` + input + `,"dimensions":2,"encoding_format":"base64","user":"test"}`)
		if _, err := validateEmbeddingRequest(body); err != nil {
			t.Fatalf("%s: %v", input, err)
		}
	}
	for _, input := range []string{`""`, `[]`, `[""]`, `[1,"x"]`, `[[1],[]]`, `[-1]`, `[1.1]`, `null`, `{}`, `[true]`, `[2147483648]`} {
		if _, err := validateEmbeddingRequest([]byte(`{"model":"embed","input":` + input + `}`)); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
	for _, extra := range []string{`,"dimensions":0`, `,"dimensions":1.5`, `,"dimensions":"2"`, `,"encoding_format":null`, `,"user":1`, `,"stream":false`, `,"extra":"x"`} {
		if _, err := validateEmbeddingRequest([]byte(`{"model":"embed","input":"x"` + extra + `}`)); err == nil {
			t.Errorf("accepted %s", extra)
		}
	}
	for _, count := range []int{128, 129} {
		body, _ := json.Marshal(map[string]any{"model": "embed", "input": strings.Split(strings.Repeat("x,", count-1)+"x", ",")})
		_, err := validateEmbeddingRequest(body)
		if (err == nil) != (count == 128) {
			t.Fatalf("batch %d: %v", count, err)
		}
	}
}

func TestEmbeddingResponseContract(t *testing.T) {
	req := []byte(`{"model":"deployment-alias","input":"hi","dimensions":2}`)
	for _, response := range []string{embeddingResponse, strings.Replace(embeddingResponse, `[0.25,-0.5]`, `"AACAPgAAAL8="`, 1)} {
		metadata, err := embeddingResponseMetadata(req, []byte(response))
		if err != nil || metadata["dimensions"] != 2 || metadata["reported_model"] != "reported-model" {
			t.Fatalf("%v %v", metadata, err)
		}
	}
	for _, response := range []string{
		strings.Replace(embeddingResponse, `"index":0`, `"index":1`, 1),
		strings.Replace(embeddingResponse, `[0.25,-0.5]`, `[]`, 1),
		strings.Replace(embeddingResponse, `[0.25,-0.5]`, `[0.25]`, 1),
		strings.Replace(embeddingResponse, `[0.25,-0.5]`, `["bad",0]`, 1),
		strings.Replace(embeddingResponse, `[0.25,-0.5]`, `"invalid"`, 1),
		strings.Replace(embeddingResponse, `[0.25,-0.5]`, `"AACAfwAAAL8="`, 1),
		strings.Replace(embeddingResponse, `"total_tokens":3`, `"total_tokens":4`, 1),
		strings.Replace(embeddingResponse, `"data":[`, `"data":[{"object":"embedding","index":0,"embedding":[1,2]},`, 1),
		`null`, `{"error":{}}`,
	} {
		if _, err := embeddingResponseMetadata(req, []byte(response)); err == nil {
			t.Errorf("accepted %s", response)
		}
	}
}

func TestEmbeddingRoutingCredentialsEvidenceAndErrors(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "separate", true: "same"}[shared], func(t *testing.T) {
			calls := map[string]int{}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls[r.URL.Path]++
				key, host, body := "Bearer generation-secret", "generation.example", `{"choices":[],"usage":{"total_tokens":4}}`
				status := 200
				if r.URL.Path == "/v1/embeddings" {
					key, host, body = "Bearer embedding-secret", "embedding.example", embeddingResponse
				}
				if shared {
					host = "shared.example"
				}
				if r.Header.Get("Authorization") != key || r.URL.Host != host {
					t.Errorf("wrong upstream/auth: %s %s", r.URL, r.Header.Get("Authorization"))
				}
				raw, _ := io.ReadAll(r.Body)
				if strings.Contains(string(raw), "rate-limit") {
					status, body = 429, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`
				}
				if strings.Contains(string(raw), "slow") {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"2"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			genURL, embURL := "https://generation.example/v1", "https://embedding.example/v1"
			if shared {
				genURL, embURL = "https://shared.example/v1", "https://shared.example/v1"
			}
			gen, _ := NewOpenAIChatProvider(HTTPProviderConfig{BaseURL: genURL, APIKey: "generation-secret", Client: client})
			emb, _ := NewOpenAIEmbeddingsProvider(HTTPProviderConfig{BaseURL: embURL, APIKey: "embedding-secret", Client: client})
			connector, err := NewHostConnector(HostConnectorConfig{SessionID: "test", Provider: gen, AllowedModel: []string{"chat"}, EmbeddingsProvider: emb, AllowedEmbeddingModels: []string{"embed"}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "events.jsonl")
			recorder, err := recording.Open(path, "test")
			if err != nil {
				t.Fatal(err)
			}
			defer recorder.Close()
			proxy, _ := NewModelProxy("test", connector, nil, nil, recorder, 16<<20, 20*time.Millisecond)
			tokens := int64(0)
			proxy.SetTokenObserver(func(n int64) { tokens += n })
			call := func(route, body string, want int) *httptest.ResponseRecorder {
				t.Helper()
				w := httptest.NewRecorder()
				r := httptest.NewRequest("POST", route, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer application-key")
				proxy.ServeHTTP(w, r)
				if w.Code != want {
					t.Fatalf("%s: %d %s", route, w.Code, w.Body.String())
				}
				return w
			}
			call("/v1/embeddings", `{"model":"embed","input":"hello"}`, 200)
			call("/v1/chat/completions", `{"model":"chat","messages":[]}`, 200)
			if tokens != 7 {
				t.Fatalf("tokens=%d", tokens)
			}
			for _, body := range []string{`{"model":"chat","input":"hi"}`, `{"model":"embed","input":[]}`} {
				want := 502
				if strings.Contains(body, "[]") {
					want = 400
				}
				call("/v1/embeddings", body, want)
			}
			call("/arbitrary/embeddings", `{}`, 501)
			call("/v1/embeddings?url=https://evil.example", `{}`, 400)
			if calls["/v1/embeddings"] != 1 {
				t.Fatal(calls)
			}
			w := call("/v1/embeddings", `{"model":"embed","input":"rate-limit"}`, 429)
			if w.Header().Get("Retry-After") != "2" || !strings.Contains(w.Body.String(), "rate_limit_error") {
				t.Fatal(w)
			}
			call("/v1/embeddings", `{"model":"embed","input":"slow"}`, 502)
			if tokens != 7 {
				t.Fatal("error counted tokens")
			}
			if err := recorder.Flush(); err != nil {
				t.Fatal(err)
			}
			events, err := recording.VerifyFile(path)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(events)
			if strings.Contains(string(encoded), "secret") || !strings.Contains(string(encoded), `"reported_model":"reported-model"`) || !strings.Contains(string(encoded), `"input_count":1`) {
				t.Fatalf("evidence: %s", encoded)
			}
			// Explicitly disabled embeddings must never fall back to generation.
			connector.config.EmbeddingsProvider = nil
			call("/v1/embeddings", `{"model":"embed","input":"hello"}`, 502)
			if calls["/v1/chat/completions"] != 1 {
				t.Fatal(calls)
			}
		})
	}
}

func TestEmbeddingsDoNotConfirmGenerationDelivery(t *testing.T) {
	journal, err := statejournal.Open(filepath.Join(t.TempDir(), "state.jsonl"), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	manager := injection.NewManager(journal, nil)
	if err := manager.Arm(responseDefinition("service_response"), ""); err != nil {
		t.Fatal(err)
	}
	content := applyResponse(t, manager, "service_response")
	before, _ := json.Marshal(manager.Evaluation())
	connector := deliveryConnector(func(f ModelFrame) (ModelResult, error) {
		body := embeddingResponse
		if f.Codec != CodecOpenAIEmbeddings {
			body = `{"choices":[]}`
		}
		return ModelResult{Status: 200, Body: []byte(body), FinalRequest: f.Body}, nil
	})
	proxy, _ := NewModelProxy("test", connector, manager, nil, nil, 1<<20, time.Second)
	body, _ := json.Marshal(map[string]any{"model": "embed", "input": content})
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(string(body))))
	after, _ := json.Marshal(manager.Evaluation())
	if w.Code != 200 || string(before) != string(after) {
		t.Fatalf("embedding altered injection: %s -> %s (%s)", before, after, w.Body)
	}
	path, chat := modelDeliveryRequest(CodecOpenAIChat, content)
	w = httptest.NewRecorder()
	proxy.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(chat)))
	if w.Code != 200 || manager.Evaluation()[0].Delivery != "confirmed" {
		t.Fatalf("generation lost confirmation: %s", w.Body)
	}
}

func TestEmbeddingEffectiveEnvelopeLimit(t *testing.T) {
	// Even worst-case duplication stays under the existing serialized frame cap.
	body := []byte(strings.Repeat("x", int(maxEmbeddingBodyBytes)))
	result := ModelResult{Body: body, FinalRequest: body, ProviderRequest: body, ProviderResponse: body}
	encoded, _ := json.Marshal(result)
	if len(encoded) >= maxConnectorFrameBytes {
		t.Fatal("embedding body limits exceed bridge envelope")
	}
	huge := append(body, 'x')
	if _, err := validateEmbeddingRequest(huge); err == nil {
		t.Fatal("oversize request accepted")
	}
	provider, _ := NewOpenAIEmbeddingsProvider(HTTPProviderConfig{BaseURL: "https://test/v1", Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(huge))), Header: http.Header{}}, nil
	})}})
	_, err := provider.Complete(context.Background(), ProviderRequest{Codec: CodecOpenAIEmbeddings, Body: []byte(`{}`), MaxBodyBytes: maxEmbeddingBodyBytes})
	if err == nil {
		t.Fatal("oversize response accepted")
	}
}

func TestEmbeddingEvidenceStorageFailureIsVisible(t *testing.T) {
	for _, responseFailure := range []bool{false, true} {
		limit := int64(1)
		if responseFailure {
			limit = 80
		}
		store, err := blobstore.Open(filepath.Join(t.TempDir(), "blobs"), limit)
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		connector := deliveryConnector(func(f ModelFrame) (ModelResult, error) {
			calls++
			return ModelResult{Status: 200, Body: []byte(embeddingResponse), FinalRequest: f.Body, ProviderRequest: f.Body, ProviderResponse: []byte(embeddingResponse)}, nil
		})
		proxy, _ := NewModelProxy("test", connector, nil, store, nil, 1<<20, time.Second)
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"embed","input":"hi"}`)))
		if w.Code != 507 || (!responseFailure && calls != 0) {
			t.Fatalf("storage failure: %d %s calls=%d", w.Code, w.Body, calls)
		}
	}
}

func TestEmbeddingProviderRejectsOriginEscape(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			t.Fatal("credential followed a cross-origin redirect")
		}
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": []string{"https://other.example/v1/embeddings"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	provider, _ := NewOpenAIEmbeddingsProvider(HTTPProviderConfig{BaseURL: "https://embedding.example/v1", APIKey: "host-key", Client: client})
	_, err := provider.Complete(context.Background(), ProviderRequest{Codec: CodecOpenAIEmbeddings, Body: []byte(`{}`), MaxBodyBytes: 1024})
	if err == nil || calls != 1 {
		t.Fatal(err, calls)
	}
}
