// Package localapi implements the single-instance, trusted-local-host MVP API.
package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evidence"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

const Version = "interceptor.dev/local-lifecycle/v1alpha1"
const MaxLifecycleRecords = 1024

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

type Runtime interface {
	ListSnapshots(context.Context, string) ([]snapshot.Checkpoint, error)
	InspectSnapshot(context.Context, string, string) (snapshot.Checkpoint, error)
	Metadata(string) (session.Metadata, error)
	Execute(context.Context, string, adaptive.OperationRequest, json.RawMessage) (adaptive.GatewayResponse, error)
	ValidateRestore(context.Context, string, string) error
	Restore(context.Context, string, string) (session.Metadata, error)
	Stop(context.Context, string) error
	Export(context.Context, string, int64) (*os.File, error)
}
type Binding struct {
	SessionID        string `json:"session_id"`
	WorkerInstanceID string `json:"worker_instance_id"`
	RunRevision      uint64 `json:"run_revision"`
}
type AttachRequest struct {
	WorkerInstanceID string `json:"worker_instance_id"`
	APIVersion       string `json:"api_version"`
	CampaignID       string `json:"campaign_id"`
	AllowTargetStop  bool   `json:"allow_target_stop,omitempty"`
}
type LifecycleRequest struct {
	WorkerInstanceID string `json:"worker_instance_id"`
	RunRevision      uint64 `json:"run_revision"`
	APIVersion       string `json:"api_version"`
	CampaignID       string `json:"campaign_id"`
	SessionID        string `json:"session_id"`
	OperationID      string `json:"operation_id"`
	Operation        string `json:"operation"`
	CheckpointID     string `json:"checkpoint_id,omitempty"`
	SourceSessionID  string `json:"source_session_id,omitempty"`
}
type Record struct {
	Request     LifecycleRequest         `json:"request"`
	Fingerprint string                   `json:"fingerprint"`
	State       string                   `json:"state"`
	Response    adaptive.GatewayResponse `json:"response"`
}
type ExecutionFailure struct {
	SessionID  string    `json:"session_id"`
	Reason     string    `json:"reason"`
	ObservedAt time.Time `json:"observed_at"`
}

type State struct {
	Failure             *ExecutionFailure  `json:"failure,omitempty"`
	TransitionSessionID string             `json:"transition_session_id,omitempty"`
	APIVersion          string             `json:"api_version"`
	InstanceID          string             `json:"instance_id"`
	CampaignID          string             `json:"campaign_id,omitempty"`
	AllowTargetStop     bool               `json:"allow_target_stop"`
	Active              Binding            `json:"active"`
	Sessions            map[string]Binding `json:"sessions"`
	Phase               string             `json:"phase"`
	Closed              bool               `json:"closed"`
	Attached            bool               `json:"attached"`
	Records             map[string]*Record `json:"records"`
}
type Service struct {
	evidenceMaxBytes int64
	frameLimit       int64
	ctx              context.Context
	runtime          Runtime
	path             string
	mu               sync.Mutex
	gate             chan struct{}
	state            State
	failed           bool
	transportFailed  atomic.Bool
}

func New(ctx context.Context, runtime Runtime, metadata session.Metadata, instanceID, path string, evidenceMaxBytes int64) (*Service, error) {
	var err error
	evidenceMaxBytes, err = evidence.ResolveLimit(evidenceMaxBytes)
	if err != nil {
		return nil, err
	}
	if !identifier.MatchString(instanceID) || metadata.ID == "" || path == "" {
		return nil, errors.New("local API requires instance, session and ledger path")
	}
	b := Binding{SessionID: metadata.ID, RunRevision: 1}
	s := &Service{evidenceMaxBytes: evidenceMaxBytes, frameLimit: adaptive.OperationFrameLimit(metadata.Limits.MaxBlobBytes), ctx: ctx, runtime: runtime, path: path, gate: make(chan struct{}, 1), state: State{APIVersion: Version, InstanceID: instanceID, Active: b, Sessions: map[string]Binding{b.SessionID: b}, Phase: "ready", Records: map[string]*Record{}}}
	// An instance ledger is audit data, never a restart/ownership takeover mechanism.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return nil, errors.New("instance ledger already exists or is inaccessible")
	}
	return s, s.saveLocked()
}
func (s *Service) saveLocked() error {
	if s.transportFailed.Load() {
		s.state.Closed = true
		s.state.Phase = "error"
	}
	if err := fsutil.WriteJSON(s.path, s.state, 0600); err != nil {
		s.failed = true
		return err
	}
	return nil
}
func response(status int, body any) adaptive.GatewayResponse {
	raw, _ := json.Marshal(body)
	return adaptive.GatewayResponse{Status: status, Body: raw}
}
func write(w http.ResponseWriter, r adaptive.GatewayResponse) {
	w.Header().Set("Content-Type", "application/json")
	status := r.Status
	if status == http.StatusNoContent {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(r)
}
func reject(w http.ResponseWriter, status int, code string) {
	write(w, adaptive.OperationError(status, code))
}
func read(w http.ResponseWriter, r *http.Request, target any, limit int64) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		reject(w, 413, "frame_too_large")
		return false
	}
	if adaptive.StrictJSON(raw, target) != nil {
		reject(w, 400, "invalid_request")
		return false
	}
	return true
}
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if runtime, ok := s.runtime.(interface{ CheckHost() error }); ok {
		_ = runtime.CheckHost()
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		reject(w, 405, "method_not_allowed")
		return
	}
	switch r.URL.Path {
	case "/v1/attach":
		s.attach(w, r)
	case "/v1/operations":
		s.operation(w, r)
	case "/v1/lifecycle":
		s.lifecycle(w, r)
	case "/v1/status":
		s.status(w, r)
	case "/v1/snapshots/list":
		s.listSnapshots(w, r)
	case "/v1/snapshots/inspect":
		s.inspectSnapshot(w, r)
	case "/v1/evidence":
		s.evidence(w, r)
	default:
		reject(w, 404, "not_found")
	}
}
func (s *Service) lockGate(ctx context.Context) bool {
	select {
	case s.gate <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-s.ctx.Done():
		return false
	}
}
func (s *Service) unlockGate() { <-s.gate }
func (s *Service) attachedLocked(campaign string) bool {
	return campaign != "" && s.state.CampaignID == campaign
}
func (s *Service) attach(w http.ResponseWriter, r *http.Request) {
	var input AttachRequest
	if !read(w, r, &input, 4096) {
		return
	}
	if input.APIVersion != Version || !identifier.MatchString(input.CampaignID) || !identifier.MatchString(input.WorkerInstanceID) {
		reject(w, 400, "invalid_attach")
		return
	}
	if !s.lockGate(r.Context()) {
		reject(w, 503, "service_unavailable")
		return
	}
	defer s.unlockGate()
	s.mu.Lock()
	if s.failed {
		s.mu.Unlock()
		reject(w, 503, "instance_store_unavailable")
		return
	}
	if s.state.CampaignID != "" && !s.attachedLocked(input.CampaignID) {
		s.mu.Unlock()
		reject(w, 409, "campaign_mismatch")
		return
	}
	if s.state.CampaignID != "" && input.AllowTargetStop != s.state.AllowTargetStop {
		s.mu.Unlock()
		reject(w, 409, "campaign_policy_conflict")
		return
	}
	if s.state.Closed || s.transportFailed.Load() || s.state.Phase != "ready" {
		s.mu.Unlock()
		reject(w, 409, "instance_not_ready")
		return
	}
	s.state.CampaignID = input.CampaignID
	s.state.AllowTargetStop = input.AllowTargetStop
	if !s.state.Attached {
		s.state.Active.WorkerInstanceID = input.WorkerInstanceID
		s.state.Sessions[s.state.Active.SessionID] = s.state.Active
	}
	b := s.state.Active
	// Attribution on a new request need not match the session's initial caller.
	b.WorkerInstanceID = input.WorkerInstanceID
	attached := s.state.Attached
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		reject(w, 503, "instance_store_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	if !attached {
		result := s.owner(ctx, b, input.CampaignID, "bind", input.AllowTargetStop, "local-bind-"+b.SessionID)
		s.mu.Lock()
		if result.Status < 300 {
			s.state.Attached = true
		} else {
			s.state.Closed = true
			s.state.Phase = "error"
		}
		err = s.saveLocked()
		s.mu.Unlock()
		if err != nil {
			reject(w, 503, "instance_store_unavailable")
			return
		}
		if result.Status >= 300 {
			write(w, result)
			return
		}
	}
	metadata, err := s.runtime.Metadata(b.SessionID)
	if err != nil {
		reject(w, 503, "session_unavailable")
		return
	}
	caps := s.native(ctx, b, input.CampaignID, "capabilities.read", "local-capabilities-"+b.SessionID, []byte(`{}`))
	if caps.Status >= 300 {
		write(w, caps)
		return
	}
	write(w, response(200, map[string]any{"api_version": Version, "campaign_id": input.CampaignID, "binding": b, "session": metadata, "capabilities": caps.Body, "evidence_max_bytes": s.evidenceMaxBytes}))
}
func (s *Service) native(ctx context.Context, b Binding, campaign, op, id string, body []byte) adaptive.GatewayResponse {
	m, err := s.runtime.Metadata(b.SessionID)
	if err != nil {
		return adaptive.OperationError(503, "session_unavailable")
	}
	req := adaptive.OperationRequest{APIVersion: adaptive.OperationRequestVersion, RequestID: id, OperationID: id, Operation: op, SessionID: b.SessionID, CampaignID: campaign, WorkerInstanceID: b.WorkerInstanceID, RunRevision: b.RunRevision, ExpectedSessionRevision: m.Revision, BodyDigest: digest.Bytes(body), Deadline: time.Now().Add(30 * time.Second)}
	result, err := s.runtime.Execute(ctx, s.state.InstanceID, req, body)
	if err != nil {
		return adaptive.OperationError(503, "session_unavailable_outcome_unknown")
	}
	return result
}
func (s *Service) owner(ctx context.Context, b Binding, campaign, action string, allow bool, id string) adaptive.GatewayResponse {
	body, _ := json.Marshal(adaptive.OwnerRequest{Action: action, SessionID: b.SessionID, CampaignID: campaign, WorkerInstanceID: b.WorkerInstanceID, AllowTargetStop: allow})
	return s.native(ctx, b, campaign, "session.owner", id, body)
}

// executionOpen checks the supervisor, not the API's cached campaign flag.
// It is called under the mutation gate, before planned native closure.
func (s *Service) executionOpen(ctx context.Context, b Binding, campaign string) string {
	result := s.native(ctx, b, campaign, "session.status", "local-execution-status-"+b.SessionID, []byte(`{}`))
	if result.Status != 200 {
		return "execution_state_unavailable"
	}
	var status struct {
		Session   session.Metadata         `json:"session"`
		Owner     *adaptive.ExecutionOwner `json:"owner"`
		Available bool                     `json:"execution_available"`
	}
	if adaptive.StrictJSON(result.Body, &status) != nil || status.Session.ID != b.SessionID || status.Owner == nil {
		return "execution_state_unavailable"
	}
	o := status.Owner
	if o.CampaignID != campaign {
		return "execution_state_unavailable"
	}
	if o.ClosedAt != nil {
		return "execution_closed"
	}
	if !status.Available || status.Session.Phase != session.Running {
		return "execution_state_unavailable"
	}
	return ""
}

func (s *Service) closeCampaign() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Closed = true
	return s.saveLocked()
}

func readOnly(op string) bool {
	switch op {
	case "session.status", "operation.status", "capabilities.read", "artifact.read", "observation.read", "observation.content.read":
		return true
	}
	return false
}
func (s *Service) operation(w http.ResponseWriter, r *http.Request) {
	var input adaptive.OperationEnvelope
	// Same fixed outer bound used by the native v1alpha2 journalled operation API.
	if !read(w, r, &input, s.frameLimit) {
		return
	}
	q := input.Request
	if adaptive.ValidateOperation(q, input.Body, q.SessionID) != nil || q.OperationID[:min(len(q.OperationID), 6)] == "local-" {
		reject(w, 400, "invalid_request")
		return
	}
	if q.Operation == "session.stop" {
		reject(w, 400, "use_lifecycle_stop")
		return
	}
	closing := false
	if q.Operation == "session.owner" {
		var owner adaptive.OwnerRequest
		if adaptive.StrictJSON(input.Body, &owner) != nil || owner.Action != "close_execution" || owner.SessionID != q.SessionID || owner.CampaignID != q.CampaignID || owner.AllowTargetStop {
			reject(w, 400, "use_attach_to_bind")
			return
		}
		if !time.Now().Before(q.Deadline) {
			reject(w, 408, "deadline_exceeded")
			return
		}
		closing = true
	}
	if !readOnly(q.Operation) && !closing {
		if !s.lockGate(r.Context()) {
			reject(w, 503, "service_unavailable")
			return
		}
		defer s.unlockGate()
	}
	s.mu.Lock()
	_, known := s.state.Sessions[q.SessionID]
	b := Binding{q.SessionID, q.WorkerInstanceID, q.RunRevision}
	if !s.attachedLocked(q.CampaignID) {
		s.mu.Unlock()
		reject(w, 403, "campaign_mismatch")
		return
	}
	if !known {
		s.mu.Unlock()
		reject(w, 409, "session_not_in_campaign")
		return
	}
	if q.SessionID != s.state.Active.SessionID && !readOnly(q.Operation) && !(closing && q.SessionID == s.state.TransitionSessionID) {
		s.mu.Unlock()
		reject(w, 409, "stale_session")
		return
	}
	if !readOnly(q.Operation) && !closing && (s.failed || s.transportFailed.Load() || s.state.Phase != "ready" || s.state.Closed && q.Operation != "injection.delete") {
		s.mu.Unlock()
		reject(w, 409, "execution_unavailable_or_closed")
		return
	}
	if closing {
		s.state.Closed = true
		if s.saveLocked() != nil {
			s.mu.Unlock()
			reject(w, 503, "instance_store_unavailable")
			return
		}
	}
	principal := s.state.InstanceID
	s.mu.Unlock()
	result, err := s.runtime.Execute(r.Context(), principal, q, input.Body)
	// Synchronize closure before releasing the mutation gate or replying. A
	// disconnected caller must not cancel the independent native-state check.
	if !readOnly(q.Operation) && !closing {
		uncertain := err != nil || result.Status >= 500
		if !uncertain {
			ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
			uncertain = s.executionOpen(ctx, b, q.CampaignID) != ""
			cancel()
		}
		if uncertain {
			if s.closeCampaign() != nil {
				reject(w, 503, "instance_store_unavailable")
				return
			}
		}
	}
	if err != nil {
		reject(w, 503, "session_unavailable_outcome_unknown")
		return
	}
	write(w, result)
}

// Lifecycle retries may come from another caller without repeating the effect.
func lifecycleFingerprint(input LifecycleRequest) string {
	input.WorkerInstanceID = ""
	input.RunRevision = 0
	raw, _ := json.Marshal(input)
	return digest.Bytes(raw)
}

func (s *Service) lifecycle(w http.ResponseWriter, r *http.Request) {
	var input LifecycleRequest
	if !read(w, r, &input, 8192) {
		return
	}
	if input.APIVersion != Version || !identifier.MatchString(input.WorkerInstanceID) || input.RunRevision == 0 || !identifier.MatchString(input.OperationID) || (input.Operation != "snapshot.restore" && input.Operation != "session.stop") || input.Operation == "session.stop" && (input.CheckpointID != "" || input.SourceSessionID != "") || input.Operation == "snapshot.restore" && !identifier.MatchString(input.CheckpointID) {
		reject(w, 400, "invalid_lifecycle_request")
		return
	}
	fingerprint := lifecycleFingerprint(input)
	s.mu.Lock()
	if !s.attachedLocked(input.CampaignID) {
		s.mu.Unlock()
		reject(w, 403, "campaign_mismatch")
		return
	}
	if old, ok := s.state.Records[input.OperationID]; ok {
		result := old.Response
		if lifecycleFingerprint(old.Request) != fingerprint {
			result = adaptive.OperationError(409, "operation_conflict")
		} else if old.State == "running" {
			result = adaptive.OperationError(202, "operation_in_progress")
		}
		s.mu.Unlock()
		write(w, result)
		return
	}
	if s.failed {
		s.mu.Unlock()
		reject(w, 503, "instance_store_unavailable")
		return
	}
	if input.SessionID != s.state.Active.SessionID {
		s.mu.Unlock()
		reject(w, 409, "stale_session")
		return
	}
	if (input.Operation != "session.stop" && (s.state.Phase != "ready" || s.state.Closed || s.transportFailed.Load())) || (input.Operation == "session.stop" && s.state.Phase == "transitioning") {
		s.mu.Unlock()
		reject(w, 409, "execution_unavailable_or_closed")
		return
	}
	if input.Operation == "session.stop" && !s.state.AllowTargetStop {
		s.mu.Unlock()
		reject(w, 403, "target_stop_not_permitted")
		return
	}
	if input.SourceSessionID == "" { /* identity remains exactly as submitted in the record */
	} else if _, ok := s.state.Sessions[input.SourceSessionID]; !ok {
		s.mu.Unlock()
		reject(w, 409, "source_session_not_in_campaign")
		return
	}
	stateBytes, _ := json.Marshal(s.state)
	if len(s.state.Records) >= MaxLifecycleRecords || len(stateBytes)+(4<<20) > (64<<20) {
		s.mu.Unlock()
		reject(w, 429, "lifecycle_capacity_exhausted")
		return
	}
	s.state.Records[input.OperationID] = &Record{Request: input, Fingerprint: fingerprint, State: "running", Response: adaptive.OperationError(202, "operation_in_progress")}
	s.state.Phase = "transitioning"
	s.state.TransitionSessionID = input.SessionID
	if s.saveLocked() != nil {
		s.mu.Unlock()
		reject(w, 503, "instance_store_unavailable")
		return
	}
	s.mu.Unlock()
	// Request disconnect does not cancel an admitted stop/restore. Keep the original
	// ID and query status or repeat the identical request if its response is lost.
	result := s.transition(input)
	s.mu.Lock()
	// Publish the new ready binding and the completed record together. A close
	// against the source remains valid throughout the admitted transition.
	if input.Operation == "snapshot.restore" && result.Status < 300 {
		if s.state.Closed || s.transportFailed.Load() {
			target := s.state.Active.SessionID
			s.mu.Unlock()
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			stopErr := s.runtime.Stop(cleanup, target)
			cancel()
			s.mu.Lock()
			s.state.Phase = "error"
			result = adaptive.OperationError(503, "execution_closed")
			if stopErr != nil {
				result = adaptive.OperationError(503, "target_stop_unconfirmed")
			}
		} else {
			s.state.Phase = "ready"
		}
	}
	s.state.TransitionSessionID = ""
	rec := s.state.Records[input.OperationID]
	rec.State = "completed"
	rec.Response = result
	if s.saveLocked() != nil {
		result = adaptive.OperationError(503, "lifecycle_outcome_unknown")
	}
	s.mu.Unlock()
	write(w, result)
}
func (s *Service) transition(input LifecycleRequest) adaptive.GatewayResponse {
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	defer cancel()
	if !s.lockGate(ctx) {
		return adaptive.OperationError(503, "service_stopping")
	}
	defer s.unlockGate()
	if input.Operation == "snapshot.restore" {
		if runtime, ok := s.runtime.(interface {
			HoldRestore(context.Context) (func(), error)
		}); ok {
			release, err := runtime.HoldRestore(ctx)
			if err != nil {
				_ = s.failTransport()
				return adaptive.OperationError(503, "host_lifetime_unavailable")
			}
			defer release()
		}
	}
	s.mu.Lock()
	b := s.state.Active
	closed := s.state.Closed || s.transportFailed.Load()
	allow := s.state.AllowTargetStop
	s.mu.Unlock()
	attribution := Binding{b.SessionID, input.WorkerInstanceID, input.RunRevision}
	source := input.SourceSessionID
	if source == "" {
		source = b.SessionID
	}
	fail := func(code string) adaptive.GatewayResponse {
		s.mu.Lock()
		s.state.Phase = "error"
		s.state.Closed = true
		s.mu.Unlock()
		return adaptive.OperationError(503, code)
	}
	if input.Operation == "snapshot.restore" {
		if closed {
			return fail("execution_closed")
		}
		current, err := s.runtime.Metadata(b.SessionID)
		if err != nil || current.Phase != session.Running {
			return fail("source_not_healthy")
		}

		// A running container can have permanently closed native execution.
		// Never use a fresh session to reopen that campaign, even if the API flag
		// missed the original closure or the native state can no longer be read.
		if code := s.executionOpen(ctx, attribution, input.CampaignID); code != "" {
			return fail(code)
		}

		if err := s.runtime.ValidateRestore(ctx, source, input.CheckpointID); err != nil {
			s.mu.Lock()
			closed := s.state.Closed || s.transportFailed.Load()
			if !closed {
				s.state.Phase = "ready"
			}
			s.mu.Unlock()
			if closed {
				return fail("execution_closed")
			}
			return adaptive.OperationError(409, "checkpoint_unavailable_or_incompatible")
		}
	}
	current, err := s.runtime.Metadata(b.SessionID)
	if err != nil {
		return fail("session_unavailable")
	}
	if current.Phase != session.Stopped && current.Phase != session.Error {
		result := s.owner(ctx, attribution, input.CampaignID, "close_execution", false, "local-close-"+digest.Bytes([]byte(input.OperationID))[7:39])
		if result.Status >= 300 {
			return fail("closure_unconfirmed")
		}
		if input.Operation == "snapshot.restore" {
			// Close is idempotent: a 200 alone could acknowledge an earlier fault.
			var owner adaptive.ExecutionOwner
			if adaptive.StrictJSON(result.Body, &owner) != nil || owner.ClosedAt == nil || owner.ClosureReason != "controller_closed_execution" {
				return fail("execution_closed_or_unconfirmed")
			}
		}
	}

	if err := s.runtime.Stop(ctx, b.SessionID); err != nil {
		return fail("target_stop_unconfirmed")
	}
	if input.Operation == "session.stop" {
		s.mu.Lock()
		if !s.transportFailed.Load() {
			s.state.Phase = "stopped"
		}
		s.state.Closed = true
		s.mu.Unlock()
		return response(200, map[string]any{"session_id": b.SessionID, "phase": "stopped"})
	}
	s.mu.Lock()
	closed = s.state.Closed || s.transportFailed.Load()
	s.mu.Unlock()
	if closed {
		return fail("execution_closed")
	}
	metadata, err := s.runtime.Restore(ctx, source, input.CheckpointID)
	if err != nil {
		if metadata.ID != "" {
			s.mu.Lock()
			next := Binding{metadata.ID, input.WorkerInstanceID, b.RunRevision + 1}
			s.state.Active = next
			s.state.Sessions[next.SessionID] = next
			s.mu.Unlock()
		}
		return fail("restore_failed")
	}
	next := Binding{metadata.ID, input.WorkerInstanceID, b.RunRevision + 1}
	// Record the replacement even if a concurrent close or failed bind requires
	// stopping it; its evidence must remain addressable.
	s.mu.Lock()
	s.state.Active = next
	s.state.Sessions[next.SessionID] = next
	closed = s.state.Closed || s.transportFailed.Load()
	err = s.saveLocked()
	s.mu.Unlock()
	if closed || err != nil {
		_ = s.runtime.Stop(ctx, next.SessionID)
		return fail("restore_closed_or_unrecorded")
	}
	result := s.owner(ctx, next, input.CampaignID, "bind", allow, "local-bind-"+next.SessionID)
	if result.Status >= 300 {
		_ = s.runtime.Stop(ctx, next.SessionID)
		return fail("restored_owner_unconfirmed")
	}
	s.mu.Lock()
	closed = s.state.Closed || s.transportFailed.Load()
	s.mu.Unlock()
	if closed {
		_ = s.runtime.Stop(ctx, next.SessionID)
		return fail("execution_closed")
	}
	metadata, err = s.runtime.Metadata(next.SessionID)
	if err != nil {
		return fail("restored_status_unavailable")
	}
	return response(200, map[string]any{"binding": next, "session": metadata, "source_session_id": source, "checkpoint_id": input.CheckpointID})
}
func (s *Service) status(w http.ResponseWriter, r *http.Request) {
	var input struct {
		APIVersion  string `json:"api_version"`
		CampaignID  string `json:"campaign_id"`
		OperationID string `json:"operation_id,omitempty"`
	}
	if !read(w, r, &input, 4096) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if input.APIVersion != Version || !s.attachedLocked(input.CampaignID) {
		reject(w, 403, "campaign_mismatch")
		return
	}
	if input.OperationID != "" {
		rec, ok := s.state.Records[input.OperationID]
		if !ok {
			reject(w, 404, "operation_not_recorded")
			return
		}
		write(w, response(200, rec))
		return
	}
	// Marshal under lock; never expose a live map to an encoder after unlocking.
	write(w, response(200, map[string]any{"instance_id": s.state.InstanceID, "campaign_id": s.state.CampaignID, "active": s.state.Active, "sessions": s.state.Sessions, "phase": s.state.Phase, "closed": s.state.Closed, "store_available": !s.failed, "evidence_max_bytes": s.evidenceMaxBytes, "failure": s.state.Failure}))
}
func (s *Service) rejectEvidenceLimit(w http.ResponseWriter) {
	write(w, response(413, map[string]any{"code": "evidence_limit_exceeded", "maximum_bytes": s.evidenceMaxBytes, "evidence_retained": true}))
}
func (s *Service) evidence(w http.ResponseWriter, r *http.Request) {
	var input struct {
		APIVersion string `json:"api_version"`
		CampaignID string `json:"campaign_id"`
		SessionID  string `json:"session_id"`
	}
	if !read(w, r, &input, 4096) {
		return
	}
	s.mu.Lock()
	_, known := s.state.Sessions[input.SessionID]
	allowed := s.attachedLocked(input.CampaignID)
	s.mu.Unlock()
	if input.APIVersion != Version || !allowed || !known {
		reject(w, 403, "session_not_in_campaign")
		return
	}
	file, err := s.runtime.Export(r.Context(), input.SessionID, s.evidenceMaxBytes)
	if err != nil {
		if errors.Is(err, evidence.ErrLimitExceeded) {
			s.rejectEvidenceLimit(w)
			return
		}
		reject(w, 409, "evidence_unavailable_or_target_not_stopped")
		return
	}
	defer file.Close()
	defer os.Remove(file.Name())
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		reject(w, 503, "evidence_read_failed")
		return
	}
	if info.Size() > s.evidenceMaxBytes {
		s.rejectEvidenceLimit(w)
		return
	}
	hash, _, err := digest.Reader(file)
	if err != nil {
		reject(w, 503, "evidence_read_failed")
		return
	}
	if _, err = file.Seek(0, 0); err != nil {
		reject(w, 503, "evidence_read_failed")
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("X-Content-SHA256", hash)
	w.Header().Set("X-Evidence-Max-Bytes", fmt.Sprint(s.evidenceMaxBytes))
	http.ServeContent(w, r, "evidence.tar", time.Time{}, file)
}

// Fault closure bypasses a stuck mutation gate. The latch closes admission even
// if persisting the ledger fails; Docker termination runs independently.
func (s *Service) failTransport() error {
	return s.failExecution(ExecutionFailure{Reason: "EXECUTION_UNAVAILABLE", ObservedAt: time.Now().UTC()})
}

func (s *Service) failExecution(failure ExecutionFailure) error {
	s.transportFailed.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Closed = true
	s.state.Phase = "error"
	if s.state.Failure == nil {
		if failure.SessionID == "" {
			failure.SessionID = s.state.Active.SessionID
		}
		s.state.Failure = &failure
	}
	return s.saveLocked()
}
