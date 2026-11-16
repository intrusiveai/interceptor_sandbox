package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

// RequireOwnership rejects legacy checkpoints rather than inventing ownership.
// Legacy journals/manifests remain readable for evidence and integrity checks.
func RequireOwnership(transactions []statejournal.Transaction) error {
	for _, tx := range transactions {
		for _, op := range tx.Operations {
			if op.Kind == "environment_materialized" {
				var value struct {
					Schema int `json:"file_manifest_schema"`
				}
				if err := json.Unmarshal(op.Value, &value); err != nil {
					return err
				}
				if value.Schema == 2 {
					return nil
				}
				return errors.New("snapshot lacks logical filesystem ownership; create a new session and snapshot with this Interceptor release")
			}
		}
	}
	return errors.New("snapshot is missing filesystem ownership provenance")
}

// RestoreManifest replays metadata without borrowing ownership from the machine
// doing the restore. Filesystem operations still use the existing journal.
func RestoreManifest(baseline Manifest, roots []Root, transactions []statejournal.Transaction, through uint64) (Manifest, error) {
	if err := VerifyManifest(baseline); err != nil {
		return Manifest{}, err
	}
	if baseline.SchemaVersion != 2 {
		return Manifest{}, errors.New("restore requires an ownership-aware baseline")
	}
	entries := entryMap(baseline.Entries)
	for _, tx := range transactions {
		if tx.Seq > through {
			break
		}
		for _, op := range tx.Operations {
			switch op.Kind {
			case "file_upsert", "directory_ensure", "symlink_set", "file_delete", "directory_delete":
			default:
				continue
			}
			if err := statejournal.ValidateOperation(op); err != nil {
				return Manifest{}, err
			}
			host, root, err := resolvePath(roots, op.Path)
			if err != nil {
				return Manifest{}, err
			}
			if op.Kind == "file_delete" || op.Kind == "directory_delete" {
				if host == root {
					return Manifest{}, errors.New("cannot delete a mutable root")
				}
				delete(entries, op.Path)
				continue
			}
			if op.Owner != SupervisorOwner && op.Owner != ApplicationOwner {
				return Manifest{}, fmt.Errorf("snapshot operation %s lacks logical ownership", op.Path)
			}
			entry := Entry{Path: op.Path, Owner: op.Owner, Mode: op.Mode}
			switch op.Kind {
			case "file_upsert":
				entry.Type = "file"
				entry.Blob = op.Blob
				entry.Size = op.Size
			case "directory_ensure":
				entry.Type = "directory"
			case "symlink_set":
				entry.Type = "symlink"
				entry.Target = op.Target
				if err := validateSymlink(root, host, op.Target); err != nil {
					return Manifest{}, err
				}
			}
			entries[op.Path] = entry
		}
	}
	m := Manifest{SchemaVersion: 2, Entries: make([]Entry, 0, len(entries))}
	for _, entry := range entries {
		m.Entries = append(m.Entries, entry)
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	m.Digest, _ = manifestDigest(m)
	return m, VerifyManifest(m)
}

// StageFilesystem leaves temporary host-owned, owner-readable files. Applying
// portable ownership roles is deferred to the container's pre-launch bootstrap.
func StageFilesystem(ctx context.Context, roots []Root, blobs *blobstore.Store, m Manifest) error {
	if err := VerifyManifest(m); err != nil {
		return err
	}
	if m.SchemaVersion != 2 {
		return errors.New("restore requires logical filesystem ownership")
	}
	ops, err := Delta(EmptyManifest(), m)
	if err != nil {
		return err
	}
	if err := ResetRoots(roots); err != nil {
		return err
	}
	for _, op := range ops {
		if op.Kind == "directory_ensure" {
			op.Mode = 0700
		} else if op.Kind == "file_upsert" {
			op.Mode = 0600
		}
		if err := applyFilesystemOperation(ctx, roots, blobs, op); err != nil {
			return err
		}
	}
	return VerifyContents(ctx, roots, m)
}

// VerifyContents checks the complete tree, including unexpected entries, without
// equating the staging machine's ownership or temporary modes to the manifest.
func VerifyContents(ctx context.Context, roots []Root, m Manifest) error {
	if err := VerifyManifest(m); err != nil {
		return err
	}
	read := RootReader(roots)
	expected := entryMap(m.Entries)
	seen := map[string]bool{}
	var walk func(string) error
	walk = func(path string) error {
		if seen[path] {
			return nil
		}
		seen[path] = true
		entry, ok := expected[path]
		if !ok {
			return fmt.Errorf("unexpected restored entry %s", path)
		}
		response, err := read(ctx, ReadRequest{Path: path, Kind: "stat"})
		if err != nil {
			return err
		}
		info := response.Info
		mode := os.FileMode(info.Mode)
		switch entry.Type {
		case "directory":
			if !mode.IsDir() {
				return fmt.Errorf("restored directory type mismatch: %s", path)
			}
			for offset := int64(0); ; {
				page, err := read(ctx, ReadRequest{Path: path, Kind: "list", Offset: offset})
				if err != nil {
					return err
				}
				for _, name := range page.Names {
					if err := walk(filepath.Join(path, name)); err != nil {
						return err
					}
				}
				offset += int64(len(page.Names))
				if page.EOF {
					break
				}
			}
		case "file":
			if !mode.IsRegular() || info.Links != 1 || info.Size != entry.Size {
				return fmt.Errorf("restored file type/size mismatch: %s", path)
			}
			value, _, err := digest.Reader(&chunkReader{ctx: ctx, read: read, path: path, size: entry.Size})
			if err != nil {
				return err
			}
			if value != entry.Blob {
				return fmt.Errorf("restored file digest mismatch: %s", path)
			}
		case "symlink":
			if mode&os.ModeSymlink == 0 {
				return fmt.Errorf("restored symlink type mismatch: %s", path)
			}
			response, err := read(ctx, ReadRequest{Path: path, Kind: "link"})
			if err != nil {
				return err
			}
			if response.Target != entry.Target {
				return fmt.Errorf("restored symlink target mismatch: %s", path)
			}
		}
		return nil
	}
	for _, root := range roots {
		if err := walk(root.ContainerPath); err != nil {
			return err
		}
	}
	if len(seen) != len(expected) {
		return errors.New("restored manifest contains missing or out-of-root entries")
	}
	return nil
}

// ApplyOwnership runs before any application process starts. Callers must remove
// CHOWN, FOWNER and DAC_OVERRIDE before launching the supervisor/application.
func ApplyOwnership(ctx context.Context, roots []Root, m Manifest, ids Identities) error {
	if m.SchemaVersion != 2 {
		return errors.New("restore requires logical filesystem ownership")
	}
	if ids.SupervisorUID <= 0 || ids.ApplicationUID <= 0 || ids.SupervisorUID == ids.ApplicationUID || ids.SupervisorGID < 0 || ids.ApplicationGID < 0 {
		return errors.New("invalid restore identities")
	}
	if err := VerifyContents(ctx, roots, m); err != nil {
		return err
	}
	entries := append([]Entry(nil), m.Entries...)
	sort.Slice(entries, func(i, j int) bool { return pathDepth(entries[i].Path) > pathDepth(entries[j].Path) })
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		host, base, err := resolvePath(roots, entry.Path)
		if err != nil {
			return err
		}
		r, err := os.OpenRoot(base)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, host)
		if err != nil {
			r.Close()
			return err
		}
		uid, gid := ids.SupervisorUID, ids.SupervisorGID
		if entry.Owner == ApplicationOwner {
			uid, gid = ids.ApplicationUID, ids.ApplicationGID
		}
		err = r.Lchown(rel, uid, gid)
		if err == nil && entry.Type != "symlink" {
			err = r.Chmod(rel, os.FileMode(entry.Mode))
		}
		r.Close()
		if err != nil {
			return fmt.Errorf("restore ownership/mode %s: %w", entry.Path, err)
		}
		response, err := RootReader(roots)(ctx, ReadRequest{Path: entry.Path, Kind: "stat"})
		if err != nil {
			return err
		}
		if response.Info.UID != uid || response.Info.GID != gid || entry.Type != "symlink" && uint32(os.FileMode(response.Info.Mode).Perm()) != entry.Mode {
			return fmt.Errorf("filesystem did not preserve restored ownership/mode for %s", entry.Path)
		}
	}
	return nil
}
