package main

import (
	"context"
	"errors"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
)

func TestEmbeddingsOptionsAreIndependentAndExplicit(t *testing.T) {
	t.Setenv("EMBEDDING_TEST_KEY", "embedding-secret")
	t.Setenv("OPENAI_API_KEY", "generation-secret")
	calls := 0
	stored := func(_ context.Context, file, id string) (string, error) {
		calls++
		if file != "credentials.yaml" || id != "embed" {
			return "", errors.New("denied")
		}
		return "stored-embedding-secret", nil
	}
	empty := embeddingsOptions{}
	if key, err := empty.resolve(context.Background(), true, stored); err != nil || key != "" || calls != 0 {
		t.Fatal(key, err, calls)
	}
	base := embeddingsOptions{provider: bridge.ProviderOpenAICompatible, baseURL: "https://embedding.example/v1", models: stringList{"embed"}}
	for _, auth := range []string{"none", "api-key-env", "secret-store", ""} {
		o := base
		o.auth = auth
		want := ""
		if auth == "api-key-env" {
			o.keyEnv = "EMBEDDING_TEST_KEY"
			want = "embedding-secret"
		}
		if auth == "secret-store" || auth == "" {
			o.credentialConfig = "credentials.yaml"
			o.credentialID = "embed"
			want = "stored-embedding-secret"
		}
		key, err := o.resolve(context.Background(), true, stored)
		if err != nil || key != want {
			t.Fatalf("%s: %q %v", auth, key, err)
		}
	}
	for _, mutate := range []func(*embeddingsOptions){
		func(o *embeddingsOptions) { o.provider = "" }, func(o *embeddingsOptions) { o.provider = "other" }, func(o *embeddingsOptions) { o.baseURL = "" },
		func(o *embeddingsOptions) { o.models = nil }, func(o *embeddingsOptions) { o.models = stringList{" "} }, func(o *embeddingsOptions) { o.auth = "" },
		func(o *embeddingsOptions) { o.auth = "api-key-env" }, func(o *embeddingsOptions) { o.keyEnv = "EMBEDDING_TEST_KEY" },
		func(o *embeddingsOptions) { o.credentialConfig = "credentials.yaml" }, func(o *embeddingsOptions) { o.auth = "secret-store" },
		func(o *embeddingsOptions) { o.auth = "api-key-env"; o.keyEnv = "MISSING_EMBEDDING_KEY" },
		func(o *embeddingsOptions) {
			o.auth = "secret-store"
			o.credentialConfig = "credentials.yaml"
			o.credentialID = "denied"
		},
	} {
		o := base
		o.auth = "none"
		mutate(&o)
		if _, err := o.resolve(context.Background(), true, stored); err == nil {
			t.Fatalf("accepted %+v", o)
		}
	}
	// Separate embeddings credential file must not invalidate generation workload identity.
	if _, err := resolveProviderOptions(context.Background(), bridge.ProviderBedrockConverse, "", "", "", "", "workload-identity", true); err != nil {
		t.Fatal(err)
	}
	o := base
	o.credentialConfig = "credentials.yaml"
	o.credentialID = "embed"
	if key, err := o.resolve(context.Background(), true, stored); err != nil || key != "stored-embedding-secret" {
		t.Fatal(key, err)
	}
}
