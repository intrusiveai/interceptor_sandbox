package sandboxd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

type operationLookup struct {
	ReceiptID   string `json:"receipt_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}
type injectionLookup struct {
	InjectionID string `json:"injection_id"`
}
type injectionArm struct {
	Definition   injection.Definition `json:"definition"`
	EligibleTurn string               `json:"eligible_turn,omitempty"`
}
type snapshotOperationRequest struct {
	Label                 string                 `json:"label,omitempty"`
	Description           string                 `json:"description,omitempty"`
	MaximumCommittedBytes snapshot.ByteAllowance `json:"maximum_committed_bytes,omitzero"`
}

type artifactRegistration struct {
	Descriptor adaptive.ArtifactDescriptor `json:"descriptor"`
	Content    []byte                      `json:"content"`
}

func (s *Server) operationsHandler(w http.ResponseWriter, r *http.Request, mux http.Handler) {
	if s.execution == nil {
		writeJSON(w, 200, adaptive.OperationError(409, "session_contract_unsupported"))
		return
	}
	var input struct {
		Principal string                     `json:"principal"`
		Envelope  adaptive.OperationEnvelope `json:"envelope"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, adaptive.OperationFrameLimit(s.metadata.Limits.MaxBlobBytes)))
	if err != nil || adaptive.StrictJSON(raw, &input) != nil || input.Principal == "" {
		writeJSON(w, 200, adaptive.OperationError(400, "invalid_envelope"))
		return
	}
	hooks := adaptive.ExecutionHooks{Check: s.checkOperation, Revision: s.operationRevision, OwnerChanged: s.ownerChanged, Dispatch: func(ctx context.Context, request adaptive.OperationRequest, body json.RawMessage) adaptive.GatewayResponse {
		return s.dispatchOperation(ctx, request, body, mux)
	}}
	result := s.execution.Execute(input.Principal, input.Envelope.Request, input.Envelope.Body, hooks)
	// The private transport carries the typed response including its HTTP status.
	writeJSON(w, 200, result)
}
func (s *Server) operationRevision() uint64 {
	s.metadataMu.RLock()
	defer s.metadataMu.RUnlock()
	return s.metadata.Revision
}
func (s *Server) ownerChanged(owner adaptive.ExecutionOwner) error {
	s.metadataMu.Lock()
	defer s.metadataMu.Unlock()
	s.metadata.Revision++
	s.metadata.CampaignID = owner.CampaignID
	if err := session.Save(s.paths.Metadata, s.metadata); err != nil {
		return err
	}
	_, err := s.recorder.Append(recording.Draft{Channel: recording.Control, Layer: "operations", Kind: "execution_owner_changed", Provenance: recording.SandboxControl, Semantic: map[string]any{"campaign_id": owner.CampaignID, "worker_instance_id": owner.WorkerInstanceID, "run_revision": owner.RunRevision, "allow_target_stop": owner.AllowTargetStop, "closed_at": owner.ClosedAt, "closure_reason": owner.ClosureReason, "session_revision": s.metadata.Revision}})
	if err != nil {
		return err
	}
	return s.recorder.Flush()
}
func (s *Server) checkOperation(r adaptive.OperationRequest, body json.RawMessage, state adaptive.ExecutionState) error {
	s.metadataMu.RLock()
	phase := s.metadata.Phase
	s.metadataMu.RUnlock()
	if phase != session.Running && phase != session.Quiescing && r.Operation != "session.status" && r.Operation != "operation.status" && r.Operation != "session.owner" {
		return errors.New("session_not_running")
	}
	switch r.Operation {
	case "session.owner":
		var owner adaptive.OwnerRequest
		if err := adaptive.StrictJSON(body, &owner); err != nil {
			return err
		}
		if owner.Action == "bind" {
			if phase != session.Running {
				return errors.New("session_not_running")
			}
			for _, a := range s.attempts.List() {
				if a.CampaignID != r.CampaignID {
					return errors.New("restored_campaign_mismatch")
				}
			}
		}
	case "attempt.register":
		var a adaptive.AttemptContext
		if adaptive.StrictJSON(body, &a) != nil || a.CampaignID != r.CampaignID || a.AttemptID != r.AttemptID || adaptive.DigestAttemptContext(a) != r.AttemptContextDigest {
			return errors.New("attempt_binding_mismatch")
		}
	case "injection.arm":
		var a injectionArm
		if adaptive.StrictJSON(body, &a) != nil || a.Definition.AttemptID != r.AttemptID || !safeOperationID(a.Definition.ID) {
			return errors.New("invalid_injection")
		}
		for _, existing := range s.injections.ListPublic() {
			if existing.Definition.ID == a.Definition.ID {
				return errors.New("injection_id_already_exists")
			}
		}
	case "injection.delete":
		var lookup injectionLookup
		if adaptive.StrictJSON(body, &lookup) != nil || !safeOperationID(lookup.InjectionID) {
			return errors.New("invalid_injection_id")
		}
		// CampaignScope has already checked access to this session. Any injection
		// present here (including replayed checkpoint state) can be deleted by that
		// campaign; creation by this worker/revision is not required.
	case "application.invoke":
		var turn TurnRequest
		if adaptive.StrictJSON(body, &turn) != nil || turn.AttemptID != r.AttemptID {
			return errors.New("invalid_application_invocation")
		}
	case "artifact.register":
		var artifact artifactRegistration
		if err := adaptive.StrictJSON(body, &artifact); err != nil {
			return errors.New("invalid_artifact")
		}
	case "artifact.read":
		var lookup struct {
			Digest string `json:"digest"`
		}
		if adaptive.StrictJSON(body, &lookup) != nil || !digest.Valid(lookup.Digest) {
			return errors.New("invalid_artifact_digest")
		}
	case "operation.status":
		var lookup operationLookup
		if adaptive.StrictJSON(body, &lookup) != nil || (lookup.ReceiptID == "") == (lookup.OperationID == "") || lookup.ReceiptID != "" && !safeOperationID(lookup.ReceiptID) || lookup.OperationID != "" && !safeOperationID(lookup.OperationID) {
			return errors.New("invalid_operation_lookup")
		}
	case "snapshot.create":
		var input snapshotOperationRequest
		if adaptive.StrictJSON(body, &input) != nil || snapshot.ValidateMetadata(r.CampaignID, input.Description) != nil {
			return errors.New("invalid_snapshot_request")
		}
	case "observation.read":
		var input adaptive.ObservationRequest
		if adaptive.StrictJSON(body, &input) != nil || input.Validate() != nil {
			return errors.New("invalid_observation_request")
		}
	case "observation.content.read":
		var input adaptive.FeedbackReadRequest
		if adaptive.StrictJSON(body, &input) != nil || input.Validate() != nil {
			return errors.New("invalid_observation_read")
		}
	case "session.status", "capabilities.read", "session.stop":
		if adaptive.StrictJSON(body, &struct{}{}) != nil {
			return errors.New("body_must_be_empty")
		}
	}
	if r.Operation == "injection.arm" || r.Operation == "application.invoke" || r.Operation == "observation.read" || r.Operation == "observation.content.read" {
		a, ok := s.attempts.Get(r.AttemptID)
		if !ok || a.CampaignID != r.CampaignID || adaptive.DigestAttemptContext(a) != r.AttemptContextDigest {
			return errors.New("attempt_binding_mismatch")
		}
	}
	return nil
}
func safeOperationID(id string) bool {
	return len(id) > 0 && len(id) <= 256 && !strings.ContainsAny(id, "/\\\x00") && id != "." && id != ".."
}
func (s *Server) dispatchOperation(ctx context.Context, r adaptive.OperationRequest, body json.RawMessage, mux http.Handler) adaptive.GatewayResponse {
	method, path := http.MethodGet, "/v1/status"
	switch r.Operation {
	case "session.status":
		s.metadataMu.RLock()
		metadata := s.metadata
		s.metadataMu.RUnlock()
		state := s.execution.State()
		raw, _ := json.Marshal(map[string]any{"session": metadata, "owner": state.Owner, "execution_available": state.Owner != nil && state.Owner.ClosedAt == nil && metadata.Phase == session.Running})
		return adaptive.GatewayResponse{Status: 200, Body: raw, SessionRevision: metadata.Revision}
	case "operation.status":
		var lookup operationLookup
		_ = json.Unmarshal(body, &lookup)
		if lookup.OperationID != "" {
			rec, ok := s.execution.Lookup(lookup.OperationID)
			if !ok {
				return adaptive.OperationError(404, "operation_not_recorded_outcome_unknown")
			}
			raw, _ := json.Marshal(rec)
			return adaptive.GatewayResponse{Status: 200, Body: raw, SessionRevision: s.operationRevision()}
		}
		path = "/v1/turns/" + url.PathEscape(lookup.ReceiptID)
		body = nil
	case "capabilities.read":
		path = "/v1/capabilities"
	case "artifact.register":
		method, path = http.MethodPost, "/v1/artifacts"
	case "artifact.read":
		var lookup struct {
			Digest string `json:"digest"`
		}
		_ = json.Unmarshal(body, &lookup)
		path = "/v1/artifacts/" + url.PathEscape(lookup.Digest)
		body = nil
	case "attempt.register":
		method, path = http.MethodPost, "/v1/attempts"
	case "injection.arm":
		method, path = http.MethodPost, "/v1/injections"
	case "injection.delete":
		var lookup injectionLookup
		_ = json.Unmarshal(body, &lookup)
		method, path = http.MethodDelete, "/v1/injections/"+url.PathEscape(lookup.InjectionID)
		body = nil
	case "application.invoke":
		method, path = http.MethodPost, "/v1/turns"
	case "observation.read":
		var input adaptive.ObservationRequest
		_ = json.Unmarshal(body, &input)
		return s.feedbackView(ctx, r, input)
	case "observation.content.read":
		var input adaptive.FeedbackReadRequest
		_ = json.Unmarshal(body, &input)
		return adaptive.ReadFeedback(s.paths.Root, s.blobs, r, input)
	case "snapshot.create":
		var input snapshotOperationRequest
		_ = json.Unmarshal(body, &input)
		// Keep the caller's exact allowance; current native limits are checked
		// inside createSnapshot, under the snapshot/control lock.
		body, _ = json.Marshal(struct {
			snapshotOperationRequest
			CampaignID string `json:"campaign_id"`
		}{input, r.CampaignID})
		method, path = http.MethodPost, "/v1/snapshots"
	case "session.stop":
		method, path = http.MethodPost, "/v1/stop"
	default:
		return adaptive.OperationError(400, "unsupported_operation")
	}
	if method != http.MethodGet {
		if _, err := s.recorder.Append(recording.Draft{Channel: recording.Control, Layer: "operations", Kind: "operation_dispatched", Provenance: recording.SandboxControl, Semantic: map[string]any{"request_id": r.RequestID, "operation_id": r.OperationID, "operation": r.Operation, "campaign_id": r.CampaignID, "worker_instance_id": r.WorkerInstanceID, "run_revision": r.RunRevision, "body_digest": r.BodyDigest}}); err != nil {
			return adaptive.OperationError(503, "recording_failed")
		}
		if err := s.recorder.Flush(); err != nil {
			return adaptive.OperationError(503, "recording_failed")
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return adaptive.OperationError(400, "invalid_operation")
	}
	request.Header.Set("Content-Type", "application/json")
	limit := int64(adaptive.MaxOperationResponse)
	if r.Operation == "artifact.read" {
		limit = s.metadata.Limits.MaxBlobBytes
	}
	response := &operationResponseWriter{header: make(http.Header), limit: limit}
	mux.ServeHTTP(response, request)
	if response.overflow {
		return adaptive.OperationError(503, "response_capacity_outcome_unknown")
	}
	status := response.status
	if status == 0 {
		status = 200
	}
	raw := response.body.Bytes()
	if r.Operation == "artifact.read" && status < 300 {
		raw, _ = json.Marshal(map[string]any{"content": raw, "digest": strings.TrimPrefix(path, "/v1/artifacts/")})
	}
	if len(raw) == 0 {
		raw = []byte("null")
	}
	result := adaptive.GatewayResponse{Status: status, Body: raw, SessionRevision: s.operationRevision()}
	if method != http.MethodGet && ctx.Err() != nil {
		raw, _ := json.Marshal(map[string]any{"code": "deadline_outcome_unknown", "native_response": result.Body})
		result.Status = 503
		result.Body = raw
	}
	if method != http.MethodGet {
		s.metadataMu.RLock()
		err := session.Save(s.paths.Metadata, s.metadata)
		s.metadataMu.RUnlock()
		if err != nil {
			return adaptive.OperationError(503, "revision_persistence_failed")
		}
	}
	if method != http.MethodGet {
		if _, err := s.recorder.Append(recording.Draft{Channel: recording.Control, Layer: "operations", Kind: "operation_returned", Provenance: recording.SandboxControl, Semantic: map[string]any{"operation_id": r.OperationID, "status": status, "session_revision": result.SessionRevision}}); err != nil {
			return adaptive.OperationError(503, "recording_failed")
		}
		if err := s.recorder.Flush(); err != nil {
			return adaptive.OperationError(503, "recording_failed")
		}
	}
	return result
}

type operationResponseWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	limit    int64
	overflow bool
}

func (w *operationResponseWriter) Header() http.Header { return w.header }
func (w *operationResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *operationResponseWriter) Write(raw []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if int64(w.body.Len()+len(raw)) > w.limit {
		w.overflow = true
		return 0, errors.New("response limit")
	}
	return w.body.Write(raw)
}

func operationControlClass(frame bridge.HTTPRequestFrame) string {
	if frame.Method != http.MethodPost || frame.Path != "/v1/operations" {
		return ""
	}
	var value struct {
		Envelope struct {
			Request struct {
				Operation string `json:"operation"`
			} `json:"request"`
			Body struct {
				Action string `json:"action"`
			} `json:"body"`
		} `json:"envelope"`
	}
	if json.Unmarshal(frame.Body, &value) == nil {
		op := value.Envelope.Request.Operation
		if op == "session.status" || op == "operation.status" || op == "session.owner" && value.Envelope.Body.Action == "close_execution" {
			return "priority"
		}
	}
	return "operation"
}
