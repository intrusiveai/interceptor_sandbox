package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func TestLogicalDeltaAndReplay(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "source")
	destination := filepath.Join(base, "destination")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "state.json"), []byte(`{"turn":1}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/state.json", filepath.Join(source, "current")); err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(filepath.Join(base, "blobs"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := Scan(context.Background(), []Root{{ContainerPath: "/state/app", HostPath: source}}, blobs, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	operations, err := Delta(EmptyManifest(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(base, "state.jsonl")
	journal, err := statejournal.Open(journalPath, "sess-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append("snapshot_filesystem_delta", operations); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	transactions, err := statejournal.VerifyFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := ReplayFilesystem(context.Background(), []Root{{ContainerPath: "/state/app", HostPath: destination}}, blobs, transactions, 1); err != nil {
		t.Fatal(err)
	}
	restored, err := Scan(context.Background(), []Root{{ContainerPath: "/state/app", HostPath: destination}}, blobs, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Digest != manifest.Digest {
		t.Fatalf("restored manifest differs: %s != %s", restored.Digest, manifest.Digest)
	}
}

func TestScanRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("../../outside", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(filepath.Join(t.TempDir(), "blobs"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(context.Background(), []Root{{ContainerPath: "/state", HostPath: root}}, blobs, 1024); err == nil {
		t.Fatal("expected escaping symlink to fail")
	}
}

func TestCheckpointTamperingDetected(t *testing.T) {
	checkpoint, err := NewCheckpoint(
		"sess-test", "", digestOf("environment"), digestOf("app"), 2,
		digestOf("journal"), digestOf("manifest"), 0, 3, "after turn", "0.1.0", fixedTime(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	checkpoint.Label = "tampered"
	if err := VerifyCheckpoint(checkpoint); err == nil {
		t.Fatal("expected checkpoint hash mismatch")
	}
}

func digestOf(value string) string { return digest.Bytes([]byte(value)) }

func fixedTime() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) }

func TestCheckpointCampaignAndDescriptionRoundTrip(t *testing.T) {
	h := digestOf("test")
	cp, err := NewCheckpoint("sess-test", "", h, h, 1, h, h, 0, 1, "baseline", "test", fixedTime(), Metadata{CampaignID: "campaign-1", Description: "Before scenario branch B — mutation baseline"})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err = WriteCheckpoint(root, cp); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCheckpoint(root, cp.ID)
	if err != nil || loaded != cp {
		t.Fatal(loaded, err)
	}
	for _, field := range []string{"campaign", "description"} {
		changed := cp
		if field == "campaign" {
			changed.CampaignID = "campaign-2"
		} else {
			changed.Description = "changed"
		}
		if VerifyCheckpoint(changed) == nil {
			t.Fatal("metadata not covered by checkpoint hash")
		}
	}
	if _, err = NewCheckpoint("sess-test", "", h, h, 1, h, h, 0, 1, "baseline", "test", fixedTime(), Metadata{Description: strings.Repeat("x", MaxDescriptionBytes+1)}); err == nil {
		t.Fatal("oversized description accepted")
	}
}
