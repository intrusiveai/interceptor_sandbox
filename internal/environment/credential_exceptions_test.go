package environment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const tokenLimitName = "SUPPORT_MODEL_MAX_TOKENS"

func writeCredentialBlueprint(t *testing.T, directory string, blueprint *Blueprint) {
	t.Helper()
	data, err := yaml.Marshal(blueprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, BlueprintFile), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialNameExceptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Blueprint)
		want string
	}{
		{"explicit exception", func(b *Blueprint) {}, ""},
		{"no exception", func(b *Blueprint) { b.Application.Launch.CredentialNameExceptions = nil }, "or, for ordinary non-secret configuration"},
		{"another credential still blocked", func(b *Blueprint) { b.Application.Launch.Environment["API_TOKEN"] = "synthetic" }, `credential-like application environment variable "API_TOKEN"`},
		{"substring does not exempt", func(b *Blueprint) { b.Application.Launch.Environment[tokenLimitName+"_OTHER"] = "1024" }, tokenLimitName + "_OTHER"},
		{"unknown name", func(b *Blueprint) { b.Application.Launch.CredentialNameExceptions = []string{"MISSING"} }, "not declared in application.launch.environment"},
		{"lowercase", func(b *Blueprint) {
			b.Application.Launch.CredentialNameExceptions = []string{strings.ToLower(tokenLimitName)}
		}, "exact valid environment variable name"},
		{"pattern", func(b *Blueprint) { b.Application.Launch.CredentialNameExceptions = []string{"SUPPORT_*"} }, "exact valid environment variable name"},
		{"empty", func(b *Blueprint) { b.Application.Launch.CredentialNameExceptions = []string{""} }, "exact valid environment variable name"},
		{"duplicate", func(b *Blueprint) {
			b.Application.Launch.CredentialNameExceptions = []string{tokenLimitName, tokenLimitName}
		}, "duplicates"},
		{"credential conflict", func(b *Blueprint) {
			b.Credentials = []Credential{{ID: "limit", EnvironmentVariable: tokenLimitName, Value: "4096", AcceptedBy: "model-proxy"}}
		}, "also declared as a synthetic credential"},
		{"synthetic credential still accepted", func(b *Blueprint) {
			b.Application.Launch.CredentialNameExceptions = nil
			b.Credentials = []Credential{{ID: "limit", EnvironmentVariable: tokenLimitName, Value: "4096", AcceptedBy: "model-proxy"}}
		}, ""},
		{"interpolation still rejected", func(b *Blueprint) { b.Application.Launch.Environment[tokenLimitName] = "${HOST_LIMIT}" }, "may not interpolate"},
		{"control characters still rejected", func(b *Blueprint) { b.Application.Launch.Environment[tokenLimitName] = "4096\n" }, "forbidden control characters"},
		{"endpoint restrictions still apply", func(b *Blueprint) {
			b.Application.Launch.CredentialNameExceptions = append(b.Application.Launch.CredentialNameExceptions, "MODEL_BASE_URL")
			b.Application.Launch.Environment["MODEL_BASE_URL"] = "https://external.example"
		}, "http loopback"},
		{"reserved variables still rejected", func(b *Blueprint) {
			b.Network.TLS.Enabled = true
			b.Network.TLS.TrustMode = "auto"
			b.Application.Launch.Environment["INTERCEPTOR_CA_BUNDLE"] = "/tmp/other.pem"
			b.Application.Launch.CredentialNameExceptions = append(b.Application.Launch.CredentialNameExceptions, "INTERCEPTOR_CA_BUNDLE")
		}, "is managed by network.tls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := testBlueprint(t, "")
			b, err := Load(directory)
			if err != nil {
				t.Fatal(err)
			}
			b.Application.Launch.Environment[tokenLimitName] = "4096"
			b.Application.Launch.CredentialNameExceptions = []string{tokenLimitName}
			tc.edit(b)
			writeCredentialBlueprint(t, directory, b)
			_, err = Validate(directory)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCredentialNameExceptionsAreSealed(t *testing.T) {
	directory := testBlueprint(t, "")
	before, _, err := Seal(directory, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	// An empty list preserves the digest of existing blueprints.
	b.Application.Launch.CredentialNameExceptions = []string{}
	writeCredentialBlueprint(t, directory, b)
	empty, _, err := Verify(directory)
	if err != nil || empty.EnvironmentDigest != before.EnvironmentDigest {
		t.Fatalf("empty exceptions changed seal: %v", err)
	}
	b.Application.Launch.Environment[tokenLimitName] = "4096"
	b.Application.Launch.CredentialNameExceptions = []string{tokenLimitName}
	writeCredentialBlueprint(t, directory, b)
	if _, _, err := Seal(directory, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(directory); err != nil {
		t.Fatal(err)
	}
	// Add a valid exception without changing any environment values.
	b.Application.Launch.CredentialNameExceptions = append(b.Application.Launch.CredentialNameExceptions, "MODEL_BASE_URL")
	writeCredentialBlueprint(t, directory, b)
	if _, _, err := Verify(directory); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("exception edit did not invalidate seal: %v", err)
	}
	if _, _, err := Seal(directory, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(directory); err != nil {
		t.Fatal(err)
	}
	b.Application.Launch.CredentialNameExceptions = nil
	writeCredentialBlueprint(t, directory, b)
	if _, _, err := Verify(directory); err == nil || !strings.Contains(err.Error(), "credential-like") {
		t.Fatalf("removed exception bypassed validation: %v", err)
	}
}
