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
		embedding, err := embeddingsProvider(context.Background(), config)
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

func TestTitanHostConfiguration(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME", "http://127.0.0.1:9")
	base := Config{EmbeddingsProviderType: bridge.ProviderBedrockTitan, EmbeddingsAuth: "aws-environment", EmbeddingsAWSRegion: "us-west-2", AllowedEmbeddingModels: []string{bridge.TitanEmbeddingModel}, AWSProfile: "generation-profile", AWSRegion: "other-region", ProviderAuthMode: "aws-profile"}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.EmbeddingsURL = "https://example.com" }, func(c *Config) { c.EmbeddingsAPIKey = "key" },
		func(c *Config) { c.AllowedEmbeddingModels = nil }, func(c *Config) { c.AllowedEmbeddingModels = []string{"other"} },
		func(c *Config) { c.EmbeddingsAuth = "" }, func(c *Config) { c.EmbeddingsAWSProfile = "unexpected" },
	} {
		config := base
		mutate(&config)
		if _, err := embeddingsProvider(context.Background(), config); err == nil {
			t.Fatal("accepted invalid Titan configuration")
		}
	}
	for range 2 {
		provider, err := embeddingsProvider(context.Background(), base)
		if err != nil || !provider.SupportsCodec(bridge.CodecTitanEmbeddings) {
			t.Fatal(err)
		}
	}
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	if _, err := embeddingsProvider(context.Background(), base); err == nil {
		t.Fatal("missing credential accepted")
	}
}
