package main

import (
	"context"
	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"testing"
)

func TestAlignedProviderSelectors(t *testing.T) {
	t.Setenv("INTERCEPTOR_TEST_MODEL_KEY", "synthetic-test-key")
	for _, row := range []struct{ provider, mode string }{{bridge.ProviderAzureOpenAI, "azure-cli"}, {bridge.ProviderAzureOpenAI, "azure-client-secret"}, {bridge.ProviderVertexGemini, "google-adc"}, {bridge.ProviderVertexGemini, "workload-identity"}} {
		if value, err := resolveProviderOptions(context.Background(), row.provider, "https://provider.invalid", "", "", "", row.mode, true); err != nil || value != "" {
			t.Fatal(row, err)
		}
		if _, err := resolveProviderOptions(context.Background(), row.provider, "https://provider.invalid", "INTERCEPTOR_TEST_MODEL_KEY", "", "", row.mode, true); err == nil {
			t.Fatal("mixed credentials accepted")
		}
	}
	for _, provider := range []string{bridge.ProviderAzureOpenAI, bridge.ProviderVertexGemini, bridge.ProviderOpenAIChat} {
		key, err := resolveProviderOptions(context.Background(), provider, "https://provider.invalid", "INTERCEPTOR_TEST_MODEL_KEY", "", "", "api-key-env", true)
		if err != nil || key != "synthetic-test-key" {
			t.Fatal(provider, err)
		}
		if _, err = resolveProviderOptions(context.Background(), provider, "https://provider.invalid", "", "", "", "api-key-env", true); err == nil {
			t.Fatal("unnamed environment accepted")
		}
	}
	for _, mode := range []string{"workload-identity", "aws-profile", "aws-environment"} {
		if _, err := resolveProviderOptions(context.Background(), bridge.ProviderBedrockConverse, "", "", "", "", mode, true); err != nil {
			t.Fatal(err)
		}
	}
}
