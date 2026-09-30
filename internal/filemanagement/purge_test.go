package filemanagement

import (
	"context"
	"encoding/json"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/ociruntime"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/testdocker"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func group(t *testing.T, root, id, campaign string) session.Metadata {
	t.Helper()
	m := session.Metadata{SchemaVersion: 1, ID: id, CampaignID: campaign, Phase: session.Stopped, ContainerID: testdocker.Retired(t, root, id)}
	p := session.PathsFor(root, id)
	if err := session.Save(p.Metadata, m); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"blobs/sha256/aa/body", "events.jsonl", "checkpoints/saved.json", "exports/report.tar"} {
		if err := fsutil.WriteFile(filepath.Join(p.Root, name), []byte("retained"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func TestPurgePreflightAndIndependentGroups(t *testing.T) {
	root := t.TempDir()
	docker, dockerRoot := testdocker.New(t)
	a := group(t, root, "sess-1-000000000000000000000000", "campaign")
	b := group(t, root, "sess-2-000000000000000000000000", "campaign")
	c := group(t, root, "sess-3-000000000000000000000000", "")
	_ = fsutil.WriteFile(filepath.Join(root, "audit", "credential-resolutions.jsonl"), []byte("audit"), 0600)
	_ = fsutil.WriteFile(filepath.Join(root, "apps", "keep"), []byte("image lock"), 0600)
	_ = fsutil.WriteFile(filepath.Join(root, "blobs", "old-body"), []byte("historical body"), 0600)
	shared, err := Lock(root, "maintenance", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := Purge(context.Background(), root, PurgeSelection{All: true}, docker, io.Discard); err == nil {
		t.Fatal("purged during live service/read")
	}
	shared.Close()
	for _, suffix := range []string{".present", ".running"} {
		_ = os.WriteFile(filepath.Join(dockerRoot, b.ContainerID+suffix), nil, 0600)
	}
	if err := Purge(context.Background(), root, PurgeSelection{All: true}, docker, io.Discard); err == nil {
		t.Fatal("purged live selection")
	}
	if _, _, err := session.Load(root, a.ID); err != nil {
		t.Fatal("partial deletion before complete preflight", err)
	}
	_ = os.Remove(filepath.Join(dockerRoot, b.ContainerID+".running"))
	if err := Purge(context.Background(), root, PurgeSelection{SessionID: a.ID}, docker, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.Load(root, b.ID); err != nil {
		t.Fatal("replacement removed", err)
	}
	if err := Purge(context.Background(), root, PurgeSelection{CampaignID: "campaign"}, docker, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.Load(root, c.ID); err != nil {
		t.Fatal("unassociated removed", err)
	}
	if _, err := os.Stat(filepath.Join(root, "audit", "credential-resolutions.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := Purge(context.Background(), root, PurgeSelection{All: true}, docker, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "audit")); !os.IsNotExist(err) {
		t.Fatal("audit survived all purge")
	}
	if _, err := os.Stat(filepath.Join(root, "blobs")); !os.IsNotExist(err) {
		t.Fatal("historical evidence survived all purge")
	}
	if _, err := os.Stat(filepath.Join(root, "apps", "keep")); err != nil {
		t.Fatal(err)
	}
	if err := Purge(context.Background(), root, PurgeSelection{All: true}, docker, io.Discard); err != nil {
		t.Fatal("not retryable", err)
	}
}

func TestPurgePrunesLifecycleInventoryAndPreservesExternalLinks(t *testing.T) {
	root := t.TempDir()
	docker, _ := testdocker.New(t)
	a := group(t, root, "sess-1-000000000000000000000000", "campaign")
	b := group(t, root, "sess-2-000000000000000000000000", "campaign")
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "keep"), []byte("export"), 0600)
	_ = os.Symlink(outside, filepath.Join(session.PathsFor(root, a.ID).Root, "outside"))
	path := filepath.Join(root, "instances", "inst-test.json")
	state := map[string]any{"sessions": map[string]any{a.ID: map[string]any{"session_id": a.ID}, b.ID: map[string]any{"session_id": b.ID}}, "active": map[string]any{"session_id": b.ID}, "records": map[string]any{"restore": map[string]any{"request": map[string]any{"session_id": a.ID}, "response": map[string]any{"body": map[string]any{"binding": map[string]any{"session_id": b.ID}}}}}}
	if err := fsutil.WriteJSON(path, state, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Purge(context.Background(), root, PurgeSelection{SessionID: a.ID}, docker, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var got struct {
		Sessions map[string]any
		Records  map[string]any
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 1 || len(got.Records) != 0 {
		t.Fatal(string(raw))
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("followed external symlink", err)
	}
	if err := Purge(context.Background(), root, PurgeSelection{All: true}, docker, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("empty ledger survived")
	}
}

func TestPurgeAndSpoolCleanupPreserveEvidenceWhenDaemonUnconfirmed(t *testing.T) {
	for _, mode := range []string{"unavailable", "changed-daemon", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			docker, fake := testdocker.New(t)
			m := group(t, root, "sess-1-000000000000000000000000", "campaign")
			paths := session.PathsFor(root, m.ID)
			spool := filepath.Join(SpoolRoots(paths)[0], "pending")
			if err := fsutil.WriteFile(spool, []byte("retain"), 0600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "unavailable":
				os.WriteFile(filepath.Join(fake, "unavailable"), nil, 0600)
			case "changed-daemon":
				os.WriteFile(filepath.Join(fake, "daemon-id"), []byte("replacement"), 0600)
			case "legacy":
				os.Remove(ociruntime.BindingPath(root, m.ID))
			}
			if err := CleanupSpools(context.Background(), root, m, docker); err == nil {
				t.Fatal("cleaned unconfirmed spool")
			}
			if err := Purge(context.Background(), root, PurgeSelection{All: true}, docker, io.Discard); err == nil {
				t.Fatal("purged unconfirmed session")
			}
			for _, path := range []string{spool, paths.Metadata, filepath.Join(paths.Checkpoints, "saved.json")} {
				if _, err := os.Stat(path); err != nil {
					t.Fatal("evidence lost", err)
				}
			}
		})
	}
}

func TestStartupChecksOriginalBindingForUnixOrphanWithoutSpools(t *testing.T) {
	root := t.TempDir()
	docker, fake := testdocker.New(t)
	m := group(t, root, "sess-1-000000000000000000000000", "")
	m.BridgeTransport = "unix"
	originalID := m.ContainerID
	m.ContainerID = "a-mutable-name-from-session-metadata"
	if err := session.Save(session.PathsFor(root, m.ID).Metadata, m); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".present", ".running"} {
		os.WriteFile(filepath.Join(fake, originalID+suffix), nil, 0600)
	}
	t.Setenv("DOCKER_CONTEXT", "different")
	t.Setenv("DOCKER_HOST", "tcp://elsewhere:2375")
	if err := CleanupAbandoned(context.Background(), root, docker); err == nil {
		t.Fatal("started beside an orphan on the original daemon")
	}
	os.Remove(filepath.Join(fake, originalID+".running"))
	if err := CleanupAbandoned(context.Background(), root, docker); err != nil {
		t.Fatal(err)
	}
}
