package hostrun

import (
	"context"
	"fmt"
	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVertexHostAPIKeyUsesGoogleKeyHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Api-Key") != "synthetic-key" || r.Header.Get("Authorization") != "" {
			t.Error("API key sent as OAuth bearer")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()
	provider, err := modelProvider(context.Background(), Config{ProviderType: bridge.ProviderVertexGemini, ProviderURL: server.URL + "/v1/projects/p/locations/global", ProviderAPIKey: "synthetic-key", ProviderAuthMode: "api-key-env"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Complete(context.Background(), bridge.ProviderRequest{Codec: bridge.CodecGoogleGenerateContent, Model: "gemini-test", Body: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`), MaxBodyBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
}
