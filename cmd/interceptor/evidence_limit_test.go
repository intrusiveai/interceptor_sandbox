package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evidence"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

func TestExportCapacityCleanupAndAdministrativeRetry(t *testing.T) {
	root := t.TempDir()
	t.Setenv("INTERCEPTOR_DATA_DIR", root)
	env := filepath.Join(root, "environment")
	if err := os.CopyFS(env, os.DirFS("../../examples/delivery-contract-environment")); err != nil {
		t.Fatal(err)
	}
	lock, _, err := environment.Seal(env, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	metadata := session.Metadata{SchemaVersion: 1, ID: "sess-1-000000000000000000000000", Phase: session.Stopped, EnvironmentPath: env, EnvironmentDigest: lock.EnvironmentDigest, AppDigest: lock.ApplicationDigest, FeedbackProfile: "black-box", Limits: session.Limits{MaxBlobBytes: 2 << 20}}
	paths, events, journal, err := session.Initialize(root, metadata, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Close(); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.WriteManifest(paths.FileManifest, snapshot.EmptyManifest()); err != nil {
		t.Fatal(err)
	}
	store, err := blobstore.Open(paths.Blobs, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Seed files without any event or journal delta referencing them, as happens
	// for unchanged files materialized from the initial sealed environment.
	initial := filepath.Join(root, "initial")
	if err := os.Mkdir(initial, 0750); err != nil {
		t.Fatal(err)
	}
	initialFiles := map[string][]byte{"unchanged.txt": []byte("unchanged seed file"), "empty.txt": {}}
	for name, content := range initialFiles {
		if err := os.WriteFile(filepath.Join(initial, name), content, 0640); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := snapshot.Scan(t.Context(), []snapshot.Root{{ContainerPath: "/state/app", HostPath: initial}}, store, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.WriteManifest(paths.FileManifest, manifest); err != nil {
		t.Fatal(err)
	}
	registry, err := adaptive.OpenArtifactRegistry(filepath.Join(paths.Root, "artifacts.json"), store, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("test-data"), 16384)
	descriptor := adaptive.ArtifactDescriptor{Digest: digest.Bytes(content), SizeBytes: int64(len(content)), MediaType: "text/plain", Canonicalization: "raw"}
	if _, err := registry.Register(t.Context(), descriptor, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	metadata, _, err = session.Load(root, metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paths.Events)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "evidence.tar")
	if err := os.WriteFile(output, []byte("previous export"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := exportEvidence(t.Context(), root, metadata, paths, output, 1024); !errors.Is(err, evidence.ErrLimitExceeded) {
		t.Fatal("expected capacity failure", err)
	}
	previous, _ := os.ReadFile(output)
	if string(previous) != "previous export" {
		t.Fatal("failed export replaced output")
	}
	temps, _ := filepath.Glob(filepath.Join(root, ".interceptor-export-*.tmp"))
	if len(temps) != 0 {
		t.Fatal("temporary files leaked", temps)
	}
	// An explicit CLI value overrides a lower environment default, without a new session.
	t.Setenv("INTERCEPTOR_EVIDENCE_MAX_BYTES", "1024")
	var stdout bytes.Buffer
	if err := logsCommand([]string{"export", metadata.ID, "--output", output, "--evidence-max-bytes", "4194304"}, &stdout); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	archive := tar.NewReader(file)
	expectedBlobs := map[string][]byte{descriptor.Digest: content}
	for _, raw := range initialFiles {
		expectedBlobs[digest.Bytes(raw)] = raw
	}
	found := map[string]bool{}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(header.Name, "blobs/") {
			raw, err := io.ReadAll(archive)
			blob := "sha256:" + strings.TrimPrefix(header.Name, "blobs/sha256/")
			expected, exists := expectedBlobs[blob]
			if err != nil || !exists || !bytes.Equal(raw, expected) || found[blob] {
				t.Fatal("blob bytes changed", err)
			}
			found[blob] = true
		}
	}
	if len(found) != len(expectedBlobs) {
		t.Fatal("missing registered or initial-manifest blob", found)
	}
	info, _ := file.Stat()
	// Full archive size includes tar padding/trailer; equality succeeds, one byte less fails.
	if err := exportEvidence(t.Context(), root, metadata, paths, output, info.Size()); err != nil {
		t.Fatal("exact boundary", err)
	}
	if err := exportEvidence(t.Context(), root, metadata, paths, output, info.Size()-1); !errors.Is(err, evidence.ErrLimitExceeded) {
		t.Fatal("footer bypassed limit", err)
	}
	after, _ := os.ReadFile(paths.Events)
	if !bytes.Equal(before, after) {
		t.Fatal("export modified source journal")
	}
	if err := store.Verify(descriptor.Digest); err != nil {
		t.Fatal("retained source changed", err)
	}
	temps, _ = filepath.Glob(filepath.Join(root, ".interceptor-export-*.tmp"))
	if len(temps) != 0 {
		t.Fatal("temporary files leaked", temps)
	}
}
func TestRunEvidenceLimitValidation(t *testing.T) {
	t.Setenv("INTERCEPTOR_DATA_DIR", t.TempDir())
	for _, bad := range []string{"0", "-1", "invalid", "9007199254740992"} {
		var output bytes.Buffer
		err := run([]string{"run", "--environment", "missing", "--allow-model", "test", "--evidence-max-bytes", bad}, &output, &output)
		if err == nil || !strings.Contains(err.Error(), "evidence-max-bytes") {
			t.Fatal(bad, err)
		}
	}
	flags := []string{"run", "--environment", "missing", "--allow-model", "test"}
	t.Setenv("INTERCEPTOR_EVIDENCE_MAX_BYTES", strconv.FormatInt(-1, 10))
	var output bytes.Buffer
	if err := run(flags, &output, &output); err == nil || !strings.Contains(err.Error(), "evidence-max-bytes") {
		t.Fatal(err)
	}
}
