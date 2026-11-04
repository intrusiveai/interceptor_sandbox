package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"github.com/intrusive-ai/interceptor-sandbox/internal/credentials"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/httpsseam"
)

func TestOnePasswordCredentialCheckModelAndSeamWorkflow(t *testing.T) {
	root := t.TempDir()
	t.Setenv("INTERCEPTOR_DATA_DIR", filepath.Join(root, "data"))
	raw, err := os.ReadFile("../../internal/credentials/testdata/onepassword.json")
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "credentials.json")
	os.WriteFile(config, raw, 0600)
	backend := &checkSecretBackend{}
	resolve := func(ctx context.Context, path, id string) (string, error) {
		return resolveStoredCredentialWithFactories(ctx, path, id, map[string]credentials.BackendFactory{"onepassword": func(context.Context, credentials.SecretStoreProfile) (credentials.Backend, error) {
			return backend, nil
		}})
	}
	output := filepath.Join(root, "check.jsonl")
	var stdout bytes.Buffer
	if err := credentialsCheckCommand([]string{"check", "--live", "--credential-config", config, "--credential-id", "model-key", "--probe-id", "onepassword-test", "--output", output}, &stdout, resolve); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer host-secret" {
			t.Error("credential was not applied")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()
	key, err := resolve(context.Background(), config, "model-key")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := bridge.NewOpenAIChatProvider(bridge.HTTPProviderConfig{BaseURL: upstream.URL, APIKey: key, Client: upstream.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Complete(context.Background(), bridge.ProviderRequest{Codec: bridge.CodecOpenAIChat, Model: "controlled", Body: []byte(`{"model":"controlled","messages":[]}`), MaxBodyBytes: 65536}); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(root, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0600)
	seamConfig := filepath.Join(root, "seam.json")
	raw, _ = json.Marshal(map[string]any{"seams": []any{map[string]any{"id": "test", "origin": upstream.URL, "ca_file": ca, "auth": map[string]any{"kind": "header", "header": "Authorization", "credential": "model-key", "prefix": "Bearer "}}}})
	os.WriteFile(seamConfig, raw, 0600)
	prepared, err := httpsseam.Load(context.Background(), seamConfig, []environment.HTTPSSeam{{ID: "test", Hostname: "test.example"}}, func(ctx context.Context, id string) (string, error) { return resolve(ctx, config, id) })
	if err != nil {
		t.Fatal(err)
	}
	connector, err := httpsseam.NewConnector(prepared, "session-test", filepath.Join(root, "seam-logs"), 65536, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connector.Close()
	request := httpsseam.Request{Session: "session-test", Sequence: 1, Seam: "test", Method: "POST", Target: "/test", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{}`), Expires: time.Now().Add(time.Second)}
	raw, _ = json.Marshal(request)
	incoming := httptest.NewRequest("POST", "http://local/v1/https-seam", bytes.NewReader(raw))
	response := httptest.NewRecorder()
	connector.ServeHTTP(response, incoming)
	if response.Code != 200 || calls.Load() != 2 {
		t.Fatal("seam not forwarded", response.Code, response.Body.String(), calls.Load())
	}
	backend.failed = true
	if _, err = resolve(context.Background(), config, "model-key"); err == nil {
		t.Fatal("credential failure accepted")
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected request after failed credential")
	}
	evidence, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	audit, err := os.ReadFile(filepath.Join(root, "data/audit/credential-resolutions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	combined := string(evidence) + string(audit) + stdout.String() + response.Body.String()
	for _, secret := range []string{"host-secret", "op://", "private remote error"} {
		if strings.Contains(combined, secret) {
			t.Fatal("credential leaked")
		}
	}
	for _, fact := range []string{"model-key", "developer-secrets", `"outcome":"resolved"`, `"outcome":"failed"`} {
		if !strings.Contains(string(audit), fact) {
			t.Fatal("missing audit fact", fact)
		}
	}
}
