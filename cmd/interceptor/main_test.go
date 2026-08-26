package main

import (
	"bytes"
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
