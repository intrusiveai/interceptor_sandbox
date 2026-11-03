package environment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPythonTrustDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name, tls string
		valid     bool
	}{
		{"default auto", `enabled: true, python_environments: [/app/.venv/bin/python, /opt/worker/bin/python]`, true},
		{"empty", `enabled: true, python_environments: []`, true},
		{"disabled", `python_environments: [/app/python]`, false},
		{"bundle", `enabled: true, trust_mode: bundle, python_environments: [/app/python]`, false},
		{"managed", `enabled: true, trust_mode: application-managed, application_managed_probe: [/app/probe], python_environments: [/app/python]`, false},
		{"relative", `enabled: true, python_environments: [python]`, false},
		{"blank", `enabled: true, python_environments: ['']`, false},
		{"root", `enabled: true, python_environments: [/]`, false},
		{"unclean", `enabled: true, python_environments: [/app/../python]`, false},
		{"duplicate", `enabled: true, python_environments: [/app/python, /app/python]`, false},
		{"newline", `enabled: true, python_environments: ["/app/python\nRUN"]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := testBlueprint(t, "network:\n  tls: {"+tc.tls+"}\n")
			_, err := Validate(directory)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

func TestPythonTrustSealCompatibility(t *testing.T) {
	directory := testBlueprint(t, "network:\n  tls: {enabled: true}\n")
	lock, _, err := Seal(directory, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, BlueprintFile)
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	empty := strings.Replace(string(original), "tls: {enabled: true}", "tls: {enabled: true, python_environments: []}", 1)
	if err := os.WriteFile(file, []byte(empty), 0600); err != nil {
		t.Fatal(err)
	}
	verified, _, err := Verify(directory)
	if err != nil || verified.EnvironmentDigest != lock.EnvironmentDigest {
		t.Fatalf("empty declaration changed existing seal: %v", err)
	}
	declared := strings.Replace(empty, "python_environments: []", "python_environments: [/app/.venv/bin/python]", 1)
	if err := os.WriteFile(file, []byte(declared), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(directory); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("declaration did not invalidate seal: %v", err)
	}
}
