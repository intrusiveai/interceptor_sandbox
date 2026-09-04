package adaptive

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"

	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func RestoreRegistries(sessionRoot string, transactions []statejournal.Transaction) error {
	artifacts := map[string]ArtifactDescriptor{}
	attempts := map[string]AttemptContext{}
	for _, transaction := range transactions {
		for _, operation := range transaction.Operations {
			switch operation.Kind {
			case "artifact_put":
				var value ArtifactDescriptor
				if json.Unmarshal(operation.Value, &value) != nil || validateArtifactDescriptor(value) != nil {
					return errors.New("restored artifact registration is invalid")
				}
				if prior, ok := artifacts[value.Digest]; ok && prior != value {
					return errors.New("restored artifact identity is contradictory")
				}
				artifacts[value.Digest] = value
			case "attempt_put":
				var value AttemptContext
				if json.Unmarshal(operation.Value, &value) != nil || value.AttemptID == "" {
					return errors.New("restored attempt registration is invalid")
				}
				if prior, ok := attempts[value.AttemptID]; ok && prior.Digest != value.Digest {
					return errors.New("restored attempt identity is contradictory")
				}
				attempts[value.AttemptID] = value
			}
		}
	}
	artifactItems := make([]ArtifactDescriptor, 0, len(artifacts))
	for _, item := range artifacts {
		artifactItems = append(artifactItems, item)
	}
	sort.Slice(artifactItems, func(i, j int) bool { return artifactItems[i].Digest < artifactItems[j].Digest })
	attemptItems := make([]AttemptContext, 0, len(attempts))
	for _, item := range attempts {
		attemptItems = append(attemptItems, item)
	}
	sort.Slice(attemptItems, func(i, j int) bool { return attemptItems[i].AttemptIndex < attemptItems[j].AttemptIndex })
	if err := fsutil.WriteJSON(filepath.Join(sessionRoot, "artifacts.json"), artifactFile{SchemaVersion: 1, Artifacts: artifactItems}, 0o640); err != nil {
		return err
	}
	if err := fsutil.WriteJSON(filepath.Join(sessionRoot, "attempts.json"), attemptFile{SchemaVersion: 1, Attempts: attemptItems}, 0o640); err != nil {
		return err
	}
	return nil
}
