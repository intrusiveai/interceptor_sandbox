package adaptive

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
)

var allowedGatewayOperations = map[string]bool{"session.status": true, "operation.status": true, "capabilities.read": true, "artifact.register": true, "artifact.read": true, "attempt.register": true, "injection.arm": true, "injection.delete": true, "application.invoke": true, "observation.read": true, "snapshot.create": true, "session.stop": true}

const (
	defaultMaximumLeaseTTL = time.Hour
	maximumLeaseTTL        = 24 * time.Hour
	leaseRequestLimit      = 64 << 10
)

type GatewayResponse struct {
	Status          int             `json:"status"`
	Body            json.RawMessage `json:"body,omitempty"`
	SessionRevision uint64          `json:"session_revision"`
	CommittedBytes  int64           `json:"-"`
}

type GatewayExecutor interface {
	Execute(*http.Request, ExecutionLease, CrossVMRequest, json.RawMessage) (GatewayResponse, error)
}

type RequestRejectedError struct{ err error }

func (e RequestRejectedError) Error() string { return e.err.Error() }
func (e RequestRejectedError) Unwrap() error { return e.err }
func RejectRequest(err error) error          { return RequestRejectedError{err: err} }

type GatewayConfig struct {
	SessionID, EnvironmentDigest, ApplicationDigest, CapabilityManifestDigest, FeedbackProfile string
	IntegrationPrincipal                                                                       string
	IntegrationToken                                                                           []byte
	Executor                                                                                   GatewayExecutor
	MaximumLeaseTTL                                                                            time.Duration
	MaximumLeaseLimits                                                                         Limits
	MaxLeases                                                                                  int
	MaxFrameBytes                                                                              int64
	MaxReplayEntries                                                                           int
	Now                                                                                        func() time.Time
}

type leaseUsage struct {
	Attempts      int64 `json:"attempts"`
	Snapshots     int64 `json:"snapshot_creations"`
	ArtifactBytes int64 `json:"artifact_bytes"`
	SnapshotBytes int64 `json:"snapshot_bytes"`
	Active        int64 `json:"active_invocations"`
}

type replayEntry struct {
	bodyDigest string
	response   GatewayResponse
}

type issuanceReplay struct {
	bodyDigest string
	response   ExecutionLeaseResponse
}

type storedLease struct {
	lease       ExecutionLease
	tokenDigest [sha256.Size]byte
}

type Gateway struct {
	config                 GatewayConfig
	integrationTokenDigest [sha256.Size]byte
	mu                     sync.Mutex
	leases                 map[string]*storedLease
	leaseTokens            map[[sha256.Size]byte]string
	usage                  map[string]*leaseUsage
	replays                map[string]replayEntry
	inflight               map[string]string
	issuanceReplays        map[string]issuanceReplay
}

func NewGateway(config GatewayConfig) (*Gateway, error) {
	if !boundedID(config.SessionID) || !digest.Valid(config.EnvironmentDigest) || !digest.Valid(config.ApplicationDigest) || !digest.Valid(config.CapabilityManifestDigest) || config.FeedbackProfile == "" || !boundedID(config.IntegrationPrincipal) || config.Executor == nil {
		return nil, errors.New("gateway requires live session bindings, an integration principal, and an executor")
	}
	if err := validateSecretToken(config.IntegrationToken); err != nil {
		return nil, fmt.Errorf("gateway integration token: %w", err)
	}
	if config.MaximumLeaseTTL == 0 {
		config.MaximumLeaseTTL = defaultMaximumLeaseTTL
	}
	if config.MaximumLeaseTTL < time.Minute || config.MaximumLeaseTTL > maximumLeaseTTL {
		return nil, errors.New("gateway maximum lease TTL is invalid")
	}
	if zeroLimits(config.MaximumLeaseLimits) {
		config.MaximumLeaseLimits = Limits{MaximumAttempts: 1_000, MaximumConcurrentInvocations: 1, MaximumArtifactBytes: 1 << 30, MaximumWallSeconds: 3_600, MaximumSnapshotCreations: 100, MaximumCumulativeSnapshotBytes: 16 << 30}
	}
	if err := validateLimits(config.MaximumLeaseLimits, false); err != nil {
		return nil, fmt.Errorf("gateway maximum lease limits: %w", err)
	}
	if config.MaxLeases == 0 {
		config.MaxLeases = 1_000
	}
	if config.MaxLeases < 1 || config.MaxLeases > 100_000 {
		return nil, errors.New("gateway lease limit is invalid")
	}
	if config.MaxFrameBytes == 0 {
		config.MaxFrameBytes = 16 << 20
	}
	if config.MaxFrameBytes < 1 || config.MaxFrameBytes > 512<<20 {
		return nil, errors.New("gateway frame limit is invalid")
	}
	if config.MaxReplayEntries == 0 {
		config.MaxReplayEntries = 10_000
	}
	if config.MaxReplayEntries < 1 || config.MaxReplayEntries > 100_000 {
		return nil, errors.New("gateway replay limit is invalid")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	integrationTokenDigest := sha256.Sum256(config.IntegrationToken)
	config.IntegrationToken = nil
	return &Gateway{
		config:                 config,
		integrationTokenDigest: integrationTokenDigest,
		leases:                 map[string]*storedLease{},
		leaseTokens:            map[[sha256.Size]byte]string{},
		usage:                  map[string]*leaseUsage{},
		replays:                map[string]replayEntry{},
		inflight:               map[string]string{},
		issuanceReplays:        map[string]issuanceReplay{},
	}, nil
}

func (g *Gateway) Revoke(leaseID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if stored := g.leases[leaseID]; stored != nil {
		stored.lease.Status = "revoked"
		delete(g.leaseTokens, stored.tokenDigest)
	}
}

func (g *Gateway) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/v1/execution-leases":
		g.issueLease(writer, request)
	case strings.HasPrefix(request.URL.Path, "/v1/execution-leases/"):
		g.manageLease(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/v1/cross-vm":
		g.execute(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

func (g *Gateway) issueLease(writer http.ResponseWriter, request *http.Request) {
	if !g.authenticateIntegration(request) {
		bearerError(writer, http.StatusUnauthorized, "invalid_token")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, leaseRequestLimit+1))
	if err != nil || len(raw) > leaseRequestLimit {
		gatewayError(writer, http.StatusRequestEntityTooLarge, "lease_request_too_large")
		return
	}
	var input ExecutionLeaseRequest
	if err := strictJSON(raw, &input); err != nil {
		gatewayError(writer, http.StatusBadRequest, "invalid_lease_request")
		return
	}
	now := g.config.Now().UTC()
	if err := g.validateLeaseRequest(input, now); err != nil {
		gatewayError(writer, http.StatusConflict, "lease_request_rejected")
		return
	}
	bodyDigest := digest.Bytes(raw)
	replayKey := g.config.IntegrationPrincipal + "\x00" + input.IdempotencyKey
	g.mu.Lock()
	if previous, ok := g.issuanceReplays[replayKey]; ok {
		g.mu.Unlock()
		if previous.bodyDigest != bodyDigest {
			gatewayError(writer, http.StatusConflict, "idempotency_conflict")
			return
		}
		gatewayJSON(writer, http.StatusCreated, previous.response)
		return
	}
	if len(g.leases) >= g.config.MaxLeases {
		g.mu.Unlock()
		gatewayError(writer, http.StatusTooManyRequests, "lease_capacity_exhausted")
		return
	}
	leaseID, token, tokenDigest, err := newLeaseCredentials()
	if err != nil {
		g.mu.Unlock()
		gatewayError(writer, http.StatusInternalServerError, "credential_generation_failed")
		return
	}
	lease := ExecutionLease{
		APIVersion: ExecutionLeaseVersion, LeaseID: leaseID, Principal: g.config.IntegrationPrincipal,
		OperatorVMInstanceID: input.OperatorVMInstanceID, CampaignID: input.CampaignID, SessionID: input.SessionID,
		ScenarioBundleDigest: input.ScenarioBundleDigest, CapabilityManifestDigest: input.CapabilityManifestDigest,
		EnvironmentDigest: input.EnvironmentDigest, ApplicationDigest: input.ApplicationDigest,
		FeedbackProfile: input.FeedbackProfile, AllowedOperations: append([]string(nil), input.AllowedOperations...),
		Limits: normalizeLimits(input.Limits, g.config.MaximumLeaseLimits), IssuedAt: now, ExpiresAt: input.ExpiresAt.UTC(), Status: "active",
	}
	response := ExecutionLeaseResponse{Lease: lease, AccessToken: token, TokenType: "Bearer"}
	g.leases[leaseID] = &storedLease{lease: lease, tokenDigest: tokenDigest}
	g.leaseTokens[tokenDigest] = leaseID
	g.usage[leaseID] = &leaseUsage{}
	g.issuanceReplays[replayKey] = issuanceReplay{bodyDigest: bodyDigest, response: response}
	g.mu.Unlock()
	gatewayJSON(writer, http.StatusCreated, response)
}

func (g *Gateway) manageLease(writer http.ResponseWriter, request *http.Request) {
	if !g.authenticateIntegration(request) {
		bearerError(writer, http.StatusUnauthorized, "invalid_token")
		return
	}
	leaseID := strings.TrimPrefix(request.URL.Path, "/v1/execution-leases/")
	if !boundedID(leaseID) || strings.Contains(leaseID, "/") {
		http.NotFound(writer, request)
		return
	}
	g.mu.Lock()
	stored := g.leases[leaseID]
	if stored == nil {
		g.mu.Unlock()
		http.NotFound(writer, request)
		return
	}
	if stored.lease.Status == "active" && !g.config.Now().Before(stored.lease.ExpiresAt) {
		stored.lease.Status = "expired"
		delete(g.leaseTokens, stored.tokenDigest)
	}
	switch request.Method {
	case http.MethodGet:
		lease := stored.lease
		usage := *g.usage[leaseID]
		g.mu.Unlock()
		gatewayJSON(writer, http.StatusOK, map[string]any{"lease": lease, "usage": usage})
	case http.MethodDelete:
		stored.lease.Status = "revoked"
		delete(g.leaseTokens, stored.tokenDigest)
		g.mu.Unlock()
		writer.WriteHeader(http.StatusNoContent)
	default:
		g.mu.Unlock()
		writer.Header().Set("Allow", "GET, DELETE")
		gatewayError(writer, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

func (g *Gateway) execute(writer http.ResponseWriter, request *http.Request) {
	lease, ok := g.authenticateLease(request)
	if !ok {
		bearerError(writer, http.StatusUnauthorized, "invalid_token")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, g.config.MaxFrameBytes+1))
	if err != nil || int64(len(raw)) > g.config.MaxFrameBytes {
		gatewayError(writer, http.StatusRequestEntityTooLarge, "frame_too_large")
		return
	}
	var envelope struct {
		Request CrossVMRequest  `json:"request"`
		Body    json.RawMessage `json:"body"`
	}
	if err := strictJSON(raw, &envelope); err != nil {
		gatewayError(writer, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := g.validateRequest(lease, envelope.Request, envelope.Body); err != nil {
		gatewayError(writer, http.StatusConflict, "request_rejected")
		return
	}
	key := lease.LeaseID + "\x00" + envelope.Request.SessionID + "\x00" + envelope.Request.Operation + "\x00" + envelope.Request.IdempotencyKey
	g.mu.Lock()
	stored := g.leases[lease.LeaseID]
	if stored == nil || stored.lease.Status != "active" || !g.config.Now().Before(stored.lease.ExpiresAt) {
		g.mu.Unlock()
		gatewayError(writer, http.StatusForbidden, "lease_inactive")
		return
	}
	if previous, ok := g.replays[key]; ok {
		g.mu.Unlock()
		if previous.bodyDigest != envelope.Request.BodyDigest {
			gatewayError(writer, http.StatusConflict, "idempotency_conflict")
			return
		}
		gatewayWrite(writer, previous.response)
		return
	}
	if bodyDigest, ok := g.inflight[key]; ok {
		g.mu.Unlock()
		if bodyDigest != envelope.Request.BodyDigest {
			gatewayError(writer, http.StatusConflict, "idempotency_conflict")
		} else {
			gatewayError(writer, http.StatusConflict, "operation_in_progress")
		}
		return
	}
	if len(g.replays)+len(g.inflight) >= g.config.MaxReplayEntries {
		g.mu.Unlock()
		gatewayError(writer, http.StatusTooManyRequests, "replay_capacity_exhausted")
		return
	}
	if err := g.reserve(lease, envelope.Request, operationBudgetBytes(envelope.Request.Operation, envelope.Body)); err != nil {
		g.mu.Unlock()
		gatewayError(writer, http.StatusTooManyRequests, "lease_limit")
		return
	}
	g.inflight[key] = envelope.Request.BodyDigest
	g.mu.Unlock()
	result, err := g.config.Executor.Execute(request, lease, envelope.Request, envelope.Body)
	g.release(lease, envelope.Request)
	if err != nil {
		var rejected RequestRejectedError
		if errors.As(err, &rejected) {
			g.unreserve(lease, envelope.Request, operationBudgetBytes(envelope.Request.Operation, envelope.Body))
			g.clearInFlight(key)
			gatewayError(writer, http.StatusConflict, "request_rejected")
			return
		}
		unknown := GatewayResponse{Status: http.StatusConflict, Body: json.RawMessage(`{"state":"UNKNOWN","code":"operation_ambiguous"}`)}
		g.mu.Lock()
		g.replays[key] = replayEntry{bodyDigest: envelope.Request.BodyDigest, response: unknown}
		delete(g.inflight, key)
		g.mu.Unlock()
		gatewayWrite(writer, unknown)
		return
	}
	if result.Status < 200 || result.Status > 599 {
		g.clearInFlight(key)
		gatewayError(writer, http.StatusInternalServerError, "invalid_executor_response")
		return
	}
	if envelope.Request.Operation == "snapshot.create" && result.CommittedBytes > 0 {
		g.mu.Lock()
		usage := g.usage[lease.LeaseID]
		if lease.Limits.MaximumCumulativeSnapshotBytes > 0 && usage.SnapshotBytes+result.CommittedBytes > lease.Limits.MaximumCumulativeSnapshotBytes {
			stored := g.leases[lease.LeaseID]
			stored.lease.Status = "revoked"
			delete(g.leaseTokens, stored.tokenDigest)
			delete(g.inflight, key)
			g.mu.Unlock()
			gatewayError(writer, http.StatusConflict, "snapshot_budget_exceeded")
			return
		}
		usage.SnapshotBytes += result.CommittedBytes
		g.mu.Unlock()
	}
	g.mu.Lock()
	g.replays[key] = replayEntry{bodyDigest: envelope.Request.BodyDigest, response: result}
	delete(g.inflight, key)
	g.mu.Unlock()
	gatewayWrite(writer, result)
}

func (g *Gateway) authenticateIntegration(request *http.Request) bool {
	token, ok := bearerToken(request)
	if !ok {
		return false
	}
	candidate := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(candidate[:], g.integrationTokenDigest[:]) == 1
}

func (g *Gateway) authenticateLease(request *http.Request) (ExecutionLease, bool) {
	token, ok := bearerToken(request)
	if !ok {
		return ExecutionLease{}, false
	}
	tokenDigest := sha256.Sum256([]byte(token))
	g.mu.Lock()
	defer g.mu.Unlock()
	leaseID, ok := g.leaseTokens[tokenDigest]
	if !ok {
		return ExecutionLease{}, false
	}
	stored := g.leases[leaseID]
	if stored == nil || stored.lease.Status != "active" || !g.config.Now().Before(stored.lease.ExpiresAt) {
		if stored != nil && stored.lease.Status == "active" {
			stored.lease.Status = "expired"
			delete(g.leaseTokens, tokenDigest)
		}
		return ExecutionLease{}, false
	}
	return stored.lease, true
}

func bearerToken(request *http.Request) (string, bool) {
	value := request.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") || strings.Count(value, " ") != 1 {
		return "", false
	}
	token := strings.TrimPrefix(value, "Bearer ")
	return token, token != ""
}

func (g *Gateway) validateLeaseRequest(input ExecutionLeaseRequest, now time.Time) error {
	if input.APIVersion != ExecutionLeaseRequestVersion || !boundedID(input.IdempotencyKey) || !boundedID(input.OperatorVMInstanceID) || !boundedID(input.CampaignID) || input.SessionID != g.config.SessionID || input.FeedbackProfile != g.config.FeedbackProfile || input.ExpiresAt.IsZero() || !input.ExpiresAt.After(now) || input.ExpiresAt.Sub(now) > g.config.MaximumLeaseTTL {
		return errors.New("lease request has invalid required fields")
	}
	if input.EnvironmentDigest != g.config.EnvironmentDigest || input.ApplicationDigest != g.config.ApplicationDigest || input.CapabilityManifestDigest != g.config.CapabilityManifestDigest {
		return errors.New("lease request binding mismatch")
	}
	if input.ScenarioBundleDigest != "" && !digest.Valid(input.ScenarioBundleDigest) {
		return errors.New("lease request scenario digest is invalid")
	}
	if len(input.AllowedOperations) == 0 || len(input.AllowedOperations) > 32 {
		return errors.New("lease request operation set is invalid")
	}
	seen := map[string]bool{}
	for _, operation := range input.AllowedOperations {
		if !allowedGatewayOperations[operation] || seen[operation] {
			return errors.New("lease request contains unsupported or duplicate operation")
		}
		seen[operation] = true
	}
	if err := validateLimits(input.Limits, true); err != nil {
		return err
	}
	if exceedsLimits(input.Limits, g.config.MaximumLeaseLimits) {
		return errors.New("lease request exceeds gateway limits")
	}
	return nil
}

func (g *Gateway) validateRequest(lease ExecutionLease, request CrossVMRequest, body []byte) error {
	if request.APIVersion != CrossVMRequestVersion || !boundedID(request.RequestID) || !boundedID(request.IdempotencyKey) || request.LeaseID != lease.LeaseID || request.SessionID != lease.SessionID || request.OperatorVMInstanceID != lease.OperatorVMInstanceID || !allowed(lease.AllowedOperations, request.Operation) || !digest.Valid(request.BodyDigest) || digest.Bytes(body) != request.BodyDigest || request.Deadline.IsZero() || !g.config.Now().Before(request.Deadline) || request.Deadline.After(lease.ExpiresAt) {
		return errors.New("invalid cross-VM request")
	}
	if request.AttemptContextDigest != "" && !digest.Valid(request.AttemptContextDigest) {
		return errors.New("invalid attempt context digest")
	}
	if request.AttemptID != "" && !boundedID(request.AttemptID) {
		return errors.New("invalid attempt ID")
	}
	if lease.Limits.MaximumWallSeconds > 0 && !g.config.Now().Before(lease.IssuedAt.Add(time.Duration(lease.Limits.MaximumWallSeconds)*time.Second)) {
		return errors.New("lease wall-time limit is exhausted")
	}
	return nil
}

func (g *Gateway) reserve(lease ExecutionLease, request CrossVMRequest, bodyBytes int64) error {
	usage := g.usage[lease.LeaseID]
	switch request.Operation {
	case "attempt.register":
		if usage.Attempts >= lease.Limits.MaximumAttempts {
			return errors.New("attempt limit")
		}
		usage.Attempts++
	case "artifact.register":
		if bodyBytes > lease.Limits.MaximumArtifactBytes-usage.ArtifactBytes {
			return errors.New("artifact limit")
		}
		usage.ArtifactBytes += bodyBytes
	case "snapshot.create":
		if usage.Snapshots >= lease.Limits.MaximumSnapshotCreations {
			return errors.New("snapshot limit")
		}
		usage.Snapshots++
	case "application.invoke":
		if usage.Active >= lease.Limits.MaximumConcurrentInvocations {
			return errors.New("concurrency limit")
		}
		usage.Active++
	}
	return nil
}

func operationBudgetBytes(operation string, body []byte) int64 {
	if operation == "artifact.register" {
		var value struct {
			Descriptor ArtifactDescriptor `json:"descriptor"`
		}
		if json.Unmarshal(body, &value) == nil && value.Descriptor.SizeBytes >= 0 {
			return value.Descriptor.SizeBytes
		}
	}
	return int64(len(body))
}

func (g *Gateway) release(lease ExecutionLease, request CrossVMRequest) {
	if request.Operation != "application.invoke" {
		return
	}
	g.mu.Lock()
	if usage := g.usage[lease.LeaseID]; usage != nil && usage.Active > 0 {
		usage.Active--
	}
	g.mu.Unlock()
}

func (g *Gateway) unreserve(lease ExecutionLease, request CrossVMRequest, bodyBytes int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	usage := g.usage[lease.LeaseID]
	if usage == nil {
		return
	}
	switch request.Operation {
	case "attempt.register":
		if usage.Attempts > 0 {
			usage.Attempts--
		}
	case "artifact.register":
		if bodyBytes <= usage.ArtifactBytes {
			usage.ArtifactBytes -= bodyBytes
		}
	case "snapshot.create":
		if usage.Snapshots > 0 {
			usage.Snapshots--
		}
	}
}

func (g *Gateway) clearInFlight(key string) {
	g.mu.Lock()
	delete(g.inflight, key)
	g.mu.Unlock()
}

func allowed(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func strictJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request must contain exactly one JSON value")
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

func newLeaseCredentials() (string, string, [sha256.Size]byte, error) {
	idBytes := make([]byte, 16)
	tokenBytes := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, idBytes); err != nil {
		return "", "", [sha256.Size]byte{}, err
	}
	if _, err := io.ReadFull(rand.Reader, tokenBytes); err != nil {
		return "", "", [sha256.Size]byte{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	return "lease-" + hex.EncodeToString(idBytes), token, sha256.Sum256([]byte(token)), nil
}

func validateLimits(limits Limits, allowZero bool) error {
	values := []struct {
		value, maximum int64
	}{
		{limits.MaximumAttempts, 1_000_000},
		{limits.MaximumConcurrentInvocations, 1_024},
		{limits.MaximumArtifactBytes, 1 << 40},
		{limits.MaximumWallSeconds, 86_400},
		{limits.MaximumSnapshotCreations, 10_000},
		{limits.MaximumCumulativeSnapshotBytes, 16 << 40},
	}
	for _, item := range values {
		if item.value < 0 || item.value > item.maximum || (!allowZero && item.value == 0) {
			return errors.New("lease limits are out of bounds")
		}
	}
	return nil
}

func normalizeLimits(requested, ceiling Limits) Limits {
	if requested.MaximumAttempts == 0 {
		requested.MaximumAttempts = ceiling.MaximumAttempts
	}
	if requested.MaximumConcurrentInvocations == 0 {
		requested.MaximumConcurrentInvocations = ceiling.MaximumConcurrentInvocations
	}
	if requested.MaximumArtifactBytes == 0 {
		requested.MaximumArtifactBytes = ceiling.MaximumArtifactBytes
	}
	if requested.MaximumWallSeconds == 0 {
		requested.MaximumWallSeconds = ceiling.MaximumWallSeconds
	}
	if requested.MaximumSnapshotCreations == 0 {
		requested.MaximumSnapshotCreations = ceiling.MaximumSnapshotCreations
	}
	if requested.MaximumCumulativeSnapshotBytes == 0 {
		requested.MaximumCumulativeSnapshotBytes = ceiling.MaximumCumulativeSnapshotBytes
	}
	return requested
}

func exceedsLimits(requested, ceiling Limits) bool {
	return requested.MaximumAttempts > ceiling.MaximumAttempts ||
		requested.MaximumConcurrentInvocations > ceiling.MaximumConcurrentInvocations ||
		requested.MaximumArtifactBytes > ceiling.MaximumArtifactBytes ||
		requested.MaximumWallSeconds > ceiling.MaximumWallSeconds ||
		requested.MaximumSnapshotCreations > ceiling.MaximumSnapshotCreations ||
		requested.MaximumCumulativeSnapshotBytes > ceiling.MaximumCumulativeSnapshotBytes
}

func zeroLimits(limits Limits) bool { return limits == (Limits{}) }

func bearerError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("WWW-Authenticate", `Bearer realm="interceptor-gateway"`)
	gatewayError(writer, status, code)
}

func gatewayError(writer http.ResponseWriter, status int, code string) {
	gatewayJSON(writer, status, map[string]any{"error": map[string]string{"code": code}})
}

func gatewayWrite(writer http.ResponseWriter, response GatewayResponse) {
	gatewayJSON(writer, response.Status, response)
}

func gatewayJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
