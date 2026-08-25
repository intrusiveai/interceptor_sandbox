package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

func Materialize(environmentDirectory string, blueprint *environment.Blueprint, paths Paths, generation int) ([]snapshot.Root, error) {
	if blueprint == nil || generation <= 0 {
		return nil, errors.New("Blueprint and positive generation are required")
	}
	base := filepath.Join(paths.Generations, fmt.Sprintf("%d", generation), "runtime")
	if err := os.MkdirAll(base, 0o750); err != nil {
		return nil, err
	}
	roots := make([]snapshot.Root, 0, len(blueprint.Application.Persistence.MutableRoots))
	for _, containerPath := range blueprint.Application.Persistence.MutableRoots {
		hostPath := filepath.Join(base, filepath.FromSlash(strings.TrimPrefix(containerPath, "/")))
		if err := os.MkdirAll(hostPath, 0o770); err != nil {
			return nil, err
		}
		roots = append(roots, snapshot.Root{ContainerPath: containerPath, HostPath: hostPath})
	}
	for _, seed := range blueprint.Filesystem.Seed {
		if seed.ReadOnly {
			continue
		}
		destination, err := runtimePath(roots, seed.Target)
		if err != nil {
			return nil, err
		}
		source := filepath.Join(environmentDirectory, filepath.FromSlash(seed.Source))
		if err := copySeed(source, destination); err != nil {
			return nil, fmt.Errorf("materialize seed %s: %w", seed.Source, err)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].ContainerPath < roots[j].ContainerPath })
	return roots, nil
}

func RuntimeRoots(paths Paths, blueprint *environment.Blueprint, generation int) []snapshot.Root {
	base := filepath.Join(paths.Generations, fmt.Sprintf("%d", generation), "runtime")
	result := make([]snapshot.Root, 0, len(blueprint.Application.Persistence.MutableRoots))
	for _, containerPath := range blueprint.Application.Persistence.MutableRoots {
		result = append(result, snapshot.Root{ContainerPath: containerPath, HostPath: filepath.Join(base, filepath.FromSlash(strings.TrimPrefix(containerPath, "/")))})
	}
	return result
}

func runtimePath(roots []snapshot.Root, containerPath string) (string, error) {
	for _, root := range roots {
		if containerPath == root.ContainerPath || strings.HasPrefix(containerPath, root.ContainerPath+"/") {
			relative := strings.TrimPrefix(strings.TrimPrefix(containerPath, root.ContainerPath), "/")
			return filepath.Join(root.HostPath, filepath.FromSlash(relative)), nil
		}
	}
	return "", fmt.Errorf("seed target %s is outside mutable roots", containerPath)
}

func copySeed(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		if err := os.MkdirAll(destination, writableDirectoryMode(info.Mode())); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copySeed(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	case info.Mode().IsRegular():
		if err := os.MkdirAll(filepath.Dir(destination), 0o770); err != nil {
			return err
		}
		input, err := os.Open(source)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, writableFileMode(info.Mode()))
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		if filepath.IsAbs(target) {
			return errors.New("seed symlink target must be relative")
		}
		return os.Symlink(target, destination)
	default:
		return fmt.Errorf("unsupported seed type %s", info.Mode().Type())
	}
}

func writableDirectoryMode(mode os.FileMode) os.FileMode { return mode.Perm() | 0o770 }
func writableFileMode(mode os.FileMode) os.FileMode      { return mode.Perm() | 0o660 }
