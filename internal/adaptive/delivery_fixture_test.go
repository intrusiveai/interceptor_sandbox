package adaptive

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

func TestDeliveryExampleNativeFixtureAndVerification(t *testing.T) {
	// Copy the committed source into a disposable environment, never seal or
	// mutate repository source assets as a test side effect.
	dir := filepath.Join(t.TempDir(), "environment")
	if err := os.CopyFS(dir, os.DirFS("../../examples/delivery-contract-environment")); err != nil {
		t.Fatal(err)
	}
	// Asset modes are sealed. Normalize the synthetic fixture independently of
	// the test runner's umask, never the original Blueprint directory.
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0644)
		if entry.IsDir() {
			mode = 0755
		}
		return os.Chmod(path, mode)
	}); err != nil {
		t.Fatal(err)
	}
	lock, report, err := environment.Seal(dir, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	offline, err := BuildCapabilityManifest(report.Blueprint, lock.EnvironmentDigest, lock.ApplicationDigest)
	if err != nil {
		t.Fatal(err)
	}
	verifiedLock, verified, err := environment.Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	runtimeSource, err := BuildCapabilityManifest(verified.Blueprint, verifiedLock.EnvironmentDigest, verifiedLock.ApplicationDigest)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeSource.Digest != offline.Digest {
		t.Fatal("offline and verified-runtime source derivations differ")
	}
	actual, err := json.MarshalIndent(offline, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("testdata/capability-delivery.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(actual), bytes.TrimSpace(golden)) {
		t.Fatal("native capability fixture differs; review version/source/digest change before recapture")
	}
	if len(offline.Services) != 1 || len(offline.Services[0].Endpoints) != 2 {
		t.Fatal("missing tool context")
	}
	for _, private := range []string{`"model_providers"`, `"authentication_modes"`, "Customer-provided benign record", "Benign private fixture note", "tickets.sandbox.local", "assets/tools.yaml"} {
		if bytes.Contains(actual, []byte(private)) {
			t.Fatalf("leaked protected data: %q", private)
		}
	}
}
