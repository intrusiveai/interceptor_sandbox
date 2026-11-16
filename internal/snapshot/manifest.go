package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

type Root struct {
	ContainerPath string
	HostPath      string
}

type Entry struct {
	Path   string `json:"path"`
	Owner  string `json:"owner,omitempty"`
	Type   string `json:"type"`
	Mode   uint32 `json:"mode"`
	Blob   string `json:"blob,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Target string `json:"target,omitempty"`
}

type Manifest struct {
	SchemaVersion int     `json:"schema_version"`
	Entries       []Entry `json:"entries"`
	Digest        string  `json:"digest"`
}

func EmptyManifest() Manifest {
	manifest := Manifest{SchemaVersion: 2, Entries: []Entry{}}
	manifest.Digest, _ = manifestDigest(manifest)
	return manifest
}

func Scan(ctx context.Context, roots []Root, blobs *blobstore.Store, maxFileBytes int64) (Manifest, error) {
	read := RootReader(roots)
	return ScanWithReader(ctx, roots, blobs, 16<<40, Identities{SupervisorUID: os.Getuid(), SupervisorGID: os.Getgid(), ApplicationUID: -1, ApplicationGID: -1}, func(ctx context.Context, q ReadRequest) (ReadResponse, error) {
		reply, err := read(ctx, q)
		if err == nil && reply.Info != nil && os.FileMode(reply.Info.Mode).IsRegular() && reply.Info.Size > maxFileBytes {
			return ReadResponse{}, errors.New("file exceeds snapshot blob limit")
		}
		return reply, err
	})
}

func LoadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode file manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Manifest{}, errors.New("file manifest contains trailing or invalid JSON")
	}
	if err := VerifyManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func WriteManifest(path string, manifest Manifest) error {
	if err := VerifyManifest(manifest); err != nil {
		return err
	}
	return fsutil.WriteJSON(path, manifest, 0o640)
}

func VerifyManifest(manifest Manifest) error {
	if manifest.SchemaVersion != 1 && manifest.SchemaVersion != 2 {
		return fmt.Errorf("unsupported file manifest schema %d", manifest.SchemaVersion)
	}
	previous := ""
	for _, entry := range manifest.Entries {
		if entry.Path <= previous || !filepath.IsAbs(entry.Path) || filepath.Clean(entry.Path) != entry.Path {
			return fmt.Errorf("file manifest entries are not uniquely sorted or path %q is invalid", entry.Path)
		}
		previous = entry.Path
		if entry.Mode & ^uint32(0777) != 0 {
			return fmt.Errorf("invalid file mode for %s", entry.Path)
		}
		if manifest.SchemaVersion == 2 && entry.Owner != SupervisorOwner && entry.Owner != ApplicationOwner || manifest.SchemaVersion == 1 && entry.Owner != "" {
			return fmt.Errorf("invalid or missing ownership for %s", entry.Path)
		}

		switch entry.Type {
		case "file":
			if !digest.Valid(entry.Blob) || entry.Size < 0 || entry.Target != "" {
				return fmt.Errorf("invalid file manifest entry %s", entry.Path)
			}
		case "directory":
			if entry.Blob != "" || entry.Target != "" {
				return fmt.Errorf("invalid directory manifest entry %s", entry.Path)
			}
		case "symlink":
			if entry.Target == "" || filepath.IsAbs(entry.Target) || entry.Blob != "" {
				return fmt.Errorf("invalid symlink manifest entry %s", entry.Path)
			}
		default:
			return fmt.Errorf("unknown manifest entry type %q", entry.Type)
		}
	}
	expected, err := manifestDigest(manifest)
	if err != nil {
		return err
	}
	if manifest.Digest != expected {
		return fmt.Errorf("file manifest digest mismatch: expected %s, got %s", expected, manifest.Digest)
	}
	return nil
}

func Delta(previous, current Manifest) ([]statejournal.Operation, error) {
	if err := VerifyManifest(previous); err != nil {
		return nil, fmt.Errorf("verify previous manifest: %w", err)
	}
	if err := VerifyManifest(current); err != nil {
		return nil, fmt.Errorf("verify current manifest: %w", err)
	}
	old, next := entryMap(previous.Entries), entryMap(current.Entries)
	deletes := []Entry{}
	creates := []Entry{}
	for path, entry := range old {
		newEntry, exists := next[path]
		if !exists || newEntry.Type != entry.Type {
			deletes = append(deletes, entry)
		}
	}
	for path, entry := range next {
		oldEntry, exists := old[path]
		if !exists || oldEntry != entry {
			creates = append(creates, entry)
		}
	}
	sort.Slice(deletes, func(i, j int) bool {
		if pathDepth(deletes[i].Path) == pathDepth(deletes[j].Path) {
			return deletes[i].Path > deletes[j].Path
		}
		return pathDepth(deletes[i].Path) > pathDepth(deletes[j].Path)
	})
	sort.Slice(creates, func(i, j int) bool {
		if pathDepth(creates[i].Path) == pathDepth(creates[j].Path) {
			if creates[i].Type == "directory" && creates[j].Type != "directory" {
				return true
			}
			return creates[i].Path < creates[j].Path
		}
		return pathDepth(creates[i].Path) < pathDepth(creates[j].Path)
	})
	operations := make([]statejournal.Operation, 0, len(deletes)+len(creates))
	for _, entry := range deletes {
		kind := "file_delete"
		if entry.Type == "directory" {
			kind = "directory_delete"
		}
		operations = append(operations, statejournal.Operation{Kind: kind, Path: entry.Path})
	}
	for _, entry := range creates {
		switch entry.Type {
		case "directory":
			operations = append(operations, statejournal.Operation{Kind: "directory_ensure", Path: entry.Path, Mode: entry.Mode, Owner: entry.Owner})
		case "file":
			operations = append(operations, statejournal.Operation{Kind: "file_upsert", Path: entry.Path, Mode: entry.Mode, Owner: entry.Owner, Blob: entry.Blob, Size: entry.Size})
		case "symlink":
			operations = append(operations, statejournal.Operation{Kind: "symlink_set", Path: entry.Path, Target: entry.Target, Mode: entry.Mode, Owner: entry.Owner})
		}
	}
	return operations, nil
}

func manifestDigest(manifest Manifest) (string, error) {
	manifest.Digest = ""
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return digest.Bytes(encoded), nil
}

func entryMap(entries []Entry) map[string]Entry {
	result := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		result[entry.Path] = entry
	}
	return result
}

func pathDepth(path string) int {
	return strings.Count(filepath.Clean(path), string(filepath.Separator))
}

func validateRoots(roots []Root) error {
	if len(roots) == 0 {
		return errors.New("at least one mutable root is required")
	}
	seen := map[string]struct{}{}
	for _, root := range roots {
		if !filepath.IsAbs(root.ContainerPath) || filepath.Clean(root.ContainerPath) != root.ContainerPath || root.ContainerPath == "/" || root.HostPath == "" {
			return fmt.Errorf("invalid mutable root mapping %#v", root)
		}
		hostPath, err := filepath.Abs(root.HostPath)
		if err != nil || hostPath == filepath.VolumeName(hostPath)+string(filepath.Separator) {
			return fmt.Errorf("host mutable root %q is invalid or too broad", root.HostPath)
		}
		if _, exists := seen[root.ContainerPath]; exists {
			return fmt.Errorf("duplicate mutable root %s", root.ContainerPath)
		}
		seen[root.ContainerPath] = struct{}{}
	}
	return nil
}

func validateSymlink(root, path, target string) error {
	if filepath.IsAbs(target) || target == "" {
		return errors.New("symlink target must be relative")
	}
	destination := filepath.Clean(filepath.Join(filepath.Dir(path), target))
	relative, err := filepath.Rel(root, destination)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("symlink target escapes mutable root")
	}
	return nil
}

func linkCount(info os.FileInfo) uint64 {
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if value.IsValid() {
		field := value.FieldByName("Nlink")
		if field.IsValid() && field.CanUint() {
			return field.Uint()
		}
	}
	return 1
}
