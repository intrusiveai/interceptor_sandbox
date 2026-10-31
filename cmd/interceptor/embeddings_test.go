package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/intrusive-ai/interceptor-sandbox/internal/credentials"
	"os"
	"path/filepath"
	"strings"
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

func TestEmbeddingStoredCredentialsReuseAuditedResolver(t *testing.T) {
	for _, failed := range []bool{false, true} {
		root := t.TempDir()
		t.Setenv("INTERCEPTOR_DATA_DIR", filepath.Join(root, "data"))
		config := credentials.Config{
			Profiles:    []credentials.SecretStoreProfile{{APIVersion: credentials.StoreProfileVersion, Kind: "SecretStoreProfile", ID: "test", BackendKind: "aws-secrets-manager", Region: "us-west-2", AllowedLocatorPrefixes: []string{"aws://us-west-2/test"}, MaxCacheTTLSeconds: 300}},
			Credentials: []credentials.HostCredentialRef{{APIVersion: credentials.CredentialRefVersion, Kind: "HostCredentialRef", CredentialID: "embeddings", StoreProfileID: "test", Locator: credentials.Locator{BackendKind: "aws-secrets-manager", Region: "us-west-2", SecretID: "test"}, Value: credentials.ValueSelection{Format: "utf8"}, CacheTTLSeconds: 300}},
		}
		raw, _ := json.Marshal(config)
		path := filepath.Join(root, "credentials.json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		backend := &checkSecretBackend{failed: failed}
		resolve := func(ctx context.Context, path, id string) (string, error) {
			return resolveStoredCredentialWithFactories(ctx, path, id, map[string]credentials.BackendFactory{"aws-secrets-manager": func(context.Context, credentials.SecretStoreProfile) (credentials.Backend, error) {
				return backend, nil
			}})
		}
		options := embeddingsOptions{provider: bridge.ProviderOpenAICompatible, baseURL: "https://embeddings.example/v1", credentialConfig: path, credentialID: "embeddings", models: stringList{"embed"}}
		key, err := options.resolve(context.Background(), true, resolve)
		if (err != nil) != failed || backend.calls != 1 || (!failed && key != "host-secret") {
			t.Fatal(err, backend.calls)
		}
		if err != nil && strings.Contains(err.Error(), "host-secret") {
			t.Fatal("credential leaked in failure")
		}
		audit, err := os.ReadFile(filepath.Join(root, "data/audit/credential-resolutions.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(audit), "host-secret") || !strings.Contains(string(audit), "embeddings") {
			t.Fatal("unsafe or absent audit")
		}
	}
}

func TestTitanEmbeddingsConfiguration(t *testing.T) {
	stored := func(context.Context, string, string) (string, error) {
		t.Fatal("Titan used secret-store bearer key")
		return "", nil
	}
	base := embeddingsOptions{provider: bridge.ProviderBedrockTitan, auth: "aws-environment", awsRegion: "us-west-2", models: stringList{bridge.TitanEmbeddingModel}}
	for _, auth := range []string{"aws-environment", "aws-profile", "workload-identity"} {
		options := base
		options.auth = auth
		if auth == "aws-profile" {
			options.awsProfile = "embedding-profile"
		}
		if key, err := options.resolve(context.Background(), true, stored); err != nil || key != "" {
			t.Fatal(key, err)
		}
	}
	for _, mutate := range []func(*embeddingsOptions){
		func(o *embeddingsOptions) { o.models = nil }, func(o *embeddingsOptions) { o.models = stringList{"other"} },
		func(o *embeddingsOptions) { o.auth = "" }, func(o *embeddingsOptions) { o.auth = "none" },
		func(o *embeddingsOptions) { o.auth = "aws-profile" }, func(o *embeddingsOptions) { o.awsProfile = "unexpected" },
		func(o *embeddingsOptions) { o.baseURL = "https://example.com" }, func(o *embeddingsOptions) { o.credentialID = "key" },
		func(o *embeddingsOptions) { o.credentialConfig = "keys.yaml" }, func(o *embeddingsOptions) { o.keyEnv = "KEY" },
		func(o *embeddingsOptions) { o.provider = "" }, func(o *embeddingsOptions) { o.provider = bridge.ProviderOpenAICompatible },
	} {
		options := base
		mutate(&options)
		if _, err := options.resolve(context.Background(), true, stored); err == nil {
			t.Fatalf("accepted %+v", options)
		}
	}
}
