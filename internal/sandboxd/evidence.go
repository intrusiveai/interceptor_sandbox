package sandboxd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
)

// executionWindows describe observation time, never causal provenance.
// Pending requests keep the value captured at entry even after a window closes.
type executionWindows struct {
	mu        sync.Mutex
	active    map[string]recording.Observation
	pending   map[string]int
	uncertain bool
}

func (s *Server) beginInvocation(ctx context.Context, invocation applicationInvocation) (func(), error) {
	s.windows.mu.Lock()
	defer s.windows.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.evidenceError(); err != nil {
		return nil, err
	}
	if s.windows.active == nil {
		s.windows.active = make(map[string]recording.Observation)
	}
	s.windows.active[invocation.TurnID] = recording.Observation{TurnID: invocation.TurnID, AttemptID: invocation.AttemptID, PayloadDigest: invocation.PayloadDigest, Window: "observed"}
	return func() {
		s.windows.mu.Lock()
		defer s.windows.mu.Unlock()
		delete(s.windows.active, invocation.TurnID)
		// A timeout does not prove the application stopped its background work.
		if ctx.Err() != nil {
			s.windows.uncertain = true
		}
	}, nil
}

func (s *Server) observeRequest(ctx context.Context) (context.Context, func()) {
	s.windows.mu.Lock()
	observation := recording.Observation{Window: "outside_turn"}
	if s.windows.uncertain {
		observation.Window = "uncertain_execution"
	} else if len(s.windows.active) > 1 {
		observation.Window = "ambiguous"
	} else {
		for _, value := range s.windows.active {
			observation = value
		}
	}
	if observation.TurnID != "" {
		if s.windows.pending == nil {
			s.windows.pending = make(map[string]int)
		}
		s.windows.pending[observation.TurnID]++
	}
	s.windows.mu.Unlock()
	return recording.WithObservation(ctx, observation), func() {
		s.windows.mu.Lock()
		defer s.windows.mu.Unlock()
		if observation.TurnID != "" {
			s.windows.pending[observation.TurnID]--
			if s.windows.pending[observation.TurnID] == 0 {
				delete(s.windows.pending, observation.TurnID)
			}
		}
	}
}

func (s *Server) pendingEvidence(id string) bool {
	s.windows.mu.Lock()
	defer s.windows.mu.Unlock()
	return s.windows.pending[id] > 0
}

// evidenceError does not attempt to record an event in the failing store.
func (s *Server) evidenceError() error {
	err := s.recorder.Err()
	if err == nil {
		return nil
	}
	s.metadataMu.Lock()
	defer s.metadataMu.Unlock()
	s.metadata.Phase = session.Error
	s.metadata.TerminationReason = err.Error()
	found := false
	for _, taint := range s.metadata.Taints {
		if taint == "evidence_incomplete" {
			found = true
		}
	}
	if !found {
		s.metadata.Taints = append(s.metadata.Taints, "evidence_incomplete")
	}
	if s.paths.Metadata != "" {
		if saveErr := session.Save(s.paths.Metadata, s.metadata); saveErr != nil {
			return fmt.Errorf("%w; failure status could not be persisted: %v", err, saveErr)
		}
	}
	return err
}

// evidenceResponse delays publication until required event writes have succeeded.
// All supported managed/model routes are non-streaming and already bounded.
type evidenceResponse struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	limit    int64
	recorder *recording.Writer
}

func newEvidenceResponse(limit int64, recorder *recording.Writer) *evidenceResponse {
	return &evidenceResponse{header: make(http.Header), limit: limit, recorder: recorder}
}
func (w *evidenceResponse) Header() http.Header { return w.header }
func (w *evidenceResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *evidenceResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if int64(w.body.Len()+len(data)) > w.limit {
		err := fmt.Errorf("response evidence exceeds %d-byte limit", w.limit)
		w.recorder.Fail(err)
		return 0, err
	}
	return w.body.Write(data)
}
func (w *evidenceResponse) publish(target http.ResponseWriter) {
	for name, values := range w.header {
		target.Header()[name] = values
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	target.WriteHeader(status)
	_, _ = target.Write(w.body.Bytes())
}

func (s *Server) observedHandler(next http.Handler, serviceID string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx, finish := s.observeRequest(request.Context())
		defer finish()
		if s.recorder == nil || s.blobs == nil {
			writeError(writer, 507, "evidence_storage_error", "required evidence collector is unavailable")
			return
		}
		if err := s.evidenceError(); err != nil {
			writeError(writer, 507, "evidence_storage_error", err.Error())
			return
		}
		// Capture before activity locking and body parsing; preserve snapshot lock order.
		s.trackActivity(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			if serviceID != "" {
				_, err := s.recorder.AppendContext(ctx, recording.Draft{Channel: recording.Evaluation, Layer: "service_ingress", Kind: "service_request", Provenance: recording.Application, Semantic: map[string]any{"service": serviceID, "method": request.Method, "path": request.URL.Path}})
				if err == nil {
					err = s.recorder.Flush()
				}
				if err != nil {
					writeError(writer, 507, "evidence_storage_error", s.evidenceError().Error())
					return
				}
			}
			capture := newEvidenceResponse(s.blueprint.Limits.MaxBlobBytes, s.recorder)
			next.ServeHTTP(capture, request)
			if s.recorder != nil {
				_ = s.recorder.Flush()
			}
			if err := s.evidenceError(); err != nil {
				writeError(writer, 507, "evidence_storage_error", err.Error())
				return
			}
			capture.publish(writer)
		})).ServeHTTP(writer, request.WithContext(ctx))
	})
}
