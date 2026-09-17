package snapshot

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
)

const MaxDescriptionBytes = 4096

type Checkpoint struct {
	CampaignID           string    `json:"campaign_id,omitempty"`
	Description          string    `json:"description,omitempty"`
	SchemaVersion        int       `json:"schema_version"`
	ID                   string    `json:"id"`
	Label                string    `json:"label,omitempty"`
	SourceSessionID      string    `json:"source_session_id"`
	ParentCheckpoint     string    `json:"parent_checkpoint,omitempty"`
	EnvironmentDigest    string    `json:"environment_digest"`
	AppDigest            string    `json:"app_digest"`
	JournalSeq           uint64    `json:"journal_seq"`
	JournalHash          string    `json:"journal_hash"`
	FileManifestDigest   string    `json:"file_manifest_digest"`
	CanonicalSizeBytes   int64     `json:"canonical_size_bytes"`
	EventSeq             uint64    `json:"event_seq"`
	CreatedAt            time.Time `json:"created_at"`
	InterceptorVersion   string    `json:"interceptor_version"`
	JournalSchemaVersion int       `json:"journal_schema_version"`
	Status               string    `json:"status"`
	Hash                 string    `json:"hash"`
}

type Metadata struct {
	CampaignID  string
	Description string
}

// Empty campaign metadata remains readable for legacy/standalone checkpoints.
// Campaign APIs supply/check the associated campaign independently.
func ValidateMetadata(campaign, description string) error {
	if len(description) > MaxDescriptionBytes || !utf8.ValidString(description) {
		return errors.New("snapshot description must be valid UTF-8 and at most 4096 bytes")
	}
	if len(campaign) > 128 {
		return errors.New("invalid snapshot campaign ID")
	}
	for i, c := range campaign {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '_' || c == '-' || c == '.' || c == ':')) {
			return errors.New("invalid snapshot campaign ID")
		}
	}
	return nil
}

func NewCheckpoint(sessionID, parent, environmentDigest, appDigest string, journalSeq uint64, journalHash, manifestDigest string, canonicalSizeBytes int64, eventSeq uint64, label, interceptorVersion string, now time.Time, metadata ...Metadata) (Checkpoint, error) {
	id, err := checkpointID(now)
	if err != nil {
		return Checkpoint{}, err
	}
	checkpoint := Checkpoint{
		SchemaVersion: 1, ID: id, Label: label, SourceSessionID: sessionID, ParentCheckpoint: parent,
		EnvironmentDigest: environmentDigest, AppDigest: appDigest, JournalSeq: journalSeq,
		JournalHash: journalHash, FileManifestDigest: manifestDigest, CanonicalSizeBytes: canonicalSizeBytes, EventSeq: eventSeq,
		CreatedAt: now.UTC(), InterceptorVersion: interceptorVersion, JournalSchemaVersion: 1, Status: "ready",
	}
	if len(metadata) > 1 {
		return Checkpoint{}, errors.New("multiple checkpoint metadata values")
	}
	if len(metadata) == 1 {
		checkpoint.CampaignID = metadata[0].CampaignID
		checkpoint.Description = metadata[0].Description
	}
	if err := ValidateMetadata(checkpoint.CampaignID, checkpoint.Description); err != nil {
		return Checkpoint{}, err
	}
	checkpoint.Hash, err = checkpointDigest(checkpoint)
	return checkpoint, err
}

func WriteCheckpoint(directory string, checkpoint Checkpoint) error {
	if err := VerifyCheckpoint(checkpoint); err != nil {
		return err
	}
	return fsutil.WriteJSON(filepath.Join(directory, checkpoint.ID+".json"), checkpoint, 0o640)
}

func LoadCheckpoint(directory, id string) (Checkpoint, error) {
	if !validCheckpointID(id) {
		return Checkpoint{}, errors.New("invalid checkpoint ID")
	}
	data, err := os.ReadFile(filepath.Join(directory, id+".json"))
	if err != nil {
		return Checkpoint{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var checkpoint Checkpoint
	if err := decoder.Decode(&checkpoint); err != nil {
		return Checkpoint{}, fmt.Errorf("decode checkpoint: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Checkpoint{}, errors.New("checkpoint contains trailing or invalid JSON")
	}
	if checkpoint.ID != id {
		return Checkpoint{}, errors.New("checkpoint filename and identity differ")
	}
	if err := VerifyCheckpoint(checkpoint); err != nil {
		return Checkpoint{}, err
	}
	return checkpoint, nil
}

func ListCheckpoints(directory string) ([]Checkpoint, error) {
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return []Checkpoint{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := []Checkpoint{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validCheckpointID(id) {
			return nil, fmt.Errorf("unexpected checkpoint file %s", entry.Name())
		}
		checkpoint, err := LoadCheckpoint(directory, id)
		if err != nil {
			return nil, err
		}
		result = append(result, checkpoint)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func VerifyCheckpoint(checkpoint Checkpoint) error {
	if err := ValidateMetadata(checkpoint.CampaignID, checkpoint.Description); err != nil {
		return err
	}
	if checkpoint.SchemaVersion != 1 || checkpoint.JournalSchemaVersion != 1 || checkpoint.Status != "ready" || !validCheckpointID(checkpoint.ID) || checkpoint.SourceSessionID == "" || checkpoint.JournalSeq == 0 || checkpoint.InterceptorVersion == "" {
		return errors.New("checkpoint has invalid required fields")
	}
	for name, value := range map[string]string{"environment": checkpoint.EnvironmentDigest, "application": checkpoint.AppDigest, "journal": checkpoint.JournalHash, "manifest": checkpoint.FileManifestDigest} {
		if !digest.Valid(value) {
			return fmt.Errorf("checkpoint %s digest is invalid", name)
		}
	}
	expected, err := checkpointDigest(checkpoint)
	if err != nil {
		return err
	}
	if checkpoint.Hash != expected {
		return errors.New("checkpoint hash mismatch")
	}
	return nil
}

func checkpointDigest(checkpoint Checkpoint) (string, error) {
	checkpoint.Hash = ""
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return "", err
	}
	return digest.Bytes(encoded), nil
}

func checkpointID(now time.Time) (string, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return fmt.Sprintf("cp-%d-%s", now.UTC().UnixMilli(), hex.EncodeToString(random)), nil
}

func validCheckpointID(id string) bool {
	if !strings.HasPrefix(id, "cp-") || len(id) < len("cp-1-")+24 {
		return false
	}
	for _, character := range id[len("cp-"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') && character != '-' {
			return false
		}
	}
	return true
}
