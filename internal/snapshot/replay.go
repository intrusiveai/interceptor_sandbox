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

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func ReplayFilesystem(ctx context.Context, roots []Root, blobs *blobstore.Store, transactions []statejournal.Transaction, through uint64) error {
	if err := validateRoots(roots); err != nil {
		return err
	}
	for _, transaction := range transactions {
		if transaction.Seq > through {
			break
		}
		for _, operation := range transaction.Operations {
			switch operation.Kind {
			case "file_upsert", "file_delete", "directory_ensure", "directory_delete", "symlink_set":
				if err := applyFilesystemOperation(ctx, roots, blobs, operation); err != nil {
					return fmt.Errorf("replay transaction %d: %w", transaction.Seq, err)
				}
			}
		}
	}
	return nil
}

func ResetRoots(roots []Root) error {
	if err := validateRoots(roots); err != nil {
		return err
	}
	for _, root := range roots {
		absolute, err := filepath.Abs(root.HostPath)
		if err != nil {
			return err
		}
		info, err := os.Lstat(absolute)
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("mutable root %s is not a real directory", absolute)
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			entries, err := os.ReadDir(absolute)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if err := os.RemoveAll(filepath.Join(absolute, entry.Name())); err != nil {
					return fmt.Errorf("clear mutable root %s: %w", root.ContainerPath, err)
				}
			}
		} else if err := os.MkdirAll(absolute, 0o750); err != nil {
			return err
		}
	}
	return nil
}

func VerifyBlobs(blobs *blobstore.Store, transactions []statejournal.Transaction, through uint64) error {
	seen := map[string]struct{}{}
	for _, transaction := range transactions {
		if transaction.Seq > through {
			break
		}
		for _, operation := range transaction.Operations {
			if operation.Kind != "file_upsert" {
				continue
			}
			if _, exists := seen[operation.Blob]; exists {
				continue
			}
			if err := blobs.Verify(operation.Blob); err != nil {
				return err
			}
			seen[operation.Blob] = struct{}{}
		}
	}
	return nil
}

func applyFilesystemOperation(ctx context.Context, roots []Root, blobs *blobstore.Store, operation statejournal.Operation) error {
	hostPath, root, err := resolvePath(roots, operation.Path)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	switch operation.Kind {
	case "directory_ensure":
		if err := ensureSafeParent(root, filepath.Dir(hostPath)); err != nil {
			return err
		}
		if info, err := os.Lstat(hostPath); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("cannot replace non-directory %s", operation.Path)
		}
		if err := os.Mkdir(hostPath, os.FileMode(operation.Mode)); err != nil && !os.IsExist(err) {
			return err
		}
		return os.Chmod(hostPath, os.FileMode(operation.Mode))
	case "directory_delete":
		if err := ensureSafeParent(root, filepath.Dir(hostPath)); err != nil {
			return err
		}
		if err := os.Remove(hostPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case "file_delete":
		if err := ensureSafeParent(root, filepath.Dir(hostPath)); err != nil {
			return err
		}
		if err := os.Remove(hostPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case "symlink_set":
		if err := ensureSafeParent(root, filepath.Dir(hostPath)); err != nil {
			return err
		}
		if err := validateSymlink(root, hostPath, operation.Target); err != nil {
			return err
		}
		if err := os.Remove(hostPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Symlink(filepath.FromSlash(operation.Target), hostPath)
	case "file_upsert":
		if blobs == nil {
			return errors.New("blob store is required for file replay")
		}
		if err := ensureSafeParent(root, filepath.Dir(hostPath)); err != nil {
			return err
		}
		reader, object, err := blobs.Open(operation.Blob)
		if err != nil {
			return err
		}
		defer reader.Close()
		if object.Size != operation.Size {
			return fmt.Errorf("blob %s size does not match operation", operation.Blob)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		return fsutil.WriteFile(hostPath, data, os.FileMode(operation.Mode))
	default:
		return fmt.Errorf("unsupported filesystem operation %q", operation.Kind)
	}
}

func resolvePath(roots []Root, containerPath string) (string, string, error) {
	type match struct{ container, host string }
	matches := []match{}
	for _, root := range roots {
		if containerPath == root.ContainerPath || strings.HasPrefix(containerPath, root.ContainerPath+"/") {
			matches = append(matches, match{root.ContainerPath, root.HostPath})
		}
	}
	if len(matches) == 0 {
		return "", "", fmt.Errorf("path %s is outside declared mutable roots", containerPath)
	}
	sort.Slice(matches, func(i, j int) bool { return len(matches[i].container) > len(matches[j].container) })
	root, err := filepath.Abs(matches[0].host)
	if err != nil {
		return "", "", err
	}
	relative := strings.TrimPrefix(containerPath, matches[0].container)
	relative = strings.TrimPrefix(relative, "/")
	hostPath := filepath.Join(root, filepath.FromSlash(relative))
	resolved, err := filepath.Rel(root, hostPath)
	if err != nil || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return "", "", errors.New("resolved path escapes mutable root")
	}
	return hostPath, root, nil
}

func ensureSafeParent(root, parent string) error {
	relative, err := filepath.Rel(root, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("parent escapes mutable root")
	}
	current := root
	if relative == "." {
		return nil
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return fmt.Errorf("parent directory %s does not exist", current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("parent %s is not a real directory", current)
		}
	}
	return nil
}
