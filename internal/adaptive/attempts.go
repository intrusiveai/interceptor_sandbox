package adaptive

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
)

type AttemptRegistry struct {
	mu             sync.Mutex
	path, feedback string
	maximum        int
	artifacts      *ArtifactRegistry
	items          map[string]AttemptContext
}
type attemptFile struct {
	SchemaVersion int              `json:"schema_version"`
	Attempts      []AttemptContext `json:"attempts"`
}

func OpenAttemptRegistry(path, feedback string, maximum int, artifacts *ArtifactRegistry) (*AttemptRegistry, error) {
	if maximum <= 0 || artifacts == nil {
		return nil, errors.New("attempt registry requires positive limit and artifact registry")
	}
	r := &AttemptRegistry{path: path, feedback: feedback, maximum: maximum, artifacts: artifacts, items: map[string]AttemptContext{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	var file attemptFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&file) != nil || file.SchemaVersion != 1 || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("attempt registry is malformed")
	}
	if len(file.Attempts) > maximum {
		return nil, errors.New("attempt registry exceeds configured limit")
	}
	sort.Slice(file.Attempts, func(i, j int) bool { return file.Attempts[i].AttemptIndex < file.Attempts[j].AttemptIndex })
	for _, item := range file.Attempts {
		validated, err := r.validate(item)
		if err != nil {
			return nil, fmt.Errorf("attempt registry entry %q: %w", item.AttemptID, err)
		}
		r.items[validated.AttemptID] = validated
	}
	return r, nil
}
func (r *AttemptRegistry) Register(context AttemptContext) (AttemptContext, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.items) >= r.maximum {
		return AttemptContext{}, errors.New("attempt limit is exhausted")
	}
	context, err := r.validate(context)
	if err != nil {
		return AttemptContext{}, err
	}
	r.items[context.AttemptID] = context
	if err := r.save(); err != nil {
		delete(r.items, context.AttemptID)
		return AttemptContext{}, err
	}
	return context, nil
}

func (r *AttemptRegistry) validate(context AttemptContext) (AttemptContext, error) {
	if context.APIVersion != AttemptContextVersion || !boundedID(context.CampaignID) || !boundedID(context.ThreadID) || !boundedID(context.AttemptID) || context.ParentAttemptID != "" && !boundedID(context.ParentAttemptID) || context.Generation == 0 || context.AttemptIndex == 0 || context.CreatedAt.IsZero() {
		return AttemptContext{}, errors.New("attempt context has invalid required fields")
	}
	if !boundedID(context.Generator.Kind) || context.Generator.ReleaseDigest != "" && !digest.Valid(context.Generator.ReleaseDigest) {
		return AttemptContext{}, errors.New("attempt generator kind or release digest is invalid")
	}
	if len(context.StrategyProvenanceRef) > 512 || strings.ContainsAny(context.StrategyProvenanceRef, "\x00\r\n") {
		return AttemptContext{}, errors.New("attempt strategy provenance reference is not bounded")
	}
	if context.FeedbackProfile != r.feedback {
		return AttemptContext{}, errors.New("attempt feedback profile differs from immutable session policy")
	}
	if _, exists := r.items[context.AttemptID]; exists {
		return AttemptContext{}, errors.New("attempt ID is immutable and already registered")
	}
	var highest uint64
	for _, existing := range r.items {
		if existing.CampaignID != context.CampaignID {
			return AttemptContext{}, errors.New("attempt campaign differs from the session campaign")
		}
		if existing.AttemptIndex > highest {
			highest = existing.AttemptIndex
		}
	}
	if context.AttemptIndex <= highest {
		return AttemptContext{}, errors.New("attempt index must increase monotonically within the session")
	}
	artifact, ok := r.artifacts.Get(context.Payload.Digest)
	if !ok || artifact.SizeBytes != context.Payload.SizeBytes || artifact.MediaType != context.Payload.MediaType || artifact.Canonicalization != context.Payload.Canonicalization {
		return AttemptContext{}, errors.New("attempt payload does not match a verified artifact")
	}
	if context.ParentAttemptID != "" {
		parent, ok := r.items[context.ParentAttemptID]
		if !ok || parent.CampaignID != context.CampaignID || parent.ThreadID != context.ThreadID {
			return AttemptContext{}, errors.New("attempt parent must exist in the same campaign and thread")
		}
		if context.Generation != parent.Generation+1 || context.AttemptIndex <= parent.AttemptIndex {
			return AttemptContext{}, errors.New("attempt generation or index is not monotonic")
		}
	} else if context.Generation != 1 {
		return AttemptContext{}, errors.New("root attempt generation must be one")
	}
	claimedDigest := context.Digest
	context.Digest = ""
	context.Digest = DigestAttemptContext(context)
	if claimedDigest != "" && claimedDigest != context.Digest {
		return AttemptContext{}, errors.New("attempt context digest does not match canonical content")
	}
	return context, nil
}

func DigestAttemptContext(context AttemptContext) string {
	context.Digest = ""
	encoded, _ := json.Marshal(context)
	return digest.Bytes(encoded)
}
func (r *AttemptRegistry) Get(id string) (AttemptContext, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[id]
	return item, ok
}
func (r *AttemptRegistry) Rollback(item AttemptContext) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.items[item.AttemptID]; ok && current.Digest == item.Digest {
		delete(r.items, item.AttemptID)
		return r.save()
	}
	return nil
}
func (r *AttemptRegistry) List() []AttemptContext { r.mu.Lock(); defer r.mu.Unlock(); return r.list() }
func (r *AttemptRegistry) list() []AttemptContext {
	items := make([]AttemptContext, 0, len(r.items))
	for _, item := range r.items {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].AttemptIndex < items[j].AttemptIndex })
	return items
}
func (r *AttemptRegistry) save() error {
	return fsutil.WriteJSON(r.path, attemptFile{SchemaVersion: 1, Attempts: r.list()}, 0o640)
}
func boundedID(value string) bool {
	return value != "" && len(value) <= 128 && !strings.ContainsAny(value, "/\\\x00\r\n")
}
