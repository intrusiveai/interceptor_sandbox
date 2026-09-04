package gatewayhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/hostrun"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

type Executor struct {
	mu            sync.Mutex
	snapshotBytes map[string]int64
}

func NewExecutor() *Executor { return &Executor{snapshotBytes: map[string]int64{}} }
func (e *Executor) Execute(request *http.Request, lease adaptive.ExecutionLease, input adaptive.CrossVMRequest, body json.RawMessage) (adaptive.GatewayResponse, error) {
	if err := validateTypedBody(lease, input, body); err != nil {
		return adaptive.GatewayResponse{}, adaptive.RejectRequest(err)
	}
	artifactReadDigest := ""
	mutation := input.Operation == "artifact.register" || input.Operation == "attempt.register" || input.Operation == "injection.arm" || input.Operation == "injection.delete" || input.Operation == "application.invoke" || input.Operation == "snapshot.create" || input.Operation == "session.stop"
	if mutation {
		e.mu.Lock()
		defer e.mu.Unlock()
		if input.ExpectedSessionRevision == 0 {
			return adaptive.GatewayResponse{}, adaptive.RejectRequest(errors.New("mutation requires expected session revision"))
		}
		metadata, err := loadMetadata(lease.SessionID)
		if err != nil {
			return adaptive.GatewayResponse{}, err
		}
		if metadata.Revision != input.ExpectedSessionRevision {
			return adaptive.GatewayResponse{}, adaptive.RejectRequest(errors.New("stale session revision"))
		}
	}
	method, path := http.MethodGet, "/v1/status"
	switch input.Operation {
	case "session.status":
	case "capabilities.read":
		path = "/v1/capabilities"
	case "artifact.register":
		var value struct {
			Descriptor adaptive.ArtifactDescriptor `json:"descriptor"`
			Content    []byte                      `json:"content"`
		}
		if err := strictDecode(body, &value); err != nil {
			return adaptive.GatewayResponse{}, errors.New("artifact registration body is invalid")
		}
		method, path = http.MethodPost, "/v1/artifacts"
	case "artifact.read":
		var lookup struct {
			Digest string `json:"digest"`
		}
		if json.Unmarshal(body, &lookup) != nil || lookup.Digest == "" {
			return adaptive.GatewayResponse{}, errors.New("artifact read requires digest")
		}
		artifactReadDigest = lookup.Digest
		path = "/v1/artifacts/" + url.PathEscape(lookup.Digest)
		body = nil
	case "attempt.register":
		var value adaptive.AttemptContext
		if err := strictDecode(body, &value); err != nil || input.AttemptID == "" || value.AttemptID != input.AttemptID || value.CampaignID != lease.CampaignID || input.AttemptContextDigest == "" || adaptive.DigestAttemptContext(value) != input.AttemptContextDigest {
			return adaptive.GatewayResponse{}, errors.New("attempt registration is not bound to the lease and request")
		}
		method, path = http.MethodPost, "/v1/attempts"
	case "injection.arm":
		var value struct {
			Definition   injection.Definition `json:"definition"`
			EligibleTurn string               `json:"eligible_turn,omitempty"`
		}
		if err := strictDecode(body, &value); err != nil || input.AttemptID == "" || value.Definition.AttemptID != input.AttemptID {
			return adaptive.GatewayResponse{}, errors.New("injection is not bound to the request attempt")
		}
		method, path = http.MethodPost, "/v1/injections"
	case "injection.delete":
		var value struct {
			InjectionID string `json:"injection_id"`
		}
		if err := strictDecode(body, &value); err != nil || value.InjectionID == "" || strings.ContainsAny(value.InjectionID, "/\\") {
			return adaptive.GatewayResponse{}, errors.New("injection deletion requires a valid injection ID")
		}
		method, path = http.MethodDelete, "/v1/injections/"+url.PathEscape(value.InjectionID)
		body = nil
	case "application.invoke":
		var value struct {
			AttemptID string `json:"attempt_id"`
		}
		if err := json.Unmarshal(body, &value); err != nil || input.AttemptID == "" || value.AttemptID != input.AttemptID {
			return adaptive.GatewayResponse{}, errors.New("application invocation is not bound to the request attempt")
		}
		method, path = http.MethodPost, "/v1/turns"
	case "observation.read":
		if input.AttemptID == "" {
			return adaptive.GatewayResponse{}, errors.New("observation requires attempt ID")
		}
		path = "/v1/observations/" + url.PathEscape(input.AttemptID)
	case "snapshot.create":
		method, path = http.MethodPost, "/v1/snapshots"
		if lease.Limits.MaximumCumulativeSnapshotBytes > 0 {
			remaining := lease.Limits.MaximumCumulativeSnapshotBytes - e.snapshotBytes[lease.LeaseID]
			if remaining <= 0 {
				return adaptive.GatewayResponse{}, adaptive.RejectRequest(errors.New("cumulative snapshot-byte limit is exhausted"))
			}
			var value struct {
				Label string `json:"label,omitempty"`
			}
			if len(body) > 0 && json.Unmarshal(body, &value) != nil {
				return adaptive.GatewayResponse{}, errors.New("snapshot body is invalid")
			}
			body, _ = json.Marshal(map[string]any{"label": value.Label, "maximum_committed_bytes": remaining})
		}
	case "session.stop":
		method, path = http.MethodPost, "/v1/stop"
	case "operation.status":
		var lookup struct {
			ReceiptID string `json:"receipt_id"`
		}
		if json.Unmarshal(body, &lookup) != nil || lookup.ReceiptID == "" {
			return adaptive.GatewayResponse{}, errors.New("operation status requires receipt_id")
		}
		path = "/v1/turns/" + url.PathEscape(lookup.ReceiptID)
		body = nil
	default:
		return adaptive.GatewayResponse{}, errors.New("unsupported gateway operation")
	}
	response, err := hostrun.CallSession(context.WithoutCancel(request.Context()), lease.SessionID, method, path, body)
	if err != nil {
		return adaptive.GatewayResponse{}, fmt.Errorf("execute typed session operation: %w", err)
	}
	if artifactReadDigest != "" {
		response, _ = json.Marshal(map[string]any{"digest": artifactReadDigest, "content": response})
	}
	metadata, _, err := loadSession(lease.SessionID)
	if err != nil {
		return adaptive.GatewayResponse{}, err
	}
	result := adaptive.GatewayResponse{Status: http.StatusOK, Body: json.RawMessage(response), SessionRevision: metadata.Revision}
	if input.Operation == "snapshot.create" {
		var checkpoint snapshot.Checkpoint
		if err := json.Unmarshal(response, &checkpoint); err != nil {
			return adaptive.GatewayResponse{}, fmt.Errorf("decode committed checkpoint: %w", err)
		}
		if err := snapshot.VerifyCheckpoint(checkpoint); err != nil {
			return adaptive.GatewayResponse{}, fmt.Errorf("verify committed checkpoint: %w", err)
		}
		result.CommittedBytes = checkpoint.CanonicalSizeBytes
		e.snapshotBytes[lease.LeaseID] += result.CommittedBytes
	}
	return result, nil
}
func loadMetadata(sessionID string) (session.Metadata, error) {
	metadata, _, err := loadSession(sessionID)
	return metadata, err
}
func loadSession(sessionID string) (session.Metadata, session.Paths, error) {
	root, err := session.DataRoot()
	if err != nil {
		return session.Metadata{}, session.Paths{}, err
	}
	return session.Load(root, sessionID)
}

func strictDecode(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("operation body must contain exactly one JSON value")
	}
	return nil
}

func validateTypedBody(lease adaptive.ExecutionLease, input adaptive.CrossVMRequest, body []byte) error {
	switch input.Operation {
	case "attempt.register":
		var value adaptive.AttemptContext
		if err := strictDecode(body, &value); err != nil || input.AttemptID == "" || value.AttemptID != input.AttemptID || value.CampaignID != lease.CampaignID || input.AttemptContextDigest == "" || adaptive.DigestAttemptContext(value) != input.AttemptContextDigest {
			return errors.New("attempt registration is not bound to the lease and request")
		}
	case "injection.arm":
		var value struct {
			Definition   injection.Definition `json:"definition"`
			EligibleTurn string               `json:"eligible_turn,omitempty"`
		}
		if err := strictDecode(body, &value); err != nil || input.AttemptID == "" || value.Definition.AttemptID != input.AttemptID {
			return errors.New("injection is not bound to the request attempt")
		}
	case "injection.delete":
		var value struct {
			InjectionID string `json:"injection_id"`
		}
		if err := strictDecode(body, &value); err != nil || value.InjectionID == "" || strings.ContainsAny(value.InjectionID, "/\\") {
			return errors.New("injection deletion requires a valid injection ID")
		}
	case "application.invoke":
		var value map[string]json.RawMessage
		if err := json.Unmarshal(body, &value); err != nil || input.AttemptID == "" {
			return errors.New("application invocation is not bound to the request attempt")
		}
		var attemptID string
		if json.Unmarshal(value["attempt_id"], &attemptID) != nil || attemptID != input.AttemptID {
			return errors.New("application invocation is not bound to the request attempt")
		}
	case "artifact.register":
		var value struct {
			Descriptor adaptive.ArtifactDescriptor `json:"descriptor"`
			Content    []byte                      `json:"content"`
		}
		if err := strictDecode(body, &value); err != nil {
			return errors.New("artifact registration body is invalid")
		}
	case "artifact.read":
		var value struct {
			Digest string `json:"digest"`
		}
		if err := strictDecode(body, &value); err != nil || !digest.Valid(value.Digest) {
			return errors.New("artifact read requires a valid digest")
		}
	case "operation.status":
		var value struct {
			ReceiptID string `json:"receipt_id"`
		}
		if err := strictDecode(body, &value); err != nil || value.ReceiptID == "" {
			return errors.New("operation status requires a receipt ID")
		}
	case "snapshot.create":
		var value struct {
			Label string `json:"label,omitempty"`
		}
		if err := strictDecode(body, &value); err != nil {
			return errors.New("snapshot request body is invalid")
		}
	case "session.status", "capabilities.read", "observation.read", "session.stop":
		var value struct{}
		if err := strictDecode(body, &value); err != nil {
			return errors.New("operation body must be empty")
		}
	}
	return nil
}
