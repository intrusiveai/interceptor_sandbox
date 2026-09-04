package adaptive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"sort"
	"sync"

	"github.com/gowebpki/jcs"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
)

type ArtifactRegistry struct {
	mu           sync.Mutex
	path         string
	store        *blobstore.Store
	maximum      int64
	maximumItems int
	items        map[string]ArtifactDescriptor
}
type artifactFile struct {
	SchemaVersion int                  `json:"schema_version"`
	Artifacts     []ArtifactDescriptor `json:"artifacts"`
}

// ArtifactEnvelopeLimit includes base64 expansion and a bounded amount of JSON
// descriptor overhead for the current registration representation.
func ArtifactEnvelopeLimit(maximumRawBytes int64) int64 {
	if maximumRawBytes <= 0 {
		return 0
	}
	return 4*((maximumRawBytes+2)/3) + (1 << 20)
}

func OpenArtifactRegistry(path string, store *blobstore.Store, maximum int64) (*ArtifactRegistry, error) {
	if store == nil || maximum <= 0 {
		return nil, errors.New("artifact registry requires a blob store and positive limit")
	}
	r := &ArtifactRegistry{path: path, store: store, maximum: maximum, maximumItems: 10000, items: map[string]ArtifactDescriptor{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	var file artifactFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&file) != nil || file.SchemaVersion != 1 || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("artifact registry is malformed")
	}
	if len(file.Artifacts) > r.maximumItems {
		return nil, errors.New("artifact registry exceeds configured item limit")
	}
	for _, item := range file.Artifacts {
		if err := validateArtifactDescriptor(item); err != nil {
			return nil, err
		}
		if item.SizeBytes > maximum {
			return nil, errors.New("artifact registry entry exceeds configured byte limit")
		}
		if _, ok := r.items[item.Digest]; ok {
			return nil, errors.New("artifact registry contains duplicate digest")
		}
		reader, object, err := store.Open(item.Digest)
		if err != nil {
			return nil, fmt.Errorf("verify registered artifact %s: %w", item.Digest, err)
		}
		_ = reader.Close()
		if object.Size != item.SizeBytes {
			return nil, fmt.Errorf("registered artifact %s has an inconsistent size", item.Digest)
		}
		r.items[item.Digest] = item
	}
	return r, nil
}

func (r *ArtifactRegistry) Register(ctx context.Context, declared ArtifactDescriptor, reader io.Reader) (ArtifactDescriptor, error) {
	if err := validateArtifactDescriptor(declared); err != nil {
		return ArtifactDescriptor{}, err
	}
	if declared.SizeBytes > r.maximum {
		return ArtifactDescriptor{}, errors.New("artifact exceeds registry limit")
	}
	bounded, err := io.ReadAll(io.LimitReader(reader, r.maximum+1))
	if err != nil {
		return ArtifactDescriptor{}, err
	}
	if int64(len(bounded)) > r.maximum {
		return ArtifactDescriptor{}, errors.New("artifact exceeds registry limit")
	}
	canonical, err := canonicalArtifact(bounded, declared.Canonicalization)
	if err != nil {
		return ArtifactDescriptor{}, err
	}
	if int64(len(canonical)) != declared.SizeBytes || digest.Bytes(canonical) != declared.Digest {
		return ArtifactDescriptor{}, errors.New("artifact digest or size does not match declared canonical bytes")
	}
	object, err := r.store.Put(ctx, bytes.NewReader(canonical))
	if err != nil {
		return ArtifactDescriptor{}, err
	}
	if object.Digest != declared.Digest {
		return ArtifactDescriptor{}, errors.New("committed artifact identity changed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.items[declared.Digest]; !exists && len(r.items) >= r.maximumItems {
		return ArtifactDescriptor{}, errors.New("artifact count limit is exhausted")
	}
	if existing, ok := r.items[declared.Digest]; ok {
		if existing != declared {
			return ArtifactDescriptor{}, errors.New("artifact digest is already registered with different metadata")
		}
		return existing, nil
	}
	r.items[declared.Digest] = declared
	if err := r.save(); err != nil {
		delete(r.items, declared.Digest)
		return ArtifactDescriptor{}, err
	}
	return declared, nil
}
func (r *ArtifactRegistry) SetMaximumItems(maximum int) error {
	if maximum < 1 || maximum > 200000 {
		return errors.New("artifact count limit is invalid")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.items) > maximum {
		return errors.New("artifact registry already exceeds requested limit")
	}
	r.maximumItems = maximum
	return nil
}
func (r *ArtifactRegistry) Get(value string) (ArtifactDescriptor, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[value]
	return item, ok
}
func (r *ArtifactRegistry) Rollback(item ArtifactDescriptor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.items[item.Digest]; ok && current == item {
		delete(r.items, item.Digest)
		return r.save()
	}
	return nil
}
func (r *ArtifactRegistry) List() []ArtifactDescriptor {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]ArtifactDescriptor, 0, len(r.items))
	for _, item := range r.items {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Digest < items[j].Digest })
	return items
}
func (r *ArtifactRegistry) save() error {
	return fsutil.WriteJSON(r.path, artifactFile{SchemaVersion: 1, Artifacts: r.ListUnlocked()}, 0o640)
}
func (r *ArtifactRegistry) ListUnlocked() []ArtifactDescriptor {
	items := make([]ArtifactDescriptor, 0, len(r.items))
	for _, item := range r.items {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Digest < items[j].Digest })
	return items
}
func validateArtifactDescriptor(item ArtifactDescriptor) error {
	if !digest.Valid(item.Digest) || item.SizeBytes < 0 || item.MediaType == "" || len(item.MediaType) > 255 {
		return errors.New("artifact descriptor has invalid digest, size, or media type")
	}
	if _, _, err := mime.ParseMediaType(item.MediaType); err != nil {
		return fmt.Errorf("artifact media type: %w", err)
	}
	if item.Canonicalization != "raw" && item.Canonicalization != "jcs-v1" {
		return errors.New("artifact canonicalization must be raw or jcs-v1")
	}
	return nil
}
func canonicalArtifact(body []byte, rule string) ([]byte, error) {
	if rule == "raw" {
		return append([]byte(nil), body...), nil
	}
	encoded, err := jcs.Transform(body)
	if err != nil {
		return nil, fmt.Errorf("canonical JSON artifact: %w", err)
	}
	return encoded, nil
}
