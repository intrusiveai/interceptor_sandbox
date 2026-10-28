package gatewayhost

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/hostrun"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
	"path/filepath"
)

type Executor struct{}

func NewExecutor() *Executor { return &Executor{} }
func (e *Executor) Execute(request *http.Request, principal string, input adaptive.OperationRequest, body json.RawMessage) (adaptive.GatewayResponse, error) {
	root, err := session.DataRoot()
	if err != nil {
		return adaptive.GatewayResponse{}, err
	}
	metadata, paths, err := session.Load(root, input.SessionID)
	if err != nil {
		return adaptive.GatewayResponse{}, err
	}
	if metadata.OperationAPIVersion != adaptive.OperationRequestVersion {
		return adaptive.OperationError(409, "session_contract_unsupported"), nil
	}
	if metadata.Phase != session.Running && metadata.Phase != session.Quiescing {
		return retainedOperation(metadata, paths, principal, input, body)
	}
	// Principal is supplied by the trusted host service, never the public envelope.
	principalJSON, _ := json.Marshal(principal)
	raw := append([]byte(`{"principal":`), principalJSON...)
	raw = append(raw, []byte(`,"envelope":`)...)
	raw = append(raw, adaptive.EncodeOperation(input, body)...)
	raw = append(raw, '}')
	response, err := hostrun.CallSession(request.Context(), input.SessionID, http.MethodPost, "/v1/operations", raw)
	if err != nil {
		if input.Operation == "session.status" || input.Operation == "operation.status" {
			return retainedOperation(metadata, paths, principal, input, body)
		}
		return adaptive.GatewayResponse{}, err
	}
	var result adaptive.GatewayResponse
	if err = adaptive.StrictJSON(response, &result); err != nil {
		return result, err
	}
	if result.Status < 200 || result.Status > 599 {
		return result, errors.New("invalid session operation response")
	}
	return result, nil
}
func retainedOperation(metadata session.Metadata, paths session.Paths, principal string, r adaptive.OperationRequest, body []byte) (adaptive.GatewayResponse, error) {
	if !time.Now().Before(r.Deadline) {
		return adaptive.OperationError(408, "deadline_exceeded"), nil
	}
	transactions, err := statejournal.VerifyFile(filepath.Join(paths.Root, "state.jsonl"))
	if err != nil {
		return adaptive.GatewayResponse{}, err
	}
	if _, err := adaptive.VerifyExecutionJournal(paths.Root, transactions); err != nil {
		return adaptive.GatewayResponse{}, err
	}
	state, err := adaptive.ReadExecutionState(paths.Root)
	if err != nil {
		return adaptive.GatewayResponse{}, err
	}
	if !adaptive.CampaignScope(state, r) {
		return adaptive.OperationError(403, "campaign_mismatch"), nil
	}
	switch r.Operation {
	case "observation.read":
		var input adaptive.ObservationRequest
		if adaptive.StrictJSON(body, &input) != nil || input.Validate() != nil {
			return adaptive.OperationError(400, "invalid_observation_request"), nil
		}
		if input.ReceiptID == "" {
			for _, taint := range metadata.Taints {
				if taint == "evidence_incomplete" {
					return adaptive.OperationError(503, "evidence_storage_error"), nil
				}
			}
			return adaptive.OperationError(409, "retained_feedback_requires_receipt"), nil
		}
		return adaptive.RetainedFeedback(paths.Root, r, input.ReceiptID), nil
	case "observation.content.read":
		var input adaptive.FeedbackReadRequest
		if adaptive.StrictJSON(body, &input) != nil || input.Validate() != nil {
			return adaptive.OperationError(400, "invalid_observation_read"), nil
		}
		blobs, err := blobstore.Open(paths.Blobs, metadata.Limits.MaxBlobBytes)
		if err != nil {
			return adaptive.GatewayResponse{}, err
		}
		return adaptive.ReadFeedback(paths.Root, blobs, r, input), nil
	case "session.owner":
		var owner adaptive.OwnerRequest
		if adaptive.StrictJSON(body, &owner) != nil || owner.Action != "close_execution" || owner.SessionID != r.SessionID || owner.CampaignID != r.CampaignID || owner.AllowTargetStop {
			return adaptive.OperationError(400, "invalid_owner_request"), nil
		}
		if state.Owner.ClosedAt == nil {
			return adaptive.OperationError(409, "closure_unconfirmed"), nil
		}
		content, _ := json.Marshal(state.Owner)
		return adaptive.GatewayResponse{Status: 200, Body: content, SessionRevision: metadata.Revision}, nil
	case "session.status":
		if adaptive.StrictJSON(body, &struct{}{}) != nil {
			return adaptive.OperationError(400, "invalid_body"), nil
		}
		content, _ := json.Marshal(map[string]any{"session": metadata, "owner": state.Owner, "execution_available": false})
		return adaptive.GatewayResponse{Status: 200, Body: content, SessionRevision: metadata.Revision}, nil
	case "operation.status":
		var lookup struct {
			OperationID string `json:"operation_id"`
			ReceiptID   string `json:"receipt_id"`
		}
		if adaptive.StrictJSON(body, &lookup) != nil || (lookup.OperationID == "") == (lookup.ReceiptID == "") {
			return adaptive.OperationError(400, "invalid_lookup"), nil
		}
		if lookup.ReceiptID != "" {
			return adaptive.OperationError(409, "receipt_service_unavailable"), nil
		}
		record, ok := state.Records[lookup.OperationID]
		if !ok {
			return adaptive.OperationError(404, "operation_not_recorded_outcome_unknown"), nil
		}
		if record.State == "running" {
			record.State = "unknown"
			record.Response = adaptive.OperationError(409, "outcome_unknown")
		}
		content, _ := json.Marshal(record)
		return adaptive.GatewayResponse{Status: 200, Body: content, SessionRevision: metadata.Revision}, nil
	default:
		return adaptive.OperationError(409, "session_not_running"), nil
	}
}
