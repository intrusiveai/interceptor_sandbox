package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListDuringMetadataPublication(t *testing.T) {
	root := t.TempDir()
	id := "sess-1-0123456789abcdef01234567"
	paths := PathsFor(root, id)
	if err := os.MkdirAll(paths.Root, 0750); err != nil {
		t.Fatal(err)
	}
	// Deterministically represent the interval between directory creation and
	// publication of session.json, including an in-progress atomic write.
	if err := os.WriteFile(filepath.Join(paths.Root, ".interceptor-pending.tmp"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if sessions, err := List(root); err != nil || len(sessions) != 0 {
		t.Fatalf("unpublished session: %v, %v", sessions, err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 50; i++ {
			if err := Save(paths.Metadata, Metadata{SchemaVersion: 1, ID: id, Phase: Creating}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for i := 0; i < 100; i++ {
		if _, err := List(root); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sessions, err := List(root)
	if err != nil || len(sessions) != 1 || sessions[0].ID != id {
		t.Fatalf("published session: %v, %v", sessions, err)
	}
}

func TestListRejectsInvalidMetadata(t *testing.T) {
	for _, content := range []string{"{", `{"schema_version":1,"id":"wrong"}`, `{"schema_version":1,"id":"sess-1-0123456789abcdef01234567","unexpected":true}`} {
		t.Run(content, func(t *testing.T) {
			root := t.TempDir()
			paths := PathsFor(root, "sess-1-0123456789abcdef01234567")
			if err := os.MkdirAll(paths.Root, 0750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.Metadata, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := List(root); err == nil {
				t.Fatal("invalid metadata was ignored")
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		root := t.TempDir()
		paths := PathsFor(root, "sess-1-0123456789abcdef01234567")
		// A directory at the metadata path fails reads even when tests run as root.
		if err := os.MkdirAll(paths.Metadata, 0750); err != nil {
			t.Fatal(err)
		}
		if _, err := List(root); err == nil {
			t.Fatal("unreadable metadata was ignored")
		}
	})
}
