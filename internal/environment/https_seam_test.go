package environment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHTTPSSeamsValidateAndBindSeal(t *testing.T) {
	config := `network:
  dns: {enabled: true}
  tls: {enabled: true}
  https_seams:
    - {id: tracing, hostname: tracing.sandbox.test}
credentials:
  - {id: placeholder, environment_variable: LANGFUSE_SECRET_KEY, value: synthetic-placeholder, accepted_by: tracing}
`
	dir := testBlueprint(t, config)
	if _, err := Validate(dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Seal(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, BlueprintFile)
	data, _ := os.ReadFile(path)
	_ = os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "tracing.sandbox.test", "another.sandbox.test")), 0600)
	if _, _, err := Verify(dir); err == nil {
		t.Fatal("seam hostname change did not invalidate seal")
	}
	for _, change := range []func(string) string{
		func(s string) string { return strings.Replace(s, "dns: {enabled: true}", "dns: {enabled: false}", 1) },
		func(s string) string { return strings.Replace(s, "tls: {enabled: true}", "tls: {enabled: false}", 1) },
		func(s string) string { return strings.ReplaceAll(s, "tracing.sandbox.test", "*.sandbox.test") },
		func(s string) string {
			return strings.Replace(s, "credentials:", "    - {id: tracing, hostname: tracing2.sandbox.test}\ncredentials:", 1)
		},
		func(s string) string {
			return strings.Replace(s, "credentials:", "    - {id: second, hostname: tracing.sandbox.test}\ncredentials:", 1)
		},
	} {
		if _, err := Validate(testBlueprint(t, change(config))); err == nil {
			t.Fatal("invalid seam declaration accepted")
		}
	}
}
