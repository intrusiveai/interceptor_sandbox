package hostrun

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
)

func TestRetainedProviderConfigurationSupportsReplacementSessions(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/v1/embeddings" {
			if r.Header.Get("Authorization") != "Bearer embedding-key" {
				t.Error("lost embeddings credential")
			}
			fmt.Fprint(w, `{"object":"list","model":"embed","data":[{"object":"embedding","index":0,"embedding":[1]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`)
		} else {
			if r.Header.Get("Authorization") != "Bearer generation-key" {
				t.Error("lost generation credential")
			}
			fmt.Fprint(w, `{"choices":[]}`)
		}
	}))
	defer server.Close()
	// NativeRuntime retains this host-only Config and passes it to both Run and
	// Restore. Each run constructs fresh providers and a session-bound connector.
	retained := Config{ProviderType: bridge.ProviderOpenAIChat, ProviderURL: server.URL + "/v1", ProviderAPIKey: "generation-key", AllowedModels: []string{"chat"}, EmbeddingsProviderType: bridge.ProviderOpenAICompatible, EmbeddingsURL: server.URL + "/v1", EmbeddingsAPIKey: "embedding-key", AllowedEmbeddingModels: []string{"embed"}}
	for _, id := range []string{"original-session", "replacement-session"} {
		config := retained
		generation, err := modelProvider(context.Background(), config)
		if err != nil {
			t.Fatal(err)
		}
		embedding, err := embeddingsProvider(config)
		if err != nil {
			t.Fatal(err)
		}
		connector, err := bridge.NewHostConnector(bridge.HostConnectorConfig{SessionID: id, Provider: generation, AllowedModel: config.AllowedModels, EmbeddingsProvider: embedding, AllowedEmbeddingModels: config.AllowedEmbeddingModels})
		if err != nil {
			t.Fatal(err)
		}
		for i, frame := range []bridge.ModelFrame{
			{Codec: bridge.CodecOpenAIEmbeddings, Path: "/v1/embeddings", Body: []byte(`{"model":"embed","input":"hi"}`)},
			{Codec: bridge.CodecOpenAIChat, Path: "/v1/chat/completions", Body: []byte(`{"model":"chat","messages":[]}`)},
			{Codec: bridge.CodecOpenAIEmbeddings, Path: "/v1/embeddings", Body: []byte(`{"model":"chat","input":"hi"}`)},
		} {
			frame.SessionID = id
			frame.RequestID = uint64(i + 1)
			frame.TimeoutMS = 1000
			_, err := connector.Forward(context.Background(), frame)
			if (err != nil) != (i == 2) {
				t.Fatalf("%s frame %d: %v", id, i, err)
			}
		}
	}
	if calls != 4 {
		t.Fatal(calls)
	}
}
