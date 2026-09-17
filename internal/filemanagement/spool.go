package filemanagement

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/ociruntime"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
)

const DefaultSpoolMaxBytes int64 = 512 << 20
const SpoolSizeLimit = "SPOOL_SIZE_LIMIT"
const SpoolInspectionFailure = "SPOOL_INSPECTION_FAILED"

type SpoolFault struct {
	Reason string `json:"reason"`
	Bytes  int64  `json:"bytes"`
	Limit  int64  `json:"limit"`
}

func (f *SpoolFault) Error() string {
	return fmt.Sprintf("%s: measured %d bytes, limit %d", f.Reason, f.Bytes, f.Limit)
}

func ParseSpoolLimit(value string) (int64, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return 0, errors.New("spool-max-bytes must be a positive integer byte count")
	}
	return n, nil
}

// ScanSpools counts logical lengths in bounded batches. os.Root confines traversal
// even when the guest replaces a path during inspection. Symlinks/special files fail.
func ScanSpools(ctx context.Context, roots []string, limit int64) (int64, error) {
	var total int64
	var visit func(*os.Root, string, int) error
	visit = func(root *os.Root, name string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 128 {
			return errors.New("spool nesting limit exceeded")
		}
		info, err := root.Lstat(name)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if info.Size() > limit-total {
				if info.Size() > int64(^uint64(0)>>1)-total {
					total = int64(^uint64(0) >> 1)
				} else {
					total += info.Size()
				}
				return &SpoolFault{SpoolSizeLimit, total, limit}
			}
			total += info.Size()
			return nil
		}
		if !info.IsDir() {
			return errors.New("unexpected link or special spool file")
		}
		dir, err := root.OpenFile(name, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		defer dir.Close()
		for {
			names, err := dir.Readdirnames(128)
			for _, child := range names {
				if err := visit(root, filepath.Join(name, child), depth+1); err != nil {
					return err
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}
	for _, path := range roots {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return total, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return total, errors.New("spool root must be a directory")
		}
		root, err := os.OpenRoot(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return total, err
		}
		err = visit(root, ".", 0)
		root.Close()
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func CheckSpools(ctx context.Context, roots []string, limit int64) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		size, err := ScanSpools(ctx, roots, limit)
		if err != nil {
			var fault *SpoolFault
			if !errors.As(err, &fault) {
				err = &SpoolFault{SpoolInspectionFailure, size, limit}
			}
		}
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return &SpoolFault{SpoolInspectionFailure, 0, limit}
	}
}

func WatchSpools(ctx context.Context, roots []string, limit int64, fault func(error)) {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := CheckSpools(ctx, roots, limit); err != nil {
			if ctx.Err() == nil {
				fault(err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func SpoolRoots(paths session.Paths) []string {
	return []string{filepath.Join(paths.Sockets, "http-spool"), filepath.Join(paths.Sockets, "model-spool")}
}

// CleanupSpools requires confirmed Docker exit and exclusive access to host writers.
func CleanupSpools(ctx context.Context, root string, m session.Metadata, docker ociruntime.Docker) error {
	lock, err := Lock(root, "writers-"+m.ID, true)
	if err != nil {
		return err
	}
	defer lock.Close()
	name := m.ContainerID
	if name == "" {
		name = "interceptor-" + m.ID
	}
	present, err := docker.ContainerPresent(ctx, name)
	if err != nil {
		return err
	}
	if present {
		running, err := docker.Running(ctx, name)
		if err != nil {
			return err
		}
		if running {
			return errors.New("spool cleanup refused: container is active")
		}
	}
	// The stopped guest cannot race these checks. Never traverse a replaced
	// sockets directory, even when its symlink points at another managed session.
	paths := session.PathsFor(root, m.ID)
	group, err := os.OpenRoot(paths.Root)
	if err != nil {
		return err
	}
	defer group.Close()
	sockets, err := group.Lstat("sockets")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if sockets.Mode()&os.ModeSymlink != 0 {
		return group.Remove("sockets")
	}
	if !sockets.IsDir() {
		return errors.New("session sockets path is not a directory")
	}
	for _, lane := range []string{"http-spool", "model-spool"} {
		if err := group.RemoveAll(filepath.Join("sockets", lane)); err != nil {
			return err
		}
	}
	_ = group.Remove("cleanup-pending.json")
	return nil
}

func CleanupAbandoned(ctx context.Context, root string, docker ociruntime.Docker) error {
	sessions, err := session.List(root)
	if err != nil {
		return err
	}
	for _, m := range sessions {
		if m.BridgeTransport == "unix" {
			continue
		}
		if err := CleanupSpools(ctx, root, m, docker); err != nil {
			return fmt.Errorf("session %s cleanup unresolved: %w", m.ID, err)
		}
	}
	return nil
}
