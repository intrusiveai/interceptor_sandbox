package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func TestHostConnectorUsesFixedDestinationAndHostCredential(t *testing.T) {
	var receivedPath, authorization string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		receivedPath, authorization = request.URL.Path, request.Header.Get("Authorization")
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"response","usage":{"total_tokens":4}}`)), Request: request}, nil
	})}
	connector, err := NewHostConnector(HostConnectorConfig{
		SessionID: "sess-test", ProviderURL: "https://provider.example/v1", APIKey: "host-secret",
		AllowedModel: []string{"safe-model"}, MaxBodyBytes: 4096, HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"safe-model","messages":[]}`)
	result, err := connector.Forward(context.Background(), ModelFrame{SessionID: "sess-test", RequestID: 1, Codec: "openai_chat_completions", Path: "/v1/chat/completions", Body: body, TimeoutMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if receivedPath != "/v1/chat/completions" || authorization != "Bearer host-secret" {
		t.Fatalf("unexpected upstream request: path=%s authorization=%s", receivedPath, authorization)
	}
	if string(result.FinalRequest) != string(body) || result.FinalRequestDigest != digest.Bytes(body) || strings.Contains(string(result.FinalRequest), "host-secret") {
		t.Fatalf("credential leaked into connector result: %#v", result)
	}
	if result.Provider != ProviderOpenAIChat || result.ProviderDigest != digest.Bytes(body) || result.ProviderResponseDigest != digest.Bytes(result.Body) {
		t.Fatalf("provider evidence was not identified and fingerprinted: %#v", result)
	}
	if _, err := connector.Forward(context.Background(), ModelFrame{SessionID: "sess-test", RequestID: 2, Codec: "openai_chat_completions", Path: "/v1/chat/completions", Body: []byte(`{"model":"other"}`), TimeoutMS: 1000}); err == nil {
		t.Fatal("expected disallowed model to fail")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type captureConnector struct{ frame ModelFrame }

func (c *captureConnector) Forward(_ context.Context, frame ModelFrame) (ModelResult, error) {
	c.frame = frame
	return ModelResult{Status: 200, Headers: map[string]string{"Content-Type": "application/json"}, Body: []byte(`{"choices":[],"usage":{"total_tokens":2}}`), FinalRequest: frame.Body, FinalRequestDigest: digest.Bytes(frame.Body)}, nil
}

type cancelConnector struct {
	started  chan struct{}
	canceled chan struct{}
}

func (c *cancelConnector) Forward(ctx context.Context, _ ModelFrame) (ModelResult, error) {
	close(c.started)
	<-ctx.Done()
	close(c.canceled)
	return ModelResult{}, ctx.Err()
}

func TestModelProxyAppliesInjectionBeforeConnector(t *testing.T) {
	journal, err := statejournal.Open(filepath.Join(t.TempDir(), "state.jsonl"), "sess-test")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	injections := injection.NewManager(journal, nil)
	if err := injections.Arm(injection.Definition{
		ID: "inj", Surface: "model_tool_result", Mode: "simulation", Scope: "standing", Enabled: true,
		Selector:  injection.Selector{ToolName: "lookup", CallOrdinal: "first"},
		Placement: injection.Placement{Operation: "append_text"}, Payload: json.RawMessage(`" attack"`),
	}, ""); err != nil {
		t.Fatal(err)
	}
	connector := &captureConnector{}
	proxy, err := NewModelProxy("sess-test", connector, injections, nil, nil, 1<<20, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"safe-model","messages":[{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"result"}]}`
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if response.Code != http.StatusOK || !strings.Contains(string(connector.frame.Body), "result attack") {
		t.Fatalf("proxy did not rewrite final request: status=%d body=%s", response.Code, connector.frame.Body)
	}
	state := injections.Evaluation()
	if len(state) != 1 || state[0].Delivery != "confirmed" {
		t.Fatalf("delivery was not confirmed: %#v", state)
	}
}

func TestModelProxyPreservesNativeApplicationRoutes(t *testing.T) {
	connector := &captureConnector{}
	proxy, err := NewModelProxy("sess-test", connector, nil, nil, nil, 1<<20, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path, body, codec string
	}{
		{"/v1/chat/completions", `{"model":"chat-model","messages":[]}`, CodecOpenAIChat},
		{"/v1/responses", `{"model":"responses-model","input":"hello"}`, CodecOpenAIResponses},
		{"/model/anthropic.claude-v2%3A1/converse", `{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`, CodecBedrockConverse},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body)))
		if response.Code != http.StatusOK || connector.frame.Codec != test.codec || string(connector.frame.Body) != test.body {
			t.Fatalf("native route %s changed: status=%d frame=%#v", test.path, response.Code, connector.frame)
		}
	}
}

func TestModelIdentifierUsesBodyForOpenAIAndPathForBedrock(t *testing.T) {
	model, err := modelIdentifier(CodecOpenAIResponses, "/v1/responses", []byte(`{"model":"gpt-test","input":"hello"}`))
	if err != nil || model != "gpt-test" {
		t.Fatalf("unexpected Responses model: %q %v", model, err)
	}
	model, err = modelIdentifier(CodecBedrockConverse, "/model/arn%3Aaws%3Abedrock%3Aus-west-2%3A123%3Ainference-profile%2Ftest/converse", []byte(`{}`))
	if err != nil || model != "arn:aws:bedrock:us-west-2:123:inference-profile/test" {
		t.Fatalf("unexpected Bedrock model: %q %v", model, err)
	}
}

func TestModelProxyRejectsStreamingWithoutCallingConnector(t *testing.T) {
	connector := &captureConnector{}
	proxy, _ := NewModelProxy("sess-test", connector, nil, nil, nil, 1<<20, time.Second)
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hello","stream":true}`)))
	if response.Code != http.StatusBadRequest || connector.frame.RequestID != 0 {
		t.Fatalf("streaming request was not rejected before forwarding: status=%d frame=%#v", response.Code, connector.frame)
	}
}

func TestSpoolConnectorRoundTrip(t *testing.T) {
	root := t.TempDir()
	client, err := NewSpoolConnector(root)
	if err != nil {
		t.Fatal(err)
	}
	host := &captureConnector{}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- ServeSpool(ctx, root, host) }()
	body := []byte(`{"model":"safe-model","messages":[]}`)
	result, err := client.Forward(context.Background(), ModelFrame{
		SessionID: "sess-test", RequestID: 1, Codec: "openai_chat_completions",
		Path: "/v1/chat/completions", Body: body, TimeoutMS: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != http.StatusOK || string(host.frame.Body) != string(body) {
		t.Fatalf("unexpected spool result: %#v frame=%#v", result, host.frame)
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("spool server did not stop")
	}
}

func TestModelSpoolPropagatesCancellation(t *testing.T) {
	root := t.TempDir()
	client, err := NewSpoolConnector(root)
	if err != nil {
		t.Fatal(err)
	}
	host := &cancelConnector{started: make(chan struct{}), canceled: make(chan struct{})}
	serverContext, stopServer := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- ServeSpool(serverContext, root, host) }()
	requestContext, cancelRequest := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Forward(requestContext, ModelFrame{SessionID: "sess-test", RequestID: 1, Codec: "openai_chat_completions", Path: "/v1/chat/completions", Body: []byte(`{"model":"safe"}`), TimeoutMS: 1000})
		result <- err
	}()
	select {
	case <-host.started:
	case <-time.After(time.Second):
		t.Fatal("host connector did not start")
	}
	cancelRequest()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled model call, got %v", err)
	}
	select {
	case <-host.canceled:
	case <-time.After(time.Second):
		t.Fatal("model cancellation did not reach the host connector")
	}
	stopServer()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestModelSpoolCleansExpiredOrphans(t *testing.T) {
	root := t.TempDir()
	if _, err := NewSpoolConnector(root); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-2 * orphanGrace)
	frame := ModelFrame{SessionID: "sess-test", RequestID: 1, TimeoutMS: 10, CreatedAt: created}
	data, _ := json.Marshal(frame)
	path := filepath.Join(processingDirectory(root), "00000000000000000001.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupModelSpool(root, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired model frame was retained: %v", err)
	}
}

func TestHTTPSpoolRoundTripUsesPrivateFiles(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	application := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		<-release
		writer.Header().Set("X-Test", "complete")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte("response"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- ServeHTTPSpool(ctx, HTTPSpoolServerConfig{Root: root, Application: application, Control: http.NotFoundHandler(), PollPeriod: time.Millisecond})
	}()
	client, err := NewHTTPSpoolClient(root, 1024, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.pollPeriod = time.Millisecond
	responses := make(chan HTTPResponseFrame, 1)
	errors := make(chan error, 1)
	go func() {
		response, err := client.Do(context.Background(), ApplicationChannel, http.MethodPost, "/invoke", http.Header{"Content-Type": []string{"text/plain"}}, []byte("request"))
		responses <- response
		errors <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("application handler did not start")
	}
	for _, directory := range []string{"requests", "processing", "responses", "cancellations"} {
		info, err := os.Stat(filepath.Join(root, directory))
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("spool directory %s is not private: mode=%v err=%v", directory, infoMode(info), err)
		}
	}
	processing, err := os.ReadDir(filepath.Join(root, "processing"))
	if err != nil || len(processing) != 1 {
		t.Fatalf("expected one claimed request: %v %#v", err, processing)
	}
	info, err := processing[0].Info()
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("request frame is not private: mode=%v err=%v", infoMode(info), err)
	}
	close(release)
	if err := <-errors; err != nil {
		t.Fatal(err)
	}
	response := <-responses
	if response.Status != http.StatusCreated || string(response.Body) != "response" || http.Header(response.Headers).Get("X-Test") != "complete" {
		t.Fatalf("unexpected response: %#v", response)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSpoolPropagatesCancellation(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{})
	canceled := make(chan struct{})
	handler := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(canceled)
	})
	serverContext, stopServer := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- ServeHTTPSpool(serverContext, HTTPSpoolServerConfig{Root: root, Application: handler, Control: http.NotFoundHandler(), PollPeriod: time.Millisecond})
	}()
	client, err := NewHTTPSpoolClient(root, 1024, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.pollPeriod = time.Millisecond
	requestContext, cancelRequest := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Do(requestContext, ApplicationChannel, http.MethodGet, "/wait", nil, nil)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	cancelRequest()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not reach handler")
	}
	stopServer()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSpoolSerializesControlRequests(t *testing.T) {
	root := t.TempDir()
	var active atomic.Int64
	var maximum atomic.Int64
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		writer.WriteHeader(http.StatusNoContent)
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- ServeHTTPSpool(ctx, HTTPSpoolServerConfig{Root: root, Application: http.NotFoundHandler(), Control: handler, PollPeriod: time.Millisecond, MaxConcurrentApplication: 4})
	}()
	client, err := NewHTTPSpoolClient(root, 1024, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.pollPeriod = time.Millisecond
	var calls sync.WaitGroup
	errors := make(chan error, 2)
	for range 2 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			_, err := client.Do(context.Background(), ControlChannel, http.MethodPost, "/mutation", nil, nil)
			errors <- err
		}()
	}
	calls.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maximum.Load() != 1 {
		t.Fatalf("control requests ran concurrently: max=%d", maximum.Load())
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSpoolBoundsApplicationConcurrency(t *testing.T) {
	root := t.TempDir()
	var active atomic.Int64
	var maximum atomic.Int64
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		updateMaximum(&maximum, current)
		time.Sleep(25 * time.Millisecond)
		active.Add(-1)
		writer.WriteHeader(http.StatusNoContent)
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- ServeHTTPSpool(ctx, HTTPSpoolServerConfig{Root: root, Application: handler, Control: http.NotFoundHandler(), PollPeriod: time.Millisecond, MaxConcurrentApplication: 2})
	}()
	client, err := NewHTTPSpoolClient(root, 1024, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.pollPeriod = time.Millisecond
	var calls sync.WaitGroup
	errors := make(chan error, 4)
	for range 4 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			_, err := client.Do(context.Background(), ApplicationChannel, http.MethodGet, "/work", nil, nil)
			errors <- err
		}()
	}
	calls.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maximum.Load() != 2 {
		t.Fatalf("application concurrency was not bounded at two: max=%d", maximum.Load())
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSpoolCleansExpiredOrphans(t *testing.T) {
	root := t.TempDir()
	if err := prepareHTTPSpool(root); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().UTC().Add(-2 * orphanGrace)
	frame := HTTPRequestFrame{ID: "expired", Channel: ApplicationChannel, Method: http.MethodGet, Path: "/", CreatedAt: expired.Add(-time.Minute), ExpiresAt: expired}
	data, _ := json.Marshal(frame)
	path := httpSpoolPath(root, "processing", frame.ID)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupHTTPSpool(root, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired processing frame was retained: %v", err)
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}

func updateMaximum(maximum *atomic.Int64, current int64) {
	for {
		previous := maximum.Load()
		if current <= previous || maximum.CompareAndSwap(previous, current) {
			return
		}
	}
}
