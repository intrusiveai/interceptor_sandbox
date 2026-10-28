package environment

import (
	"fmt"
	"path"
	"strings"
)

func validateCodeState(b *Blueprint) error {
	c := b.Application.Interface.Code
	if c == nil || c.State == nil {
		return nil
	}
	s := c.State
	invalid := func(reason string) error { return fmt.Errorf("application.interface.code.state: %s", reason) }
	if b.Application.Interface.Adapter != "code" || !b.Application.Persistence.SnapshotCapable {
		return invalid("requires code interface and persistence.snapshot_capable")
	}
	if c.Concurrency > 1 {
		return invalid("requires concurrency 1")
	}
	if strings.TrimSpace(s.Save) == "" || strings.TrimSpace(s.Restore) == "" || strings.ContainsAny(s.Save+s.Restore, "\x00\r\n") {
		return invalid("save and restore must name callable methods")
	}
	if _, err := validateContainerPath("state.path", s.Path); err != nil || path.Clean(s.Path) != s.Path {
		return invalid("path must be a clean absolute container file path")
	}
	inside := false
	for _, root := range b.Application.Persistence.MutableRoots {
		if s.Path != root && pathWithin(s.Path, root) {
			inside = true
		}
	}
	if !inside {
		return invalid("path must be below a declared mutable root")
	}
	for _, root := range []string{b.Application.Launch.Workdir, "/tmp", "/proc", "/sys", "/dev", "/run", "/opt/interceptor", "/usr", "/bin", "/sbin", "/etc"} {
		if pathWithin(s.Path, root) {
			return invalid("path overlaps application code or a protected/runtime directory; use a dedicated state root")
		}
	}
	for _, namespace := range b.Filesystem.InjectionNamespaces {
		if pathWithin(s.Path, namespace.Root) || pathWithin(namespace.Root, s.Path) {
			return invalid("path overlaps a file injection namespace")
		}
	}
	for _, seed := range b.Filesystem.Seed {
		if seed.Target == s.Path || pathWithin(s.Path, seed.Target) || pathWithin(seed.Target, s.Path) {
			return invalid("path must not overlap a seeded filesystem target")
		}
	}
	return nil
}
