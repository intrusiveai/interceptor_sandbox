package adaptive

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
)

var allowedGatewayOperations = map[string]bool{"session.owner": true, "session.status": true, "operation.status": true, "capabilities.read": true, "artifact.register": true, "artifact.read": true, "attempt.register": true, "injection.arm": true, "injection.delete": true, "application.invoke": true, "observation.read": true, "observation.content.read": true, "snapshot.create": true, "session.stop": true}

type GatewayResponse struct {
	Status          int             `json:"status"`
	Body            json.RawMessage `json:"body,omitempty"`
	SessionRevision uint64          `json:"session_revision"`
	CommittedBytes  int64           `json:"-"`
}
type GatewayExecutor interface {
	Execute(*http.Request, string, OperationRequest, json.RawMessage) (GatewayResponse, error)
}
type GatewayConfig struct {
	SessionID, EnvironmentDigest, ApplicationDigest, CapabilityManifestDigest, FeedbackProfile string
	IntegrationPrincipal                                                                       string
	IntegrationToken                                                                           []byte
	Executor                                                                                   GatewayExecutor
	MaxFrameBytes                                                                              int64
}

// Gateway authenticates and frames requests. It owns no execution authority or ledger.
type Gateway struct {
	config      GatewayConfig
	tokenDigest [sha256.Size]byte
}

func NewGateway(c GatewayConfig) (*Gateway, error) {
	if !boundedID(c.SessionID) || !boundedID(c.IntegrationPrincipal) || c.Executor == nil || !digest.Valid(c.EnvironmentDigest) || !digest.Valid(c.ApplicationDigest) || !digest.Valid(c.CapabilityManifestDigest) || c.FeedbackProfile == "" {
		return nil, errors.New("gateway requires session bindings, principal and executor")
	}
	if err := validateSecretToken(c.IntegrationToken); err != nil {
		return nil, err
	}
	if c.MaxFrameBytes == 0 {
		c.MaxFrameBytes = 16 << 20
	}
	if c.MaxFrameBytes < 1 || c.MaxFrameBytes > 512<<20 {
		return nil, errors.New("invalid frame limit")
	}
	hash := sha256.Sum256(c.IntegrationToken)
	c.IntegrationToken = nil
	return &Gateway{config: c, tokenDigest: hash}, nil
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path != "/v1/operations" {
		gatewayError(w, 404, "not_found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		gatewayError(w, 405, "method_not_allowed")
		return
	}
	auth := r.Header.Values("Authorization")
	if len(auth) != 1 || !strings.HasPrefix(auth[0], "Bearer ") {
		bearerError(w, 401, "invalid_credential")
		return
	}
	token := strings.TrimPrefix(auth[0], "Bearer ")
	hash := sha256.Sum256([]byte(token))
	if validateSecretToken([]byte(token)) != nil || subtle.ConstantTimeCompare(hash[:], g.tokenDigest[:]) != 1 {
		bearerError(w, 401, "invalid_credential")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.config.MaxFrameBytes))
	if err != nil {
		gatewayError(w, 413, "frame_too_large")
		return
	}
	var envelope OperationEnvelope
	if err = StrictJSON(raw, &envelope); err != nil {
		gatewayError(w, 400, "invalid_envelope")
		return
	}
	if err = ValidateOperation(envelope.Request, envelope.Body, g.config.SessionID); err != nil {
		gatewayError(w, 400, "invalid_request")
		return
	}
	result, err := g.config.Executor.Execute(r, g.config.IntegrationPrincipal, envelope.Request, envelope.Body)
	if err != nil {
		gatewayError(w, 503, "session_unavailable_outcome_unknown")
		return
	}
	gatewayWrite(w, result)
}
func ValidateOperation(r OperationRequest, body []byte, sessionID string) error {
	if r.APIVersion != OperationRequestVersion || !boundedID(r.RequestID) || !boundedID(r.OperationID) || !boundedID(r.CampaignID) || !boundedID(r.WorkerInstanceID) || r.SessionID != sessionID || r.RunRevision == 0 || !allowedGatewayOperations[r.Operation] || r.Deadline.IsZero() || !digest.Valid(r.BodyDigest) || digest.Bytes(body) != r.BodyDigest {
		return errors.New("invalid operation envelope or body digest")
	}
	if r.AttemptID != "" && !boundedID(r.AttemptID) || r.AttemptContextDigest != "" && !digest.Valid(r.AttemptContextDigest) {
		return errors.New("invalid attempt binding")
	}
	return nil
}

// EncodeOperation preserves the exact JSON body bytes covered by body_digest.
func EncodeOperation(r OperationRequest, body []byte) []byte {
	header, _ := json.Marshal(r)
	result := append([]byte(`{"request":`), header...)
	result = append(result, []byte(`,"body":`)...)
	result = append(result, body...)
	return append(result, '}')
}

// StrictJSON also rejects duplicate keys (including nested objects), trailing values
// and excessive nesting before decoding into the closed Go schema.
func StrictJSON(raw []byte, target any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return errors.New("JSON object required")
	}
	if !utf8.Valid(raw) {
		return errors.New("invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 64 {
			return errors.New("JSON nesting limit")
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := k.(string)
				if !ok || keys[key] {
					return errors.New("duplicate JSON key")
				}
				keys[key] = true
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}
func validateSecretToken(token []byte) error {
	if len(token) < 32 || len(token) > 4<<10 {
		return errors.New("must contain between 32 and 4096 bytes")
	}
	for _, value := range token {
		if value <= ' ' || value == 0x7f {
			return errors.New("must not contain whitespace or control characters")
		}
	}
	return nil
}

func bearerError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("WWW-Authenticate", `Bearer realm="interceptor-gateway"`)
	gatewayError(writer, status, code)
}

func gatewayError(writer http.ResponseWriter, status int, code string) {
	gatewayJSON(writer, status, map[string]any{"error": map[string]string{"code": code}})
}

func gatewayWrite(writer http.ResponseWriter, response GatewayResponse) {
	status := response.Status
	if status == http.StatusNoContent {
		status = http.StatusOK
	}
	gatewayJSON(writer, status, response)
}

func gatewayJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
