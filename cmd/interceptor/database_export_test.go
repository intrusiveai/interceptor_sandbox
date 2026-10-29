package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

// Exercise the real exporter with a current database checkpoint and a restored
// session whose only backup references are in its retained source journal.
func TestExportDatabaseCheckpointBlobs(t *testing.T) {
	for _, restored := range []bool{false, true} {
		name := "current"
		if restored {
			name = "restored"
		}
		t.Run(name, func(t *testing.T) {
			for _, damage := range []string{"none", "missing", "corrupt", "size", "conflicting-size"} {
				t.Run(damage, func(t *testing.T) {
					must := func(err error) {
						t.Helper()
						if err != nil {
							t.Fatal(err)
						}
					}
					root := t.TempDir()
					env := filepath.Join(root, "environment")
					must(os.CopyFS(env, os.DirFS("../../examples/delivery-contract-environment")))
					lock, _, err := environment.Seal(env, time.Unix(0, 0))
					must(err)
					m := session.Metadata{SchemaVersion: 1, ID: "sess-1-000000000000000000000000", Phase: session.Stopped, EnvironmentPath: env, EnvironmentDigest: lock.EnvironmentDigest, AppDigest: lock.ApplicationDigest, FeedbackProfile: "black-box", Limits: session.Limits{MaxBlobBytes: 2 << 20}}
					p, e, j, err := session.Initialize(root, m, "")
					must(err)
					must(e.Close())
					must(snapshot.WriteManifest(p.FileManifest, snapshot.EmptyManifest()))
					store, err := blobstore.Open(p.Blobs, 2<<20)
					must(err)
					content := []byte("logical database backup fixture")
					backup, err := store.Put(t.Context(), bytes.NewReader(content))
					must(err)
					fileContent := []byte("historical file fixture")
					file, err := store.Put(t.Context(), bytes.NewReader(fileContent))
					must(err)
					size := backup.Size
					if damage == "size" {
						size++
					}
					ops := []statejournal.Operation{
						{Kind: "database_checkpoint", Service: "database", Blob: backup.Digest, Size: size, Value: json.RawMessage(`{"version":"18.6/0.8.7","oracle_baselines":{}}`)},
						{Kind: "database_checkpoint", Service: "another_database", Blob: backup.Digest, Size: size, Value: json.RawMessage(`{"version":"18.6/0.8.7","oracle_baselines":{}}`)},
						{Kind: "file_upsert", Path: "/state/app/old.txt", Blob: file.Digest, Size: file.Size, Mode: 0600},
					}
					if damage == "conflicting-size" {
						ops[1].Size++
					}
					tx, err := j.Append("database snapshot", ops)
					must(err)
					must(j.Close())
					cp, err := snapshot.NewCheckpoint(m.ID, "", m.EnvironmentDigest, m.AppDigest, tx.Seq, tx.Hash, snapshot.EmptyManifest().Digest, backup.Size, 0, "database", "test", time.Now().UTC())
					must(err)
					raw, err := json.Marshal(cp)
					must(err)
					must(os.WriteFile(filepath.Join(p.Checkpoints, cp.ID+".json"), raw, 0600))
					if restored {
						child := m
						child.ID = "sess-2-000000000000000000000000"
						child.ParentSessionID = m.ID
						child.ParentCheckpoint = cp.ID
						p2, e2, j2, err := session.Initialize(root, child, "")
						must(err)
						must(e2.Close())
						must(j2.Close())
						must(snapshot.WriteManifest(p2.FileManifest, snapshot.EmptyManifest()))
						must(os.CopyFS(p2.Blobs, os.DirFS(p.Blobs)))
						source := filepath.Join(p2.Root, "restore-source")
						must(os.Mkdir(source, 0700))
						must(os.WriteFile(filepath.Join(source, "checkpoint.json"), raw, 0600))
						journal, err := os.ReadFile(p.Journal)
						must(err)
						must(os.WriteFile(filepath.Join(source, "state.jsonl"), journal, 0600))
						m, p = child, p2
					}
					hexDigest := strings.TrimPrefix(backup.Digest, "sha256:")
					backupPath := filepath.Join(p.Blobs, "sha256", hexDigest[:2], hexDigest[2:])
					switch damage {
					case "missing":
						must(os.Remove(backupPath))
					case "corrupt":
						must(os.WriteFile(backupPath, bytes.Repeat([]byte("X"), len(content)), 0600))
					}
					output := filepath.Join(root, "evidence.tar")
					must(os.WriteFile(output, []byte("previous export"), 0600))
					err = exportEvidence(t.Context(), root, m, p, output, 4<<20)
					if damage != "none" {
						if err == nil {
							t.Fatal("invalid backup exported")
						}
						if strings.Contains(damage, "size") && !strings.Contains(err.Error(), "size") {
							t.Fatal("unexpected rejection", err)
						}
						previous, readErr := os.ReadFile(output)
						must(readErr)
						if string(previous) != "previous export" {
							t.Fatal("failed export replaced existing archive")
						}
						temps, err := filepath.Glob(filepath.Join(root, ".interceptor-export-*.tmp"))
						must(err)
						if len(temps) != 0 {
							t.Fatal("temporary export leaked", temps)
						}
						return
					}
					must(err)
					f, err := os.Open(output)
					must(err)
					defer f.Close()
					archive := tar.NewReader(f)
					expected := map[string][]byte{backup.Digest: content, file.Digest: fileContent}
					found := map[string]bool{}
					for {
						h, err := archive.Next()
						if err == io.EOF {
							break
						}
						must(err)
						if strings.HasPrefix(h.Name, "blobs/sha256/") {
							id := "sha256:" + strings.TrimPrefix(h.Name, "blobs/sha256/")
							data, err := io.ReadAll(archive)
							must(err)
							if found[id] || !bytes.Equal(data, expected[id]) {
								t.Fatal("duplicate or incorrect blob", id)
							}
							found[id] = true
						}
					}
					if len(found) != len(expected) {
						t.Fatal("missing journal backup/file", found)
					}
				})
			}
		})
	}
}
