package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/intrusive-ai/interceptor-sandbox/internal/appimage"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

// This bootstrap runs as container root before adopting the supervisor identity.
// No target code is started until its temporary restore capabilities are dropped.
func initializeMutablePermissions(ctx context.Context) error {
	lock, report, err := environment.Verify(containerEnvironment)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(containerSession, "session.json"))
	if err != nil {
		return err
	}
	var metadata session.Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return err
	}
	if metadata.EnvironmentDigest != lock.EnvironmentDigest {
		return errors.New("invalid restored session binding")
	}
	m, err := snapshot.LoadManifest(filepath.Join(containerSession, "file-manifest.json"))
	if err != nil {
		return err
	}
	if metadata.ParentSessionID != "" {
		data, err = os.ReadFile(filepath.Join(containerSession, "restore-source", "checkpoint.json"))
		if err != nil {
			return err
		}
		var checkpoint snapshot.Checkpoint
		if err := json.Unmarshal(data, &checkpoint); err != nil {
			return err
		}
		if err := snapshot.VerifyCheckpoint(checkpoint); err != nil {
			return err
		}
		if checkpoint.ID != metadata.ParentCheckpoint || checkpoint.SourceSessionID != metadata.ParentSessionID || checkpoint.EnvironmentDigest != metadata.EnvironmentDigest || checkpoint.AppDigest != metadata.AppDigest || checkpoint.FileManifestDigest != m.Digest {
			return errors.New("restored manifest does not match checkpoint")
		}
	} else {
		for _, entry := range m.Entries {
			if entry.Owner != snapshot.SupervisorOwner {
				return errors.New("fresh seed must belong to supervisor")
			}
		}
	}

	roots := []snapshot.Root{}
	for _, path := range report.Blueprint.Application.Persistence.MutableRoots {
		roots = append(roots, snapshot.Root{ContainerPath: path, HostPath: path})
	}
	if os.Getenv("INTERCEPTOR_NATIVE_MUTABLE") == "1" {
		blobRoot := containerBlobs
		if appimage.NeedsPrivateMounts(report.Blueprint) {
			blobRoot = "/run/interceptor-private/interceptor-blobs"
		}
		blobs, err := blobstore.Open(blobRoot, report.Blueprint.Limits.MaxBlobBytes)
		if err != nil {
			return err
		}
		if err := snapshot.StageFilesystem(ctx, roots, blobs, m); err != nil {
			return err
		}
	}
	return snapshot.ApplyOwnership(ctx, roots, m, snapshot.Identities{SupervisorUID: envInt("INTERCEPTOR_HOST_UID", 0), SupervisorGID: envInt("INTERCEPTOR_HOST_GID", 0), ApplicationUID: envInt("INTERCEPTOR_APP_UID", 20000), ApplicationGID: envInt("INTERCEPTOR_APP_GID", 20000)})
}
