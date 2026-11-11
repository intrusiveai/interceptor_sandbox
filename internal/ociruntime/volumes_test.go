package ociruntime_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/ociruntime"
	"github.com/intrusive-ai/interceptor-sandbox/internal/testdocker"
)

func TestMutableVolumeOwnershipAndRetry(t *testing.T) {
	ctx := context.Background()
	d, fake := testdocker.New(t)
	d, err := d.Pin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	d, err = d.PrepareBinding(root, "sess-private")
	if err != nil {
		t.Fatal(err)
	}
	name, err := d.CreateMutableVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateMutableVolume(ctx, 0); err == nil {
		t.Fatal("reused volume accepted")
	}
	id, err := d.Create(ctx, ociruntime.RunConfig{Name: "interceptor-private", Image: "test:local", Mounts: []ociruntime.Mount{{Volume: true, Source: name, Target: "/state/app"}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(ociruntime.BindingPath(root, "sess-private"))
	if err != nil {
		t.Fatal(err)
	}
	var b ociruntime.Binding
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	if b.ContainerID != id || len(b.MutableVolumes) != 1 || b.MutableVolumes[0] != name {
		t.Fatal("container save lost volume binding", b)
	}
	if err := d.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := d.PurgeMutableVolumes(ctx); err == nil {
		t.Fatal("purged live target volume")
	}
	if err := d.EnsureRemoved(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	volumePath := filepath.Join(fake, name+".volume")
	original, err := os.ReadFile(volumePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(string(original), `"sess-private"`, `"sess-foreign"`, 1), strings.Replace(string(original), `"local"`, `"remote"`, 1)} {
		if err := os.WriteFile(volumePath, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if err := d.PurgeMutableVolumes(ctx); err == nil {
			t.Fatal("deleted unowned/remote volume")
		}
		if _, err := os.Stat(volumePath); err != nil {
			t.Fatal("unverified volume removed", err)
		}
	}
	if err := os.WriteFile(volumePath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake, "daemon-id"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.PurgeMutableVolumes(ctx); err == nil {
		t.Fatal("purged on wrong daemon")
	}
	if err := os.Remove(filepath.Join(fake, "daemon-id")); err != nil {
		t.Fatal(err)
	}
	if err := d.PurgeMutableVolumes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.PurgeMutableVolumes(ctx); err != nil {
		t.Fatal("purge retry failed", err)
	}
	if _, err := os.Stat(volumePath); !os.IsNotExist(err) {
		t.Fatal("volume survived", err)
	}
}

func TestMutableVolumeBeforeContainerFailureIsPurgeable(t *testing.T) {
	ctx := context.Background()
	d, fake := testdocker.New(t)
	d, err := d.Pin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	d, err = d.PrepareBinding(t.TempDir(), "sess-precreate")
	if err != nil {
		t.Fatal(err)
	}
	name, err := d.CreateMutableVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PurgeMutableVolumes(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fake, name+".volume")); !os.IsNotExist(err) {
		t.Fatal("pre-create volume leaked", err)
	}
}
