package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
)

const ReadChunkBytes = 64 << 10
const DirectoryPageEntries = 128
const MaxManifestEntries = 100000
const SupervisorOwner = "supervisor"
const ApplicationOwner = "application"

type Identities struct{ SupervisorUID, SupervisorGID, ApplicationUID, ApplicationGID int }
type ReadRequest struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Offset int64  `json:"offset,omitempty"`
}
type FileInfo struct {
	Mode  uint32 `json:"mode"`
	Size  int64  `json:"size"`
	UID   int    `json:"uid"`
	GID   int    `json:"gid"`
	Links uint64 `json:"links"`
}
type ReadResponse struct {
	Info   *FileInfo `json:"info,omitempty"`
	Names  []string  `json:"names,omitempty"`
	Data   []byte    `json:"data,omitempty"`
	Target string    `json:"target,omitempty"`
	EOF    bool      `json:"eof,omitempty"`
}
type ReadFunc func(context.Context, ReadRequest) (ReadResponse, error)

// RootReader opens only declared roots and never follows symlink path components.
// It is used unchanged by the supervisor and its application-identity helper.
func RootReader(roots []Root) ReadFunc {
	return func(ctx context.Context, q ReadRequest) (ReadResponse, error) {
		if err := ctx.Err(); err != nil {
			return ReadResponse{}, err
		}
		if err := validateRoots(roots); err != nil {
			return ReadResponse{}, err
		}
		if !filepath.IsAbs(q.Path) || filepath.Clean(q.Path) != q.Path || q.Offset < 0 {
			return ReadResponse{}, errors.New("invalid snapshot read path/offset")
		}
		host, base, err := resolvePath(roots, q.Path)
		if err != nil {
			return ReadResponse{}, err
		}
		info, err := os.Lstat(base)
		if err != nil {
			return ReadResponse{}, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ReadResponse{}, errors.New("snapshot root is not a real directory")
		}
		relative, err := filepath.Rel(base, host)
		if err != nil {
			return ReadResponse{}, err
		}
		// Opening a private root can fail for the supervisor; its identity helper is
		// then responsible for the entire operation, including its own root checks.
		r, err := os.OpenRoot(base)
		if err != nil {
			return ReadResponse{}, err
		}
		defer r.Close()
		parts := strings.Split(relative, string(filepath.Separator))
		for i := 1; i < len(parts); i++ {
			p := filepath.Join(parts[:i]...)
			v, e := r.Lstat(p)
			if e != nil {
				return ReadResponse{}, e
			}
			if !v.IsDir() || v.Mode()&os.ModeSymlink != 0 {
				return ReadResponse{}, errors.New("snapshot read traverses a non-directory")
			}
		}
		info, err = r.Lstat(relative)
		if err != nil {
			return ReadResponse{}, err
		}
		switch q.Kind {
		case "stat":
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return ReadResponse{}, errors.New("filesystem ownership is unavailable")
			}
			return ReadResponse{Info: &FileInfo{Mode: uint32(info.Mode()), Size: info.Size(), UID: int(stat.Uid), GID: int(stat.Gid), Links: linkCount(info)}}, nil
		case "link":
			if info.Mode()&os.ModeSymlink == 0 {
				return ReadResponse{}, errors.New("not a symlink")
			}
			target, err := r.Readlink(relative)
			return ReadResponse{Target: target}, err
		case "list", "read":
			if q.Kind == "list" && !info.IsDir() || q.Kind == "read" && !info.Mode().IsRegular() {
				return ReadResponse{}, errors.New("invalid snapshot read type")
			}
			f, err := r.OpenFile(relative, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return ReadResponse{}, err
			}
			defer f.Close()
			opened, err := f.Stat()
			if err != nil {
				return ReadResponse{}, err
			}
			if !os.SameFile(info, opened) {
				return ReadResponse{}, errors.New("snapshot entry changed while opening")
			}
			if q.Kind == "list" {
				if q.Offset > MaxManifestEntries {
					return ReadResponse{}, errors.New("snapshot directory entry limit exceeded")
				}
				for skipped := int64(0); skipped < q.Offset; {
					count := min(int64(DirectoryPageEntries), q.Offset-skipped)
					items, e := f.ReadDir(int(count))
					if e != nil && e != io.EOF {
						return ReadResponse{}, e
					}
					skipped += int64(len(items))
					if e == io.EOF {
						return ReadResponse{EOF: true}, nil
					}
				}
				items, e := f.ReadDir(DirectoryPageEntries)
				if e != nil && e != io.EOF {
					return ReadResponse{}, e
				}
				names := make([]string, 0, len(items))
				for _, item := range items {
					names = append(names, item.Name())
				}
				return ReadResponse{Names: names, EOF: e == io.EOF}, nil
			}
			data := make([]byte, ReadChunkBytes)
			n, e := f.ReadAt(data, q.Offset)
			if e != nil && e != io.EOF {
				return ReadResponse{}, e
			}
			return ReadResponse{Data: data[:n], EOF: e == io.EOF}, nil
		default:
			return ReadResponse{}, errors.New("unknown snapshot read operation")
		}
	}
}

type chunkReader struct {
	ctx          context.Context
	read         ReadFunc
	path         string
	offset, size int64
	buffer       []byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.buffer) == 0 {
		if r.offset == r.size {
			return 0, io.EOF
		}
		response, err := r.read(r.ctx, ReadRequest{Path: r.path, Kind: "read", Offset: r.offset})
		if err != nil {
			return 0, err
		}
		if len(response.Data) == 0 || len(response.Data) > ReadChunkBytes || int64(len(response.Data)) > r.size-r.offset {
			return 0, errors.New("invalid snapshot file chunk or file changed")
		}
		r.buffer = response.Data
		r.offset += int64(len(response.Data))
	}
	n := copy(p, r.buffer)
	r.buffer = r.buffer[n:]
	return n, nil
}

func ScanWithReader(ctx context.Context, roots []Root, blobs *blobstore.Store, maxBytes int64, ids Identities, read ReadFunc) (Manifest, error) {
	if blobs == nil || maxBytes <= 0 || read == nil {
		return Manifest{}, errors.New("snapshot storage, reader and positive limit required")
	}
	if err := validateRoots(roots); err != nil {
		return Manifest{}, err
	}
	entries := []Entry{}
	seen := map[string]bool{}
	total := int64(0)
	var walk func(string, string) error
	walk = func(path, root string) error {
		if seen[path] {
			return nil
		}
		seen[path] = true
		if len(seen) > MaxManifestEntries {
			return errors.New("snapshot manifest entry limit exceeded")
		}
		response, err := read(ctx, ReadRequest{Path: path, Kind: "stat"})
		if err != nil {
			return err
		}
		if response.Info == nil {
			return errors.New("missing snapshot entry metadata")
		}
		info := *response.Info
		mode := os.FileMode(info.Mode)
		if mode&(os.ModeSetuid|os.ModeSetgid) != 0 {
			return errors.New("setuid/setgid snapshot entry forbidden")
		}
		owner := ""
		if info.UID == ids.SupervisorUID && info.GID == ids.SupervisorGID {
			owner = SupervisorOwner
		} else if info.UID == ids.ApplicationUID && info.GID == ids.ApplicationGID {
			owner = ApplicationOwner
		} else {
			return fmt.Errorf("%s has unsupported owner %d:%d", path, info.UID, info.GID)
		}
		entry := Entry{Path: path, Mode: uint32(mode.Perm()), Owner: owner}
		switch {
		case mode.IsDir():
			entry.Type = "directory"
		case mode.IsRegular():
			if info.Links > 1 || info.Size < 0 {
				return fmt.Errorf("%s has hard links or invalid size", path)
			}
			if info.Size > maxBytes-total {
				return ErrBytesExhausted
			}
			object, err := blobs.Put(ctx, &chunkReader{ctx: ctx, read: read, path: path, size: info.Size})
			if err != nil {
				return err
			}
			if object.Size != info.Size {
				return errors.New("snapshot file size changed")
			}
			entry.Type = "file"
			entry.Blob = object.Digest
			entry.Size = object.Size
			total += object.Size
		case mode&os.ModeSymlink != 0:
			response, err := read(ctx, ReadRequest{Path: path, Kind: "link"})
			if err != nil {
				return err
			}
			if err := validateSymlink(root, path, response.Target); err != nil {
				return err
			}
			entry.Type = "symlink"
			entry.Target = response.Target
		default:
			return fmt.Errorf("%s has unsupported filesystem type", path)
		}
		entries = append(entries, entry)
		if entry.Type == "directory" {
			names := []string{}
			namesSeen := map[string]bool{}
			for offset := int64(0); ; {
				response, err := read(ctx, ReadRequest{Path: path, Kind: "list", Offset: offset})
				if err != nil {
					return err
				}
				if len(response.Names) > DirectoryPageEntries || len(response.Names) == 0 && !response.EOF {
					return errors.New("invalid snapshot directory page")
				}
				for _, name := range response.Names {
					if name == "." || name == ".." || name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00") || namesSeen[name] {
						return errors.New("invalid or duplicate snapshot directory entry")
					}
					namesSeen[name] = true
					names = append(names, name)
				}
				offset += int64(len(response.Names))
				if offset > MaxManifestEntries {
					return errors.New("snapshot directory entry limit exceeded")
				}
				if response.EOF {
					break
				}
			}
			sort.Strings(names)
			for _, name := range names {
				if err := walk(filepath.Join(path, name), root); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, root := range roots {
		if err := walk(root.ContainerPath, root.ContainerPath); err != nil {
			return Manifest{}, fmt.Errorf("scan mutable root %s: %w", root.ContainerPath, err)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	m := Manifest{SchemaVersion: 2, Entries: entries}
	m.Digest, _ = manifestDigest(m)
	return m, VerifyManifest(m)
}

// ReadContents reuses bounded, no-follow reads for file-state oracles. A failed
// or changing read is unavailable evidence, never a negative content match.
func ReadContents(ctx context.Context, read ReadFunc, path string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("file read limit must be positive")
	}
	result, err := read(ctx, ReadRequest{Path: path, Kind: "stat"})
	if err != nil {
		return nil, err
	}
	if result.Info == nil {
		return nil, errors.New("file metadata unavailable")
	}
	info := result.Info
	if !os.FileMode(info.Mode).IsRegular() || info.Links != 1 || info.Size < 0 || info.Size > maxBytes {
		return nil, errors.New("file oracle requires a single-link regular file within the blob size limit")
	}
	if info.Size == 0 {
		// Even an empty file must be opened: stat alone does not establish that
		// its contents are readable by either permitted identity.
		response, err := read(ctx, ReadRequest{Path: path, Kind: "read"})
		if err != nil {
			return nil, err
		}
		if len(response.Data) != 0 || !response.EOF {
			return nil, errors.New("empty oracle file changed while reading")
		}
		return []byte{}, nil
	}
	return io.ReadAll(&chunkReader{ctx: ctx, read: read, path: path, size: info.Size})
}
