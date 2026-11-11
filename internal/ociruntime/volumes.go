package ociruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
)

var mutableVolumePattern = regexp.MustCompile(`^interceptor-sess-[a-z0-9-]+-mutable-[0-9]+$`)

func validMutableVolumeName(name string) bool { return mutableVolumePattern.MatchString(name) }

func (d Docker) readBinding() (Binding, error) {
	data, err := os.ReadFile(d.bindingPath)
	if err != nil {
		return Binding{}, err
	}
	var b Binding
	if err := json.Unmarshal(data, &b); err != nil {
		return b, err
	}
	if b.SessionID != d.SessionID || b.Endpoint != d.Endpoint || b.DaemonID != d.DaemonID {
		return b, errors.New("host Docker binding changed")
	}
	return b, nil
}

// CreateMutableVolume records intent before creating a resource so interrupted
// preparation remains discoverable by purge. Labels and the pinned daemon, not
// a name alone, authorize subsequent use and removal.
func (d Docker) CreateMutableVolume(ctx context.Context, index int) (string, error) {
	if err := d.verifyDaemon(ctx); err != nil {
		return "", err
	}
	b, err := d.readBinding()
	if err != nil {
		return "", err
	}
	name := "interceptor-" + d.SessionID + "-mutable-" + strconv.Itoa(index)
	if index < 0 || !validMutableVolumeName(name) {
		return "", errors.New("invalid mutable volume identity")
	}
	if slices.Contains(b.MutableVolumes, name) {
		return "", errors.New("mutable volume creation already attempted")
	}
	existing, err := d.volumeExists(ctx, name)
	if err != nil {
		return "", err
	}
	if existing {
		return "", errors.New("mutable volume name already exists; refusing adoption")
	}
	b.MutableVolumes = append(b.MutableVolumes, name)
	if err := fsutil.WriteJSON(d.bindingPath, b, 0600); err != nil {
		return "", err
	}
	if _, err := d.output(ctx, "volume", "create", "--driver", "local", "--label", SessionLabel+"="+d.SessionID, name); err != nil {
		return "", err
	}
	return name, d.verifyMutableVolume(ctx, name)
}

func (d Docker) volumeExists(ctx context.Context, name string) (bool, error) {
	out, err := d.output(ctx, "volume", "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}")
	if err != nil {
		return false, err
	}
	names := strings.Fields(string(out))
	if len(names) == 0 {
		return false, nil
	}
	if len(names) != 1 || names[0] != name {
		return false, errors.New("unexpected Docker volume inventory")
	}
	return true, nil
}
func (d Docker) verifyMutableVolume(ctx context.Context, name string) error {
	if !validMutableVolumeName(name) || !strings.HasPrefix(name, "interceptor-"+d.SessionID+"-mutable-") {
		return errors.New("mutable volume/session identity mismatch")
	}
	out, err := d.output(ctx, "volume", "inspect", "--format", "{{json .}}", name)
	if err != nil {
		return err
	}
	var v struct {
		Name, Driver    string
		Labels, Options map[string]string
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return err
	}
	if v.Name != name || v.Driver != "local" || len(v.Options) != 0 || v.Labels[SessionLabel] != d.SessionID {
		return errors.New("mutable volume identity/driver mismatch")
	}
	return nil
}

func (d Docker) PurgeMutableVolumes(ctx context.Context) error {
	b, err := d.readBinding()
	if err != nil {
		return err
	}
	if len(b.MutableVolumes) == 0 {
		return nil
	}
	if err := d.verifyDaemon(ctx); err != nil {
		return err
	}
	// Remove only the owned, already-stopped target if it still holds its volumes.
	present, err := d.ContainerPresent(ctx, b.ContainerID)
	if err != nil {
		return err
	}
	if present {
		running, err := d.Running(ctx, b.ContainerID)
		if err != nil {
			return err
		}
		if running {
			return errors.New("cannot purge mutable volumes of a running target")
		}
		if err := d.EnsureRemoved(ctx, b.ContainerID, false); err != nil {
			return err
		}
	}
	names := []string{}
	for _, name := range b.MutableVolumes {
		if !validMutableVolumeName(name) || !strings.HasPrefix(name, "interceptor-"+d.SessionID+"-mutable-") {
			return errors.New("invalid saved mutable volume")
		}
		exists, err := d.volumeExists(ctx, name)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if err := d.verifyMutableVolume(ctx, name); err != nil {
			return err
		}
		names = append(names, name)
	}
	for _, name := range names {
		if _, err := d.output(ctx, "volume", "rm", name); err != nil {
			return fmt.Errorf("remove mutable volume %s: %w", name, err)
		}
	}
	return nil
}
