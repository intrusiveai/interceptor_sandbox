package services

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

type SinkResponse struct {
	Status int
	JSON   map[string]any
	Body   string
}

type SinkObservation struct {
	Sequence   uint64              `json:"sequence"`
	ObservedAt time.Time           `json:"observed_at"`
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Query      map[string][]string `json:"query"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
	BodyRef    string              `json:"body_ref,omitempty"`
}

type SinkHandler struct {
	serviceID string
	methods   map[string]struct{}
	paths     map[string]struct{}
	response  SinkResponse
	maxBytes  int64
	blobs     *blobstore.Store
	recorder  *recording.Writer
	now       func() time.Time
	mu        sync.RWMutex
	items     []SinkObservation
}

func NewSinkHandler(serviceID string, methods, paths []string, response SinkResponse, maxBytes int64, blobs *blobstore.Store, recorder *recording.Writer) (*SinkHandler, error) {
	if serviceID == "" || maxBytes <= 0 {
		return nil, fmt.Errorf("sink service ID and positive capture limit are required")
	}
	if response.Status < 100 || response.Status > 599 {
		return nil, fmt.Errorf("sink response status is invalid")
	}
	if response.Body != "" && response.JSON != nil {
		return nil, fmt.Errorf("sink response must choose body or JSON")
	}
	handler := &SinkHandler{serviceID: serviceID, methods: map[string]struct{}{}, paths: map[string]struct{}{}, response: response, maxBytes: maxBytes, blobs: blobs, recorder: recorder, now: time.Now, items: []SinkObservation{}}
	for _, method := range methods {
		method = strings.ToUpper(strings.TrimSpace(method))
		if method == "" {
			return nil, fmt.Errorf("sink method may not be empty")
		}
		handler.methods[method] = struct{}{}
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("sink path %q must be absolute", path)
		}
		handler.paths[path] = struct{}{}
	}
	return handler, nil
}

func (h *SinkHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if len(h.methods) > 0 {
		if _, allowed := h.methods[request.Method]; !allowed {
			writeServiceError(writer, http.StatusNotImplemented, "coverage_gap", "method is not configured for attacker sink")
			return
		}
	}
	if len(h.paths) > 0 {
		if _, allowed := h.paths[request.URL.Path]; !allowed {
			writeServiceError(writer, http.StatusNotImplemented, "coverage_gap", "path is not configured for attacker sink")
			return
		}
	}
	body, err := readBounded(request.Body, h.maxBytes)
	if err != nil {
		writeServiceError(writer, http.StatusRequestEntityTooLarge, "capture_limit", err.Error())
		return
	}
	bodyRef := ""
	if len(body) > 0 && h.blobs != nil {
		object, err := h.blobs.Put(request.Context(), strings.NewReader(string(body)))
		if err != nil {
			writeServiceError(writer, http.StatusInternalServerError, "recording_error", "unable to durably capture request")
			return
		}
		bodyRef = object.Digest
	}
	observation := SinkObservation{
		ObservedAt: h.now().UTC(), Method: request.Method, Path: request.URL.Path,
		Query: normalizedValues(request.URL.Query()), Headers: normalizedValues(request.Header), Body: string(body), BodyRef: bodyRef,
	}
	h.mu.Lock()
	observation.Sequence = uint64(len(h.items) + 1)
	h.items = append(h.items, observation)
	h.mu.Unlock()
	if h.recorder != nil {
		_, _ = h.recorder.Append(recording.Draft{
			Channel: recording.Attack, Layer: "attacker_sink", Kind: "attacker_observation", Provenance: recording.AttackerInfrastructure, RawRef: bodyRef,
			Semantic: map[string]any{"service": h.serviceID, "observation": observation.Sequence, "method": observation.Method, "path": observation.Path, "body": observation.Body},
		})
	}
	if h.response.JSON != nil {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(h.response.Status)
		_ = json.NewEncoder(writer).Encode(h.response.JSON)
		return
	}
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(h.response.Status)
	_, _ = writer.Write([]byte(h.response.Body))
}

func (h *SinkHandler) Observations() []SinkObservation {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]SinkObservation, len(h.items))
	copy(result, h.items)
	return result
}

func normalizedValues(values map[string][]string) map[string][]string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make(map[string][]string, len(values))
	for _, key := range keys {
		result[key] = append([]string(nil), values[key]...)
		sort.Strings(result[key])
	}
	return result
}
