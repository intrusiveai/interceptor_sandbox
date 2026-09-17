package adaptive

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

const ExecutionStateVersion = "interceptor.dev/execution-state/v1alpha2"
const MaxOperationRecords = 4096
const MaxOperationResponse = 4 << 20
const MaxExecutionStateBytes = 64 << 20

// ExecutionOwner is the historical wire name for campaign association and closure.
// Principal/worker/revision describe initial attachment, not exclusive authority.
type ExecutionOwner struct {
	Principal        string     `json:"principal"`
	CampaignID       string     `json:"campaign_id"`
	WorkerInstanceID string     `json:"worker_instance_id"`
	RunRevision      uint64     `json:"run_revision"`
	AllowTargetStop  bool       `json:"allow_target_stop"`
	BoundAt          time.Time  `json:"bound_at"`
	ClosedAt         *time.Time `json:"closed_at,omitempty"`
	ClosureReason    string     `json:"closure_reason,omitempty"`
}
type OperationRecord struct {
	CommandFingerprint string           `json:"command_fingerprint,omitempty"`
	Request            OperationRequest `json:"request"`
	Fingerprint        string           `json:"fingerprint"`
	State              string           `json:"state"`
	Response           GatewayResponse  `json:"response"`
	AdmittedAt         time.Time        `json:"admitted_at"`
	FinishedAt         *time.Time       `json:"finished_at,omitempty"`
}
type ExecutionState struct {
	ClosedAt      *time.Time                  `json:"closed_at,omitempty"`
	ClosureReason string                      `json:"closure_reason,omitempty"`
	APIVersion    string                      `json:"api_version"`
	SessionID     string                      `json:"session_id"`
	Owner         *ExecutionOwner             `json:"owner,omitempty"`
	Records       map[string]*OperationRecord `json:"records"`
}

// ExecutionStore has one writer: the session supervisor. Gateways only call it
// through the supervisor's private control channel. The file is atomically
// replaced and fsynced before dispatch and again before returning a result.
type ExecutionStore struct {
	mu       sync.Mutex
	gate     chan struct{}
	path     string
	state    ExecutionState
	poisoned error
	journal  *statejournal.Writer
}
type ExecutionHooks struct {
	// Check executes at dispatch under the admission lock; it must not call this store.
	Check        func(OperationRequest, json.RawMessage, ExecutionState) error
	Dispatch     func(context.Context, OperationRequest, json.RawMessage) GatewayResponse
	Revision     func() uint64
	OwnerChanged func(ExecutionOwner) error
}

func ReadExecutionState(root string) (ExecutionState, error) {
	f, err := os.Open(filepath.Join(root, "execution.json"))
	if err != nil {
		return ExecutionState{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ExecutionState{}, err
	}
	if info.Size() > MaxExecutionStateBytes {
		return ExecutionState{}, errors.New("execution state exceeds capacity")
	}
	raw := make([]byte, info.Size())
	if _, err = f.ReadAt(raw, 0); err != nil {
		return ExecutionState{}, err
	}
	var state ExecutionState
	if err = StrictJSON(raw, &state); err != nil {
		return state, err
	}
	if state.APIVersion != ExecutionStateVersion || state.SessionID == "" || state.Records == nil || len(state.Records) > MaxOperationRecords+2 {
		return state, errors.New("invalid execution state")
	}
	if state.Owner != nil && (!boundedID(state.Owner.Principal) || !boundedID(state.Owner.CampaignID) || !boundedID(state.Owner.WorkerInstanceID) || state.Owner.RunRevision == 0 || state.Owner.BoundAt.IsZero()) {
		return state, errors.New("invalid execution owner")
	}
	for id, rec := range state.Records {
		if rec == nil || id != rec.Request.OperationID || rec.Request.SessionID != state.SessionID || rec.Fingerprint != OperationFingerprint(rec.Request) || rec.Request.APIVersion != OperationRequestVersion || !boundedID(rec.Request.RequestID) || !digest.Valid(rec.Request.BodyDigest) || rec.Request.RunRevision == 0 || (rec.State != "running" && rec.State != "completed" && rec.State != "unknown") {
			return state, errors.New("invalid operation record")
		}
	}
	return state, nil
}
func OpenExecutionStore(root, sessionID string, journals ...*statejournal.Writer) (*ExecutionStore, error) {
	state, err := ReadExecutionState(root)
	if err != nil {
		return nil, err
	}
	if state.Owner == nil && len(state.Records) > 0 {
		return nil, errors.New("incomplete initial ownership; session cannot resume")
	}
	if state.SessionID != sessionID {
		return nil, errors.New("execution session mismatch")
	}
	s := &ExecutionStore{path: filepath.Join(root, "execution.json"), state: state, gate: make(chan struct{}, 1)}
	if len(journals) > 0 {
		s.journal = journals[0]
		transactions, err := statejournal.VerifyFile(filepath.Join(root, "state.jsonl"))
		if err != nil {
			return nil, err
		}
		if _, err = VerifyExecutionJournal(root, transactions); err != nil {
			return nil, err
		}
	}
	// A supervisor restart is terminal. A gateway restart never opens this store.
	if state.Owner != nil {
		s.closeLocked("session_supervisor_restarted")
		now := time.Now().UTC()
		for _, rec := range s.state.Records {
			if rec.State == "running" {
				rec.State = "unknown"
				rec.Response = OperationError(409, "outcome_unknown")
				rec.FinishedAt = &now
			}
		}
		if err = s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}
func OperationError(status int, code string) GatewayResponse {
	body, _ := json.Marshal(map[string]string{"code": code})
	return GatewayResponse{Status: status, Body: body}
}
func (s *ExecutionStore) saveLocked() error {
	if s.poisoned != nil {
		return s.poisoned
	}
	raw, err := json.Marshal(s.state)
	if err == nil && len(raw) > MaxExecutionStateBytes {
		err = errors.New("execution state capacity exhausted")
	}
	if err == nil {
		err = fsutil.WriteFile(s.path, raw, 0600)
	}
	if err == nil && s.journal != nil {
		value, _ := json.Marshal(map[string]string{"execution_state_digest": digest.Bytes(raw), "session_id": s.state.SessionID})
		_, err = s.journal.Append("execution_state", []statejournal.Operation{{Kind: "idempotency_commit", Value: value}})
	}
	if err != nil {
		s.poisoned = err
		s.closeLocked("execution_persistence_failed")
	}
	return err
}
func (s *ExecutionStore) closeLocked(reason string) {
	if s.state.ClosedAt == nil {
		now := time.Now().UTC()
		s.state.ClosedAt = &now
		s.state.ClosureReason = reason
	}
	if s.state.Owner != nil && s.state.Owner.ClosedAt == nil {
		now := time.Now().UTC()
		s.state.Owner.ClosedAt = &now
		s.state.Owner.ClosureReason = reason
	}
}
func (s *ExecutionStore) Close(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked(reason)
	return s.saveLocked()
}
func (s *ExecutionStore) State() ExecutionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s.state)
	var v ExecutionState
	_ = json.Unmarshal(raw, &v)
	return v
}
func (s *ExecutionStore) Bound() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.poisoned != nil || s.state.Owner != nil || s.state.ClosedAt != nil
}

// LocalMutation excludes campaign attachment while an unscoped local mutation runs.
func (s *ExecutionStore) LocalMutation(ctx context.Context) (func(), error) {
	if s.Bound() {
		return nil, errors.New("campaign session requires /v1/operations")
	}
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.Bound() {
		<-s.gate
		return nil, errors.New("campaign session requires /v1/operations")
	}
	return func() { <-s.gate }, nil
}
func CampaignScope(state ExecutionState, r OperationRequest) bool {
	o := state.Owner
	return state.SessionID == r.SessionID && o != nil && o.CampaignID == r.CampaignID
}
func executionMutation(op string) bool {
	switch op {
	case "session.owner", "artifact.register", "attempt.register", "injection.arm", "injection.delete", "application.invoke", "snapshot.create", "session.stop":
		return true
	}
	return false
}
func (s *ExecutionStore) Execute(principal string, r OperationRequest, body json.RawMessage, h ExecutionHooks) GatewayResponse {
	if !boundedID(principal) {
		return OperationError(400, "invalid_principal")
	}
	if err := ValidateOperation(r, body, s.state.SessionID); err != nil {
		return OperationError(400, "invalid_request")
	}
	if !executionMutation(r.Operation) {
		s.mu.Lock()
		ok := CampaignScope(s.state, r)
		unbound := s.state.Owner == nil
		s.mu.Unlock()
		if !ok && !(unbound && (r.Operation == "session.status" || r.Operation == "capabilities.read")) {
			return OperationError(403, "campaign_mismatch")
		}
		if !time.Now().Before(r.Deadline) {
			return OperationError(408, "deadline_exceeded")
		}
		if err := h.Check(r, body, s.State()); err != nil {
			return OperationError(400, err.Error())
		}
		ctx, cancel := context.WithDeadline(context.Background(), r.Deadline)
		defer cancel()
		return h.Dispatch(ctx, r, body)
	}
	var owner OwnerRequest
	if r.Operation == "session.owner" {
		if StrictJSON(body, &owner) != nil || owner.SessionID != r.SessionID || owner.CampaignID != r.CampaignID || (owner.Action != "bind" && owner.Action != "close_execution") || owner.Action == "close_execution" && owner.AllowTargetStop {
			return OperationError(400, "invalid_owner_request")
		}
	}
	fingerprint := OperationFingerprint(r)
	commandFingerprint := CommandFingerprint(r, body)
	s.mu.Lock()
	if s.poisoned != nil {
		s.mu.Unlock()
		return OperationError(503, "execution_store_unavailable")
	}
	if owner.Action == "bind" && s.state.ClosedAt != nil {
		s.mu.Unlock()
		return OperationError(409, "execution_closed")
	}
	if rec, ok := s.state.Records[r.OperationID]; ok {
		savedCommand := rec.CommandFingerprint
		if savedCommand == "" {
			savedCommand = CommandFingerprint(rec.Request, nil)
		}
		if savedCommand != commandFingerprint || !CampaignScope(s.state, r) {
			s.mu.Unlock()
			return OperationError(409, "operation_conflict")
		}
		result := rec.Response
		if rec.State == "running" {
			result = OperationError(202, "operation_in_progress")
		}
		s.mu.Unlock()
		return result
	}
	for _, rec := range s.state.Records {
		if rec.Request.RequestID == r.RequestID {
			s.mu.Unlock()
			return OperationError(409, "request_id_conflict")
		}
	}
	if !CampaignScope(s.state, r) && !(s.state.Owner == nil && owner.Action == "bind") {
		s.mu.Unlock()
		return OperationError(403, "campaign_mismatch")
	}
	if !time.Now().Before(r.Deadline) {
		s.mu.Unlock()
		return OperationError(408, "deadline_exceeded")
	}
	closing := owner.Action == "close_execution"
	if !closing && s.state.ClosedAt != nil && r.Operation != "injection.delete" && r.Operation != "session.stop" {
		s.mu.Unlock()
		return OperationError(409, "execution_closed")
	}
	if owner.Action == "bind" && s.state.Owner != nil {
		if s.state.Owner.AllowTargetStop != owner.AllowTargetStop {
			s.mu.Unlock()
			return OperationError(409, "campaign_policy_conflict")
		}
		response := s.ownerResponse(h.Revision())
		s.mu.Unlock()
		return response
	}
	raw, _ := json.Marshal(s.state)
	pending := 0
	for _, rec := range s.state.Records {
		if rec.State == "running" {
			pending++
		}
	}
	if !closing && (len(s.state.Records) >= MaxOperationRecords || len(raw)+(pending+1)*MaxOperationResponse > MaxExecutionStateBytes-(64<<10)) {
		s.mu.Unlock()
		return OperationError(429, "operation_capacity_exhausted")
	}
	if closing && s.state.Owner.ClosedAt != nil {
		response := s.ownerResponse(h.Revision())
		s.mu.Unlock()
		return response
	}
	rec := &OperationRecord{Request: r, Fingerprint: fingerprint, CommandFingerprint: commandFingerprint, State: "running", AdmittedAt: time.Now().UTC()}
	s.state.Records[r.OperationID] = rec
	if err := s.saveLocked(); err != nil {
		s.mu.Unlock()
		return OperationError(503, "execution_store_unavailable")
	}
	s.mu.Unlock()
	ctx, cancel := context.WithDeadline(context.Background(), r.Deadline)
	defer cancel()
	if !closing {
		select {
		case s.gate <- struct{}{}:
			defer func() { <-s.gate }()
		case <-ctx.Done():
			return s.finish(r.OperationID, OperationError(408, "deadline_exceeded"))
		}
	}
	s.mu.Lock()
	// Recheck after the queue wait. Closing is independent of the mutation gate.
	var reject string
	if s.poisoned != nil {
		reject = "execution_store_unavailable"
	} else if ctx.Err() != nil {
		reject = "deadline_exceeded"
	} else if s.state.Owner != nil && !CampaignScope(s.state, r) {
		reject = "campaign_mismatch"
	} else if s.state.ClosedAt != nil && !closing && r.Operation != "injection.delete" && r.Operation != "session.stop" {
		reject = "execution_closed"
	} else if !closing && (r.ExpectedSessionRevision == 0 || r.ExpectedSessionRevision != h.Revision()) {
		reject = "stale_session_revision"
	} else if r.Operation == "session.stop" && (s.state.Owner == nil || !s.state.Owner.AllowTargetStop) {
		reject = "target_stop_not_permitted"
	}
	if reject == "" && owner.Action == "bind" && s.state.Owner != nil && s.state.Owner.AllowTargetStop != owner.AllowTargetStop {
		reject = "campaign_policy_conflict"
	}
	if reject == "" {
		if err := h.Check(r, body, s.state); err != nil {
			reject = err.Error()
		}
	}
	if reject != "" {
		s.mu.Unlock()
		return s.finish(r.OperationID, OperationError(409, reject))
	}
	if r.Operation == "session.owner" {
		if closing {
			s.closeLocked("controller_closed_execution")
		} else if s.state.Owner == nil {
			s.state.Owner = &ExecutionOwner{Principal: principal, CampaignID: r.CampaignID, WorkerInstanceID: r.WorkerInstanceID, RunRevision: r.RunRevision, AllowTargetStop: owner.AllowTargetStop, BoundAt: time.Now().UTC()}
		}
		err := s.saveLocked()
		if err == nil {
			err = h.OwnerChanged(*s.state.Owner)
		}
		result := s.ownerResponse(h.Revision())
		if err != nil {
			s.poisoned = err
			s.closeLocked("execution_persistence_failed")
			result = OperationError(503, "ownership_outcome_unknown")
		}
		s.mu.Unlock()
		return s.finish(r.OperationID, result)
	}
	if r.Operation == "session.stop" {
		s.closeLocked("target_stop_requested")
		if err := s.saveLocked(); err != nil {
			s.mu.Unlock()
			return s.finish(r.OperationID, OperationError(503, "execution_store_unavailable"))
		}
	}
	// Persisted running is the durable admission point. Closure after this point
	// cannot retract a dispatched effect; its eventual outcome is retained.
	s.mu.Unlock()
	result := h.Dispatch(ctx, r, body)
	return s.finish(r.OperationID, result)
}
func (s *ExecutionStore) ownerResponse(revision uint64) GatewayResponse {
	raw, _ := json.Marshal(s.state.Owner)
	return GatewayResponse{Status: 200, Body: raw, SessionRevision: revision}
}
func (s *ExecutionStore) finish(id string, result GatewayResponse) GatewayResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.state.Records[id]
	rec.State = "completed"
	if len(result.Body) > MaxOperationResponse {
		result = OperationError(409, "response_capacity_outcome_unknown")
		rec.State = "unknown"
		s.closeLocked("operation_outcome_unknown")
	}
	if result.Status >= 500 {
		rec.State = "unknown"
		s.closeLocked("operation_outcome_unknown")
	}
	now := time.Now().UTC()
	rec.FinishedAt = &now
	rec.Response = result
	if err := s.saveLocked(); err != nil {
		return OperationError(503, "operation_outcome_unknown")
	}
	return result
}
func (s *ExecutionStore) Lookup(id string) (OperationRecord, bool) {
	state := s.State()
	rec, ok := state.Records[id]
	if !ok {
		return OperationRecord{}, false
	}
	return *rec, true
}

// VerifyExecutionJournal binds the retained state to the native hash-chained
// journal. A torn state/journal commit fails closed; it never restores admission.
func VerifyExecutionJournal(root string, transactions []statejournal.Transaction) ([]byte, error) {
	state, err := ReadExecutionState(root)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(root, "execution.json"))
	if err != nil {
		return nil, err
	}
	head := ""
	for _, tx := range transactions {
		for _, op := range tx.Operations {
			if op.Kind != "idempotency_commit" {
				continue
			}
			var value struct {
				ExecutionStateDigest string `json:"execution_state_digest"`
				SessionID            string `json:"session_id"`
			}
			if json.Unmarshal(op.Value, &value) == nil && value.SessionID == state.SessionID && value.ExecutionStateDigest != "" {
				head = value.ExecutionStateDigest
			}
		}
	}
	if head == "" && state.Owner == nil && state.ClosedAt == nil && len(state.Records) == 0 {
		return raw, nil
	}
	if head != digest.Bytes(raw) {
		return nil, errors.New("execution state does not match native journal")
	}
	return raw, nil
}

func OperationFingerprint(r OperationRequest) string {
	raw, _ := json.Marshal(r)
	return digest.Bytes(raw)
}

// CommandFingerprint separates caller attribution from duplicate-effect identity.
// OperationFingerprint still binds the complete original request for audit.
func CommandFingerprint(r OperationRequest, body []byte) string {
	r.WorkerInstanceID = ""
	r.RunRevision = 0
	if r.Operation == "session.owner" && body != nil {
		var association OwnerRequest
		if StrictJSON(body, &association) == nil {
			association.WorkerInstanceID = "" // deprecated duplicate of envelope attribution
			canonical, _ := json.Marshal(association)
			r.BodyDigest = digest.Bytes(canonical)
		}
	}
	return OperationFingerprint(r)
}

// OperationFrameLimit leaves room for a bounded saved response and its envelope.
func OperationFrameLimit(maximumRawBytes int64) int64 {
	limit := ArtifactEnvelopeLimit(maximumRawBytes)
	if limit < MaxOperationResponse+(1<<20) {
		limit = MaxOperationResponse + (1 << 20)
	}
	return limit
}
