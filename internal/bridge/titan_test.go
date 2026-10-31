package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

const titanPath = "/model/" + TitanEmbeddingModel + "/invoke"
const titanInput = `{"inputText":"hello","dimensions":256}`

func titanFixture(t *testing.T, request []byte) []byte {
	t.Helper()
	options, err := titanRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]any{}
	output := map[string]any{"inputTextTokenCount": 3, "embeddingsByType": byType}
	for _, kind := range options.types {
		vector := make([]float64, options.dimensions)
		vector[0] = 1
		byType[kind] = vector
		if kind == "float" {
			output["embedding"] = vector
		}
	}
	raw, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTitanNativeContract(t *testing.T) {
	for _, dimensions := range []int{256, 512, 1024} {
		for _, types := range [][]string{{"float"}, {"binary"}, {"float", "binary"}} {
			for _, normalize := range []bool{true, false} {
				request, _ := json.Marshal(map[string]any{"inputText": "你好", "dimensions": dimensions, "normalize": normalize, "embeddingTypes": types})
				response := titanFixture(t, request)
				metadata, err := titanResponseMetadata(request, response)
				if err != nil || metadata["dimensions"] != dimensions || metadata["normalize"] != normalize || metadata["reported_model"] != nil {
					t.Fatal(metadata, err)
				}
				_, usage := modelSemantics(CodecTitanEmbeddings, titanPath, request, response)
				if totalTokens(usage) != 3 {
					t.Fatal(usage)
				}
			}
		}
	}
	defaults, err := titanRequest([]byte(`{"inputText":"x"}`))
	if err != nil || defaults.dimensions != 1024 || !defaults.normalize || len(defaults.types) != 1 || defaults.types[0] != "float" {
		t.Fatal(defaults, err)
	}
	for _, count := range []int{50000, 50001} {
		request, _ := json.Marshal(map[string]string{"inputText": strings.Repeat("界", count)})
		_, err := titanRequest(request)
		if (err == nil) != (count == 50000) {
			t.Fatal(count, err)
		}
	}
	for _, body := range []string{`null`, `[]`, `{}`, `{"inputText":""}`, `{"inputText":[]}`, `{"inputText":"x","dimensions":257}`, `{"inputText":"x","dimensions":256.5}`, `{"inputText":"x","normalize":null}`, `{"inputText":"x","embeddingTypes":[]}`, `{"inputText":"x","embeddingTypes":["binary","binary"]}`, `{"inputText":"x","embeddingTypes":["base64"]}`, `{"inputText":"x","stream":false}`} {
		if _, err := titanRequest([]byte(body)); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	for _, mutate := range []func(map[string]any){
		func(o map[string]any) { delete(o, "embeddingsByType") },
		func(o map[string]any) { o["inputTextTokenCount"] = -1 },
		func(o map[string]any) { o["inputTextTokenCount"] = 1.5 },
		func(o map[string]any) { o["embedding"] = []float64{1} },
		func(o map[string]any) { o["embedding"].([]any)[0] = 2 },
		func(o map[string]any) { o["embeddingsByType"].(map[string]any)["float"].([]any)[0] = "NaN" },
		func(o map[string]any) { o["embeddingsByType"].(map[string]any)["binary"] = []int{0} },
	} {
		var output map[string]any
		_ = json.Unmarshal(titanFixture(t, []byte(titanInput)), &output)
		mutate(output)
		raw, _ := json.Marshal(output)
		if _, err := titanResponseMetadata([]byte(titanInput), raw); err == nil {
			t.Fatal("accepted malformed response")
		}
	}
	binary := []byte(`{"inputText":"x","dimensions":256,"embeddingTypes":["binary"]}`)
	for _, bad := range []string{"2", "-1", "0.5"} {
		raw := strings.Replace(string(titanFixture(t, binary)), `[1,`, `[`+bad+`,`, 1)
		if _, err := titanResponseMetadata(binary, []byte(raw)); err == nil {
			t.Fatal("accepted non-bit binary vector")
		}
	}
	for _, path := range []string{titanPath, strings.Replace(titanPath, ":", "%3A", 1)} {
		if model, err := titanModel(path); err != nil || model != TitanEmbeddingModel {
			t.Fatal(model, err)
		}
	}
	for _, path := range []string{"/model/invoke", "/prefix" + titanPath, titanPath + "/", strings.Replace(titanPath, ":", "%253A", 1), strings.Replace(titanPath, "/invoke", "/invoke-with-response-stream", 1), "/model/other/invoke", "/model/a%2Fb/invoke"} {
		if _, err := titanModel(path); err == nil {
			t.Fatal(path)
		}
	}
}

func TestTitanSDKTransportRoutingEvidence(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != titanPath || !strings.Contains(r.Header.Get("Authorization"), "Credential=HOSTKEY/") || r.Header.Get("X-Amz-Security-Token") != "host-session-secret" {
			t.Error("incorrect host routing/signing")
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("wrong content types")
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Amzn-Requestid", "fixture-request")
		if strings.Contains(string(body), "throttle") {
			w.Header().Set("X-Amzn-Errortype", "ThrottlingException")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"message":"host-session-secret"}`)
			return
		}
		_, _ = w.Write(titanFixture(t, body))
	}))
	defer server.Close()
	client := bedrockruntime.NewFromConfig(aws.Config{Region: "us-west-2", Credentials: awscredentials.NewStaticCredentialsProvider("HOSTKEY", "host-secret", "host-session-secret")}, func(o *bedrockruntime.Options) { o.BaseEndpoint = aws.String(server.URL) })
	generation, _ := NewOpenAIChatProvider(HTTPProviderConfig{BaseURL: "https://unused.example/v1"})
	connector, err := NewHostConnector(HostConnectorConfig{SessionID: "test", Provider: generation, AllowedModel: []string{"chat"}, EmbeddingsProvider: NewTitanProvider(client), AllowedEmbeddingModels: []string{TitanEmbeddingModel}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "events.jsonl")
	recorder, err := recording.Open(path, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	proxy, _ := NewModelProxy("test", connector, nil, nil, recorder, 1<<20, time.Second)
	tokens := int64(0)
	proxy.SetTokenObserver(func(n int64) { tokens += n })
	call := func(body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", titanPath, strings.NewReader(body))
		r.Header.Set("Authorization", "guest-secret")
		r.Header.Set("X-Amz-Security-Token", "guest-token")
		proxy.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%d: %s", w.Code, w.Body)
		}
		return w
	}
	response := call(titanInput, 200)
	if response.Body.String() != string(titanFixture(t, []byte(titanInput))) || response.Header().Get("X-Amzn-Requestid") != "fixture-request" {
		t.Fatal(response)
	}
	call(`{"inputText":"x","dimensions":256,"embeddingTypes":["binary"]}`, 200)
	response = call(`{"inputText":"throttle"}`, 429)
	if response.Header().Get("X-Amzn-Errortype") != "ThrottlingException" || strings.Contains(response.Body.String(), "secret") {
		t.Fatal(response)
	}
	call(`{"inputText":"x","dimensions":2}`, 400)
	connector.config.AllowedEmbeddingModels = []string{"other"}
	call(titanInput, 502)
	connector.config.EmbeddingsProvider = nil
	call(titanInput, 502)
	if calls != 3 || tokens != 6 {
		t.Fatal("retry, disabled route or accounting failure", calls, tokens)
	}
	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	events, err := recording.VerifyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), "secret") || !strings.Contains(string(raw), `"operation":"embeddings"`) || !strings.Contains(string(raw), `"request_id":"fixture-request"`) {
		t.Fatalf("unsafe/missing evidence %s", raw)
	}
}

type invokeFunc func(context.Context, *bedrockruntime.InvokeModelInput) (*bedrockruntime.InvokeModelOutput, error)

func (f invokeFunc) InvokeModel(ctx context.Context, in *bedrockruntime.InvokeModelInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error) {
	return f(ctx, in)
}

func TestTitanBoundsCancellationAndWrongProvider(t *testing.T) {
	calls := 0
	provider := NewTitanProvider(invokeFunc(func(ctx context.Context, in *bedrockruntime.InvokeModelInput) (*bedrockruntime.InvokeModelOutput, error) {
		calls++
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	request := ProviderRequest{Codec: CodecTitanEmbeddings, Model: TitanEmbeddingModel, Path: titanPath, Body: []byte(titanInput), MaxBodyBytes: 1 << 20}
	result, err := provider.Complete(ctx, request)
	if err != nil || result.Status != 504 || calls != 1 {
		t.Fatal(result, err, calls)
	}
	request.MaxBodyBytes = 1
	if _, err := provider.Complete(context.Background(), request); err == nil || calls != 1 {
		t.Fatal("body limit bypassed")
	}
	provider = NewTitanProvider(invokeFunc(func(ctx context.Context, in *bedrockruntime.InvokeModelInput) (*bedrockruntime.InvokeModelOutput, error) {
		return &bedrockruntime.InvokeModelOutput{ContentType: aws.String("application/json"), Body: make([]byte, maxEmbeddingBodyBytes+1)}, nil
	}))
	request.MaxBodyBytes = maxEmbeddingBodyBytes
	if _, err := provider.Complete(context.Background(), request); err == nil {
		t.Fatal("oversize response accepted")
	}
	generation, _ := NewOpenAIChatProvider(HTTPProviderConfig{BaseURL: "https://unused.example/v1"})
	connector, _ := NewHostConnector(HostConnectorConfig{SessionID: "test", Provider: generation, EmbeddingsProvider: generation, AllowedEmbeddingModels: []string{TitanEmbeddingModel}})
	if _, err := connector.Forward(context.Background(), ModelFrame{SessionID: "test", RequestID: 1, Codec: CodecTitanEmbeddings, Path: titanPath, Body: []byte(titanInput), TimeoutMS: 100}); err == nil {
		t.Fatal("wrong provider accepted")
	}
}

func TestTitanStorageFailuresAndInjectionExclusion(t *testing.T) {
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
			body := titanFixture(t, f.Body)
			return ModelResult{Status: 200, Body: body, FinalRequest: f.Body, ProviderRequest: f.Body, ProviderResponse: body}, nil
		})
		proxy, _ := NewModelProxy("test", connector, nil, store, nil, 1<<20, time.Second)
		tokens := int64(0)
		proxy.SetTokenObserver(func(n int64) { tokens += n })
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, httptest.NewRequest("POST", titanPath, strings.NewReader(titanInput)))
		if w.Code != 507 || (!responseFailure && (calls != 0 || tokens != 0)) || (responseFailure && (calls != 1 || tokens != 3)) {
			t.Fatal(w.Code, calls, tokens)
		}
	}
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
		return ModelResult{Status: 200, Body: titanFixture(t, f.Body), FinalRequest: f.Body}, nil
	})
	proxy, _ := NewModelProxy("test", connector, manager, nil, nil, 1<<20, time.Second)
	body, _ := json.Marshal(map[string]string{"inputText": content})
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, httptest.NewRequest("POST", titanPath, strings.NewReader(string(body))))
	after, _ := json.Marshal(manager.Evaluation())
	if w.Code != 200 || string(before) != string(after) {
		t.Fatalf("embedding changed injection: %s %s %s", before, after, w.Body)
	}
}

func TestTitanObservedWindowsAndRequiredRecorder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	recorder, err := recording.Open(path, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	connector := deliveryConnector(func(f ModelFrame) (ModelResult, error) {
		return ModelResult{Status: 200, Body: titanFixture(t, f.Body), FinalRequest: f.Body}, nil
	})
	proxy, _ := NewModelProxy("test", connector, nil, nil, recorder, 1<<20, time.Second)
	for _, observation := range []recording.Observation{{Window: "none"}, {TurnID: "observed-turn", AttemptID: "observed-attempt", Window: "active"}, {Window: "ambiguous"}} {
		req := httptest.NewRequest("POST", titanPath, strings.NewReader(titanInput))
		req.Header.Set("X-Interceptor-Turn-ID", "caller-claimed")
		req = req.WithContext(recording.WithObservation(req.Context(), observation))
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
	}
	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	events, err := recording.VerifyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	turns := []string{}
	for _, event := range events {
		if event.Kind == "model_exchange" {
			turns = append(turns, event.TurnID)
		}
	}
	if strings.Join(turns, ",") != ",observed-turn," {
		t.Fatal(turns)
	}
}

func TestTitanErrorMapping(t *testing.T) {
	for code, status := range map[string]int{"ValidationException": 400, "AccessDeniedException": 403, "ThrottlingException": 429, "ServiceUnavailableException": 503, "ModelTimeoutException": 408, "ResourceNotFoundException": 404, "InternalServerException": 502} {
		result := titanProviderError(&smithy.GenericAPIError{Code: code, Message: "private-credential"})
		if result.Status != status || result.Headers["X-Amzn-Errortype"] != code || strings.Contains(string(result.Body), "private-credential") {
			t.Fatal(code, result)
		}
	}
}

func TestTitanHostEndpointAdmissionAndRedirects(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "host-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "host-secret")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Location", "http://127.0.0.1:9/leak")
		w.WriteHeader(307)
	}))
	defer server.Close()
	config := BedrockProviderConfig{Authentication: "aws-environment", Region: "us-west-2"}
	for _, endpoint := range []string{"http://external.example", "https://user:pass@example.com", "https://example.com/path", "https://example.com?secret=x"} {
		t.Setenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME", endpoint)
		if _, err := LoadTitanProvider(context.Background(), config); err == nil {
			t.Fatal("unsafe endpoint admitted")
		}
	}
	t.Setenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME", server.URL)
	provider, err := LoadTitanProvider(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Complete(context.Background(), ProviderRequest{Codec: CodecTitanEmbeddings, Model: TitanEmbeddingModel, Path: titanPath, Body: []byte(titanInput), MaxBodyBytes: 1 << 20})
	if err != nil || result.Status == 200 || calls != 1 {
		t.Fatal(result, err, calls)
	}
}
