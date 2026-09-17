package hostrun

import (
	"encoding/json"
	"errors"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
	"os"
	"path/filepath"
)

// VerifyRestoreSource uses the replacement's owned provenance, never a parent path.
func VerifyRestoreSource(m session.Metadata, paths session.Paths, blobs *blobstore.Store) error {
	if m.ParentSessionID == "" {
		return nil
	}
	directory := filepath.Join(paths.Root, "restore-source")
	raw, err := os.ReadFile(filepath.Join(directory, "checkpoint.json"))
	if err != nil {
		return err
	}
	var checkpoint snapshot.Checkpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return err
	}
	if err := snapshot.VerifyCheckpoint(checkpoint); err != nil {
		return err
	}
	if checkpoint.ID != m.ParentCheckpoint || checkpoint.SourceSessionID != m.ParentSessionID || checkpoint.EnvironmentDigest != m.EnvironmentDigest || checkpoint.AppDigest != m.AppDigest {
		return errors.New("restore provenance identity mismatch")
	}
	tx, err := statejournal.VerifyFile(filepath.Join(directory, "state.jsonl"))
	if err != nil {
		return err
	}
	if len(tx) == 0 || tx[0].SessionID != checkpoint.SourceSessionID || uint64(len(tx)) != checkpoint.JournalSeq || tx[len(tx)-1].Hash != checkpoint.JournalHash {
		return errors.New("restore provenance journal mismatch")
	}
	return snapshot.VerifyBlobs(blobs, tx, checkpoint.JournalSeq)
}
