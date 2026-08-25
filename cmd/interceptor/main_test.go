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
	if err == nil || !strings.Contains(err.Error(), "--provider-base-url") {
		t.Fatalf("expected explicit provider configuration error, got %v", err)
	}
}
