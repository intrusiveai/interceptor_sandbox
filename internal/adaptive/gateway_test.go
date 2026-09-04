package adaptive

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
)

const testIntegrationToken = "this-is-a-long-integration-secret-token"

type fakeExecutor struct{ calls int }

func (f *fakeExecutor) Execute(_ *http.Request, _ ExecutionLease, _ CrossVMRequest, _ json.RawMessage) (GatewayResponse, error) {
	f.calls++
	return GatewayResponse{Status: http.StatusOK, Body: json.RawMessage(`{"ok":true}`), SessionRevision: 2}, nil
}

type blockingExecutor struct {
	started chan struct{}
	release chan struct{}
	calls   int
}

func (b *blockingExecutor) Execute(_ *http.Request, _ ExecutionLease, _ CrossVMRequest, _ json.RawMessage) (GatewayResponse, error) {
	b.calls++
	close(b.started)
	<-b.release
	return GatewayResponse{Status: http.StatusOK, Body: json.RawMessage(`{"ok":true}`), SessionRevision: 2}, nil
}

func TestGatewayIssuesLeaseExecutesIdempotentlyAndRevokes(t *testing.T) {
	now := time.Now().UTC()
	binding := digest.Bytes([]byte("binding"))
	executor := &fakeExecutor{}
	gateway := testGateway(t, now, binding, executor)
	issued := issueTestLease(t, gateway, now, binding, "lease-idem-1", []string{"application.invoke"}, Limits{})
	if issued.Lease.Principal != "operator-runtime" || issued.Lease.Status != "active" || issued.AccessToken == "" {
		t.Fatalf("unexpected issued lease: %+v", issued)
	}
	if issued.Lease.Limits.MaximumConcurrentInvocations != 1 {
		t.Fatalf("optional limit did not inherit server ceiling: %+v", issued.Lease.Limits)
	}

	body := json.RawMessage(`{"attempt_id":"attempt-1"}`)
	request := CrossVMRequest{APIVersion: CrossVMRequestVersion, RequestID: "request-1", IdempotencyKey: "idem-1", LeaseID: issued.Lease.LeaseID, Operation: "application.invoke", SessionID: "session-1", OperatorVMInstanceID: "opvm-1", AttemptID: "attempt-1", BodyDigest: digest.Bytes(body), Deadline: now.Add(time.Minute)}
	envelope, _ := json.Marshal(map[string]any{"request": request, "body": body})
	for i := 0; i < 2; i++ {
		response := serveAuthorized(gateway, http.MethodPost, "/v1/cross-vm", issued.AccessToken, envelope)
		if response.Code != http.StatusOK {
			t.Fatalf("request %d failed: %d %s", i, response.Code, response.Body.String())
		}
	}
	if executor.calls != 1 {
		t.Fatalf("idempotent retry executed %d times", executor.calls)
	}

	inspect := serveAuthorized(gateway, http.MethodGet, "/v1/execution-leases/"+issued.Lease.LeaseID, testIntegrationToken, nil)
	if inspect.Code != http.StatusOK || strings.Contains(inspect.Body.String(), issued.AccessToken) {
		t.Fatalf("unsafe lease introspection response: %d %s", inspect.Code, inspect.Body.String())
	}
	revoke := serveAuthorized(gateway, http.MethodDelete, "/v1/execution-leases/"+issued.Lease.LeaseID, testIntegrationToken, nil)
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("lease revocation returned %d", revoke.Code)
	}
	response := serveAuthorized(gateway, http.MethodPost, "/v1/cross-vm", issued.AccessToken, envelope)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked lease token returned %d", response.Code)
	}
}

func TestGatewayLeaseIssuanceRequiresIntegrationTokenAndIsIdempotent(t *testing.T) {
	now := time.Now().UTC()
	binding := digest.Bytes([]byte("binding"))
	gateway := testGateway(t, now, binding, &fakeExecutor{})
	input := testLeaseRequest(now, binding, "lease-idem-1", []string{"session.status"}, Limits{})
	body, _ := json.Marshal(input)

	unauthorized := serveAuthorized(gateway, http.MethodPost, "/v1/execution-leases", "wrong-token-that-is-long-enough-to-look-real", body)
	if unauthorized.Code != http.StatusUnauthorized || unauthorized.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("unauthorized issuance returned %d", unauthorized.Code)
	}
	first := serveAuthorized(gateway, http.MethodPost, "/v1/execution-leases", testIntegrationToken, body)
	second := serveAuthorized(gateway, http.MethodPost, "/v1/execution-leases", testIntegrationToken, body)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated || first.Body.String() != second.Body.String() {
		t.Fatalf("idempotent issuance mismatch: first=%d %s second=%d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}

	input.CampaignID = "different-campaign"
	conflicting, _ := json.Marshal(input)
	conflict := serveAuthorized(gateway, http.MethodPost, "/v1/execution-leases", testIntegrationToken, conflicting)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("issuance idempotency conflict returned %d", conflict.Code)
	}
}

func TestGatewayRejectsLeaseRequestAboveServerCeiling(t *testing.T) {
	now := time.Now().UTC()
	binding := digest.Bytes([]byte("binding"))
	gateway := testGateway(t, now, binding, &fakeExecutor{})
	input := testLeaseRequest(now, binding, "lease-idem-1", []string{"application.invoke"}, Limits{MaximumConcurrentInvocations: 2})
	body, _ := json.Marshal(input)
	response := serveAuthorized(gateway, http.MethodPost, "/v1/execution-leases", testIntegrationToken, body)
	if response.Code != http.StatusConflict {
		t.Fatalf("over-limit lease request returned %d: %s", response.Code, response.Body.String())
	}
}

func TestGatewayExpiresLeaseAndDoesNotDiscloseToken(t *testing.T) {
	current := time.Now().UTC()
	binding := digest.Bytes([]byte("binding"))
	gateway, err := NewGateway(GatewayConfig{
		SessionID: "session-1", EnvironmentDigest: binding, ApplicationDigest: binding,
		CapabilityManifestDigest: binding, FeedbackProfile: "black-box",
		IntegrationPrincipal: "operator-runtime", IntegrationToken: []byte(testIntegrationToken), Executor: &fakeExecutor{},
		MaximumLeaseLimits: Limits{MaximumAttempts: 10, MaximumConcurrentInvocations: 1, MaximumArtifactBytes: 1 << 20, MaximumWallSeconds: 600, MaximumSnapshotCreations: 4, MaximumCumulativeSnapshotBytes: 4 << 20},
		Now:                func() time.Time { return current },
	})
	if err != nil {
		t.Fatal(err)
	}
	input := testLeaseRequest(current, binding, "lease-idem-expiry", []string{"session.status"}, Limits{})
	input.ExpiresAt = current.Add(time.Minute)
	body, _ := json.Marshal(input)
	response := serveAuthorized(gateway, http.MethodPost, "/v1/execution-leases", testIntegrationToken, body)
	var issued ExecutionLeaseResponse
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &issued) != nil {
		t.Fatalf("lease issuance failed: %d %s", response.Code, response.Body.String())
	}
	current = current.Add(2 * time.Minute)
	inspect := serveAuthorized(gateway, http.MethodGet, "/v1/execution-leases/"+issued.Lease.LeaseID, testIntegrationToken, nil)
	if inspect.Code != http.StatusOK || !strings.Contains(inspect.Body.String(), `"status":"expired"`) || strings.Contains(inspect.Body.String(), issued.AccessToken) {
		t.Fatalf("expired lease introspection was unsafe: %d %s", inspect.Code, inspect.Body.String())
	}
}

func TestGatewayDoesNotExecuteConcurrentIdempotentMutationTwice(t *testing.T) {
	now := time.Now().UTC()
	binding := digest.Bytes([]byte("binding"))
	executor := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	gateway := testGateway(t, now, binding, executor)
	issued := issueTestLease(t, gateway, now, binding, "lease-idem-1", []string{"application.invoke"}, Limits{})
	body := json.RawMessage(`{"attempt_id":"attempt-1"}`)
	request := CrossVMRequest{APIVersion: CrossVMRequestVersion, RequestID: "request-1", IdempotencyKey: "idem-1", LeaseID: issued.Lease.LeaseID, Operation: "application.invoke", SessionID: "session-1", OperatorVMInstanceID: "opvm-1", AttemptID: "attempt-1", BodyDigest: digest.Bytes(body), Deadline: now.Add(time.Minute)}
	envelope, _ := json.Marshal(map[string]any{"request": request, "body": body})
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- serveAuthorized(gateway, http.MethodPost, "/v1/cross-vm", issued.AccessToken, envelope)
	}()
	<-executor.started
	second := serveAuthorized(gateway, http.MethodPost, "/v1/cross-vm", issued.AccessToken, envelope)
	if second.Code != http.StatusConflict {
		t.Fatalf("concurrent replay returned %d", second.Code)
	}
	close(executor.release)
	if first := <-firstDone; first.Code != http.StatusOK || executor.calls != 1 {
		t.Fatalf("first call=%d executor calls=%d", first.Code, executor.calls)
	}
}

func testGateway(t *testing.T, now time.Time, binding string, executor GatewayExecutor) *Gateway {
	t.Helper()
	gateway, err := NewGateway(GatewayConfig{
		SessionID: "session-1", EnvironmentDigest: binding, ApplicationDigest: binding,
		CapabilityManifestDigest: binding, FeedbackProfile: "black-box",
		IntegrationPrincipal: "operator-runtime", IntegrationToken: []byte(testIntegrationToken), Executor: executor,
		MaximumLeaseLimits: Limits{MaximumAttempts: 10, MaximumConcurrentInvocations: 1, MaximumArtifactBytes: 1 << 20, MaximumWallSeconds: 600, MaximumSnapshotCreations: 4, MaximumCumulativeSnapshotBytes: 4 << 20},
		Now:                func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return gateway
}

func issueTestLease(t *testing.T, gateway *Gateway, now time.Time, binding, idempotencyKey string, operations []string, limits Limits) ExecutionLeaseResponse {
	t.Helper()
	input := testLeaseRequest(now, binding, idempotencyKey, operations, limits)
	body, _ := json.Marshal(input)
	response := serveAuthorized(gateway, http.MethodPost, "/v1/execution-leases", testIntegrationToken, body)
	if response.Code != http.StatusCreated {
		t.Fatalf("lease issuance failed: %d %s", response.Code, response.Body.String())
	}
	var issued ExecutionLeaseResponse
	if err := json.Unmarshal(response.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	return issued
}

func testLeaseRequest(now time.Time, binding, idempotencyKey string, operations []string, limits Limits) ExecutionLeaseRequest {
	return ExecutionLeaseRequest{APIVersion: ExecutionLeaseRequestVersion, IdempotencyKey: idempotencyKey, OperatorVMInstanceID: "opvm-1", CampaignID: "campaign-1", SessionID: "session-1", CapabilityManifestDigest: binding, EnvironmentDigest: binding, ApplicationDigest: binding, FeedbackProfile: "black-box", AllowedOperations: operations, Limits: limits, ExpiresAt: now.Add(5 * time.Minute)}
}

func serveAuthorized(gateway *Gateway, method, path, token string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)
	return response
}
