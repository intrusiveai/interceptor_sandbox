package session

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
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func DataRoot() (string, error) {
	if configured := os.Getenv("INTERCEPTOR_DATA_DIR"); configured != "" {
		return filepath.Abs(configured)
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(config, "interceptor"), nil
}

func NewMetadata(environmentPath string, lock *environment.Lock, blueprint *environment.Blueprint, now time.Time) (Metadata, error) {
	id, err := newID("sess")
	if err != nil {
		return Metadata{}, err
	}
	absolute, err := filepath.Abs(environmentPath)
	if err != nil {
		return Metadata{}, fmt.Errorf("resolve Environment path: %w", err)
	}
	return Metadata{
		SchemaVersion: 1, ID: id, EnvironmentPath: absolute,
		EnvironmentDigest: lock.EnvironmentDigest, AppImage: blueprint.Application.Image,
		AppDigest: lock.ApplicationDigest, Phase: Creating, Generation: 1,
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
		Limits: Limits{
			MaxTurns: blueprint.Limits.MaxTurns, MaxTokens: blueprint.Limits.MaxTokens,
			MaxWallSeconds: blueprint.Limits.MaxWallSeconds, MaxSnapshots: blueprint.Limits.MaxSnapshots,
			MaxBlobBytes: blueprint.Limits.MaxBlobBytes, MaxPIDs: blueprint.Limits.MaxPIDs,
			MemoryBytes: blueprint.Limits.MemoryBytes, CPUs: blueprint.Limits.CPUs,
		},
		Taints: []string{},
	}, nil
}

func Initialize(dataRoot string, metadata Metadata, baselineManifestDigest string) (Paths, *recording.Writer, *statejournal.Writer, error) {
	if baselineManifestDigest == "" {
		baselineManifestDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	}
	paths := PathsFor(dataRoot, metadata.ID)
	for _, directory := range []string{paths.Root, paths.Checkpoints, paths.Generations, paths.Logs, paths.Sockets} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return Paths{}, nil, nil, fmt.Errorf("create session directory: %w", err)
		}
	}
	if err := Save(paths.Metadata, metadata); err != nil {
		return Paths{}, nil, nil, err
	}
	events, err := recording.Open(paths.Events, metadata.ID)
	if err != nil {
		return Paths{}, nil, nil, err
	}
	journal, err := statejournal.Open(paths.Journal, metadata.ID)
	if err != nil {
		events.Close()
		return Paths{}, nil, nil, err
	}
	value, _ := json.Marshal(map[string]any{
		"environment_digest":            metadata.EnvironmentDigest,
		"application_digest":            metadata.AppDigest,
		"baseline_file_manifest_digest": baselineManifestDigest,
		"bridge_transport":              metadata.BridgeTransport,
	})
	if _, err := journal.Append("environment_materialized", []statejournal.Operation{{Kind: "environment_materialized", Value: value}}); err != nil {
		journal.Close()
		events.Close()
		return Paths{}, nil, nil, err
	}
	if _, err := events.Append(recording.Draft{
		Channel: recording.Control, Layer: "session", Kind: "session_created",
		Provenance: recording.SandboxControl, Semantic: map[string]any{
			"environment_digest": metadata.EnvironmentDigest, "application_digest": metadata.AppDigest, "bridge_transport": metadata.BridgeTransport,
		},
	}); err != nil {
		journal.Close()
		events.Close()
		return Paths{}, nil, nil, err
	}
	if err := events.Flush(); err != nil {
		journal.Close()
		events.Close()
		return Paths{}, nil, nil, err
	}
	return paths, events, journal, nil
}

func Save(path string, metadata Metadata) error {
	metadata.UpdatedAt = time.Now().UTC()
	return fsutil.WriteJSON(path, metadata, 0o640)
}

func Load(dataRoot, id string) (Metadata, Paths, error) {
	if !validSessionID(id) {
		return Metadata{}, Paths{}, errors.New("invalid session ID")
	}
	paths := PathsFor(dataRoot, id)
	data, err := os.ReadFile(paths.Metadata)
	if err != nil {
		return Metadata{}, Paths{}, fmt.Errorf("read session metadata: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var metadata Metadata
	if err := decoder.Decode(&metadata); err != nil {
		return Metadata{}, Paths{}, fmt.Errorf("decode session metadata: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Metadata{}, Paths{}, errors.New("session metadata contains trailing JSON")
	}
	if metadata.SchemaVersion != 1 || metadata.ID != id {
		return Metadata{}, Paths{}, errors.New("invalid session metadata identity or schema")
	}
	return metadata, paths, nil
}

func List(dataRoot string) ([]Metadata, error) {
	entries, err := os.ReadDir(filepath.Join(dataRoot, "sessions"))
	if os.IsNotExist(err) {
		return []Metadata{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	result := []Metadata{}
	for _, entry := range entries {
		if !entry.IsDir() || !validSessionID(entry.Name()) {
			continue
		}
		metadata, _, err := Load(dataRoot, entry.Name())
		if err != nil {
			return nil, err
		}
		result = append(result, metadata)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func Transition(metadata *Metadata, target Phase) error {
	allowed := map[Phase]map[Phase]bool{
		Creating: {Starting: true, Error: true}, Starting: {Running: true, Error: true},
		Running: {Quiescing: true, Stopping: true, Error: true}, Quiescing: {Running: true, Error: true},
		Stopping: {Stopped: true, Error: true}, Stopped: {}, Error: {Stopping: true, Stopped: true},
	}
	if !allowed[metadata.Phase][target] {
		return fmt.Errorf("invalid session phase transition %s -> %s", metadata.Phase, target)
	}
	metadata.Phase = target
	return nil
}

func newID(prefix string) (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}
	return fmt.Sprintf("%s-%x-%s", prefix, time.Now().UTC().UnixMilli(), hex.EncodeToString(raw)), nil
}

func validSessionID(id string) bool {
	if len(id) < len("sess-1-")+24 || !bytes.HasPrefix([]byte(id), []byte("sess-")) {
		return false
	}
	for _, character := range id[len("sess-"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') && character != '-' {
			return false
		}
	}
	return true
}
