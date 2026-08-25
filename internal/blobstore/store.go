package blobstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
)

type Object struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type Store struct {
	root     string
	maxBytes int64
}

func Open(root string, maxBytes int64) (*Store, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("max blob size must be positive")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve blob root: %w", err)
	}
	if absolute == filepath.VolumeName(absolute)+string(filepath.Separator) {
		return nil, fmt.Errorf("blob root may not be a filesystem root")
	}
	if info, err := os.Lstat(absolute); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("blob root must be a real directory")
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect blob root: %w", err)
	} else if err := os.MkdirAll(absolute, 0o750); err != nil {
		return nil, fmt.Errorf("create blob root: %w", err)
	}
	return &Store{root: absolute, maxBytes: maxBytes}, nil
}

func (s *Store) Put(ctx context.Context, reader io.Reader) (Object, error) {
	temporary, err := os.CreateTemp(s.root, ".blob-*.tmp")
	if err != nil {
		return Object{}, fmt.Errorf("create blob temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	hashed := io.TeeReader(io.LimitReader(contextReader{ctx: ctx, reader: reader}, s.maxBytes+1), temporary)
	value, size, err := digest.Reader(hashed)
	if err != nil {
		temporary.Close()
		return Object{}, fmt.Errorf("store blob: %w", err)
	}
	if size > s.maxBytes {
		temporary.Close()
		return Object{}, fmt.Errorf("blob exceeds %d-byte limit", s.maxBytes)
	}
	if err := temporary.Chmod(0o640); err != nil {
		temporary.Close()
		return Object{}, fmt.Errorf("set blob mode: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return Object{}, fmt.Errorf("sync blob: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return Object{}, fmt.Errorf("close blob: %w", err)
	}

	destination, err := s.path(value)
	if err != nil {
		return Object{}, err
	}
	if info, err := os.Stat(destination); err == nil {
		if info.Size() != size {
			return Object{}, fmt.Errorf("existing blob %s has unexpected size", value)
		}
		if err := s.Verify(value); err != nil {
			return Object{}, err
		}
		return Object{Digest: value, Size: size}, nil
	} else if !os.IsNotExist(err) {
		return Object{}, fmt.Errorf("inspect destination blob: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return Object{}, fmt.Errorf("create blob shard: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return Object{}, fmt.Errorf("publish blob: %w", err)
	}
	if err := fsutil.SyncDir(filepath.Dir(destination)); err != nil {
		return Object{}, err
	}
	return Object{Digest: value, Size: size}, nil
}

func (s *Store) Open(value string) (io.ReadCloser, Object, error) {
	path, err := s.path(value)
	if err != nil {
		return nil, Object{}, err
	}
	if err := s.Verify(value); err != nil {
		return nil, Object{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, Object{}, fmt.Errorf("open blob: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, Object{}, fmt.Errorf("stat blob: %w", err)
	}
	return file, Object{Digest: value, Size: info.Size()}, nil
}

func (s *Store) Verify(value string) error {
	path, err := s.path(value)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect blob %s: %w", value, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("blob %s is not a regular file", value)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open blob %s: %w", value, err)
	}
	actual, _, hashErr := digest.Reader(file)
	file.Close()
	if hashErr != nil {
		return fmt.Errorf("hash blob %s: %w", value, hashErr)
	}
	if actual != value {
		return fmt.Errorf("blob digest mismatch: expected %s, got %s", value, actual)
	}
	return nil
}

func (s *Store) path(value string) (string, error) {
	hexValue, err := digest.Hex(value)
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.root, "sha256", hexValue[:2], hexValue[2:])
	relative, err := filepath.Rel(s.root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return "", fmt.Errorf("blob path escapes root")
	}
	return path, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(buffer)
	}
}
