package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func TestPrivateOwnershipRoundTrip(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	roots := []Root{{"/state/app", source}}
	for _, dir := range []string{"private", "private/nested"} {
		if err := os.Mkdir(filepath.Join(source, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "private/nested/value"), []byte("private-state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "seed"), []byte("seed-state"), 0660); err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	// Model the portable roles independently of the test runner's real uid. The
	// Docker regression separately exercises real, distinct process identities.
	ids := Identities{SupervisorUID: os.Getuid(), SupervisorGID: os.Getgid(), ApplicationUID: 20000, ApplicationGID: os.Getgid()}
	read := RootReader(roots)
	m, err := ScanWithReader(ctx, roots, blobs, 1024, ids, func(ctx context.Context, q ReadRequest) (ReadResponse, error) {
		r, e := read(ctx, q)
		if r.Info != nil && strings.Contains(q.Path, "/private") {
			r.Info.UID = 20000
		}
		return r, e
	})
	if err != nil {
		t.Fatal(err)
	}
	ops, err := Delta(EmptyManifest(), m)
	if err != nil {
		t.Fatal(err)
	}
	tx := []statejournal.Transaction{{Seq: 1, Operations: ops}}
	destination := []Root{{"/state/app", t.TempDir()}}
	restored, err := RestoreManifest(EmptyManifest(), destination, tx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Digest != m.Digest {
		t.Fatal("ownership/modes lost during journal replay")
	}
	if err := StageFilesystem(ctx, destination, blobs, restored); err != nil {
		t.Fatal(err)
	}
	expected := entryMap(m.Entries)
	if expected["/state/app/seed"].Owner != SupervisorOwner || expected["/state/app/private/nested/value"].Owner != ApplicationOwner || expected["/state/app/private/nested/value"].Mode != 0600 || expected["/state/app/private"].Mode != 0700 {
		t.Fatal(expected)
	}
	tampered := m
	tampered.Entries = append([]Entry(nil), m.Entries...)
	tampered.Entries[0].Owner = ApplicationOwner
	if VerifyManifest(tampered) == nil {
		t.Fatal("owner not protected by digest")
	}
	if err := os.WriteFile(filepath.Join(destination[0].HostPath, "extra"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if VerifyContents(ctx, destination, restored) == nil {
		t.Fatal("unexpected restored entry accepted")
	}
}

func TestSnapshotReaderBoundaries(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "private"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	read := RootReader([]Root{{"/state/app", root}})
	for _, path := range []string{"/etc/passwd", "/state/app/../private", "/state/app/escape/private"} {
		if _, err := read(context.Background(), ReadRequest{Path: path, Kind: "read"}); err == nil {
			t.Fatalf("read accepted %s", path)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "value"), []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	ids := Identities{SupervisorUID: os.Getuid(), SupervisorGID: os.Getgid(), ApplicationUID: -1, ApplicationGID: -1}
	if _, err := ScanWithReader(context.Background(), []Root{{"/state/app", root}}, blobs, 4, ids, read); err == nil {
		t.Fatal("capture allowance ignored")
	}
	if _, err := ScanWithReader(context.Background(), []Root{{"/state/app", root}}, blobs, 1024, ids, func(ctx context.Context, q ReadRequest) (ReadResponse, error) {
		r, e := read(ctx, q)
		if r.Info != nil {
			r.Info.UID = 12345
		}
		return r, e
	}); err == nil {
		t.Fatal("unknown owner accepted")
	}
}

func TestLegacyOwnershipExplicit(t *testing.T) {
	old := Manifest{SchemaVersion: 1, Entries: []Entry{}}
	old.Digest, _ = manifestDigest(old)
	if err := VerifyManifest(old); err != nil {
		t.Fatal("legacy evidence should remain verifiable", err)
	}
	for _, schema := range []int{0, 1, 2} {
		v, _ := json.Marshal(map[string]any{"file_manifest_schema": schema})
		tx := []statejournal.Transaction{{Seq: 1, Operations: []statejournal.Operation{{Kind: "environment_materialized", Value: v}}}}
		err := RequireOwnership(tx)
		if (err == nil) != (schema == 2) {
			t.Fatalf("schema %d: %v", schema, err)
		}
	}
	if _, err := RestoreManifest(old, []Root{{"/state/app", t.TempDir()}}, nil, 0); err == nil {
		t.Fatal("legacy ownership guessed")
	}
}

func TestOwnershipOnlyChangeIsJournaled(t *testing.T) {
	previous := Manifest{SchemaVersion: 2, Entries: []Entry{{Path: "/state/app", Type: "directory", Mode: 0700, Owner: SupervisorOwner}}}
	previous.Digest, _ = manifestDigest(previous)
	current := Manifest{SchemaVersion: 2, Entries: append([]Entry(nil), previous.Entries...)}
	current.Entries[0].Owner = ApplicationOwner
	current.Digest, _ = manifestDigest(current)
	ops, err := Delta(previous, current)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Kind != "directory_ensure" || ops[0].Owner != ApplicationOwner || ops[0].Mode != 0700 {
		t.Fatal("owner-only mutation lost", ops)
	}
	roots := []Root{{"/state/app", t.TempDir()}}
	got, err := RestoreManifest(previous, roots, []statejournal.Transaction{{Seq: 1, Operations: ops}}, 1)
	if err != nil || got.Digest != current.Digest {
		t.Fatal("owner-only replay mismatch", got, err)
	}
}

func TestSnapshotChunkAndDirectoryPageBoundaries(t *testing.T) {
	root := t.TempDir()
	roots := []Root{{"/state/app", root}}
	ctx := context.Background()
	content := strings.Repeat("large-private-file", ReadChunkBytes/4)
	if err := os.WriteFile(filepath.Join(root, "large"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < DirectoryPageEntries+1; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("entry-%03d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	blobs, err := blobstore.Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := Scan(ctx, roots, blobs, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != DirectoryPageEntries+3 {
		t.Fatal("directory page lost entries", len(manifest.Entries))
	}
	entry := entryMap(manifest.Entries)["/state/app/large"]
	if entry.Blob != digest.Bytes([]byte(content)) || entry.Size != int64(len(content)) {
		t.Fatal("chunked file changed", entry)
	}
	read := RootReader(roots)
	ids := Identities{SupervisorUID: os.Getuid(), SupervisorGID: os.Getgid(), ApplicationUID: -1, ApplicationGID: -1}
	for _, bad := range []string{"../escape", "/outside", "a/b"} {
		_, err := ScanWithReader(ctx, roots, blobs, 1<<20, ids, func(ctx context.Context, q ReadRequest) (ReadResponse, error) {
			if q.Kind == "list" {
				return ReadResponse{Names: []string{bad}, EOF: true}, nil
			}
			return read(ctx, q)
		})
		if err == nil {
			t.Fatalf("unsafe helper name accepted: %s", bad)
		}
	}
}
