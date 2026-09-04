package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvironmentCommandLifecycle(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "environment")
	for _, arguments := range [][]string{
		{"environment", "init", directory},
		{"environment", "validate", directory, "--json"},
		{"environment", "seal", directory, "--json"},
		{"environment", "inspect", directory, "--json"},
	} {
		var stdout, stderr bytes.Buffer
		if err := run(arguments, &stdout, &stderr); err != nil {
			t.Fatalf("%v failed: %v (%s)", arguments, err, stderr.String())
		}
		if stdout.Len() == 0 {
			t.Fatalf("%v produced no output", arguments)
		}
	}
}

func TestGatewayTokenPolicy(t *testing.T) {
	const variable = "INTERCEPTOR_TEST_GATEWAY_TOKEN"
	if err := os.Setenv(variable, "a-development-token-that-is-long-enough"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(variable) })
	value, source, err := resolveGatewayToken(t.Context(), "", "", variable, false)
	if err != nil || source != "environment" || value == "" {
		t.Fatalf("development token resolution failed: source=%q err=%v", source, err)
	}
	if _, _, err := resolveGatewayToken(t.Context(), "", "", variable, true); err == nil {
		t.Fatal("production gateway accepted an environment token")
	}
	if _, _, err := resolveGatewayToken(t.Context(), "missing.yaml", "", variable, false); err == nil {
		t.Fatal("credential configuration without an ID was accepted")
	}
}

func TestBoundedProduct(t *testing.T) {
	if got := boundedProduct(10, 20, 1_000); got != 200 {
		t.Fatalf("bounded product = %d", got)
	}
	if got := boundedProduct(1<<40, 2, 1<<40); got != 1<<40 {
		t.Fatalf("overflowing product = %d", got)
	}
}

func TestRunRequiresExplicitProviderAndModel(t *testing.T) {
	var stdout bytes.Buffer
	err := run([]string{"run", "--environment", "missing"}, &stdout, &stdout)
	if err == nil || !strings.Contains(err.Error(), "--allow-model") {
		t.Fatalf("expected explicit provider configuration error, got %v", err)
	}
}

func TestProviderOptionValidation(t *testing.T) {
	if _, err := validateProviderOptions("bedrock-converse", "", ""); err != nil {
		t.Fatalf("Bedrock should use the AWS SDK credential chain: %v", err)
	}
	if _, err := validateProviderOptions("litellm", "http://127.0.0.1:4000/v1", ""); err != nil {
		t.Fatalf("an unauthenticated local LiteLLM gateway should be supported: %v", err)
	}
	if _, err := resolveProviderOptions(t.Context(), "litellm", "http://127.0.0.1:4000/v1", "", "", "", "none", true); err != nil {
		t.Fatalf("an explicitly unauthenticated production LiteLLM gateway should be supported: %v", err)
	}
	if _, err := resolveProviderOptions(t.Context(), "vertex-gemini", "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1", "", "", "", "api-key", false); err == nil {
		t.Fatal("Vertex should reject API-key authentication")
	}
	if _, err := validateProviderOptions("litellm", "http://127.0.0.1:4000/v1", "MISSING_LITELLM_KEY"); err == nil {
		t.Fatal("an explicitly selected but empty LiteLLM key variable should fail")
	}
	if _, err := validateProviderOptions("openai-responses", "", "OPENAI_API_KEY"); err == nil {
		t.Fatal("OpenAI Responses should require a fixed provider URL")
	}
	if _, err := validateProviderOptions("unknown", "http://provider.example/v1", ""); err == nil {
		t.Fatal("unknown providers should be rejected")
	}
}
