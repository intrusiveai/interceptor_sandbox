package identity

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func TestClientCredentialsAndCurrentGrantAuthorization(t *testing.T) {
	authority := testAuthority(t)
	token := requestToken(t, authority, url.Values{
		"grant_type": {"client_credentials"}, "client_id": {"support-client"},
		"client_secret": {"synthetic-secret"}, "scope": {"tickets:read"},
		"resource": {"http://tickets.sandbox.local:18100"},
	})
	claims, err := authority.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "support-agent" || claims.Actor != nil || !contains(claims.Scopes, "tickets:read") {
		t.Fatalf("unexpected client-credentials claims: %#v", claims)
	}

	handler := authority.Authorize("tickets", servicePolicy(), http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "http://tickets.sandbox.local:18100/tickets", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("authorized request returned %d: %s", response.Code, response.Body.String())
	}

	state := authority.Snapshot()["grants"]["agent-read"]
	var grant GrantState
	if err := json.Unmarshal(state, &grant); err != nil {
		t.Fatal(err)
	}
	grant.Active = false
	encoded, _ := json.Marshal(grant)
	operation := statejournal.Operation{Kind: "service_put", Service: "sandbox-identity", Collection: "grants", Key: grant.ID, Value: encoded}
	if err := authority.Apply(operation); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("inactive current grant returned %d", response.Code)
	}
}

func TestDelegatedTokenExchangePreservesUserAndAgent(t *testing.T) {
	authority := testAuthority(t)
	caller, _, err := authority.IssueCallerToken("alice", "support-agent", []string{"agent:invoke"})
	if err != nil {
		t.Fatal(err)
	}
	token := requestToken(t, authority, url.Values{
		"grant_type": {tokenExchangeGrant}, "client_id": {"support-client"},
		"client_secret": {"synthetic-secret"}, "subject_token": {caller},
		"scope": {"tickets:read"}, "resource": {"http://tickets.sandbox.local:18100"},
	})
	claims, err := authority.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "alice" || claims.Actor == nil || claims.Actor.Subject != "support-agent" {
		t.Fatalf("delegated identity chain was not preserved: %#v", claims)
	}
	handler := authority.Authorize("tickets", servicePolicy(), http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		contextClaims, ok := ClaimsFromContext(request.Context())
		if !ok || contextClaims.Subject != "alice" || contextClaims.Actor.Subject != "support-agent" {
			t.Errorf("authorized claims absent from request context: %#v", contextClaims)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "http://tickets.sandbox.local:18100/tickets", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delegated request returned %d: %s", response.Code, response.Body.String())
	}
}

func TestDelegatedTokenExchangeRejectsCallerTokenForAnotherAgent(t *testing.T) {
	authority := testAuthority(t)
	authority.principals["other-agent"] = environment.IdentityPrincipal{ID: "other-agent", Kind: "agent", Issuer: "sandbox-identity"}
	caller, _, err := authority.IssueCallerToken("alice", "other-agent", nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://sandbox-identity.sandbox.local:18300/token", strings.NewReader(url.Values{
		"grant_type": {tokenExchangeGrant}, "client_id": {"support-client"},
		"client_secret": {"synthetic-secret"}, "subject_token": {caller},
		"scope": {"tickets:read"}, "resource": {"http://tickets.sandbox.local:18100"},
	}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	authority.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "not issued for this agent") {
		t.Fatalf("unexpected token exchange response: %d %s", response.Code, response.Body.String())
	}
}

func TestCallerTokenRejectsMalformedScope(t *testing.T) {
	authority := testAuthority(t)
	if _, _, err := authority.IssueCallerToken("alice", "support-agent", []string{"bad scope"}); err == nil {
		t.Fatal("caller token accepted a malformed scope")
	}
}

func TestMCPProtectedResourceMetadataAndProjectedDelivery(t *testing.T) {
	authority := testAuthority(t)
	handler := authority.Authorize("tickets", servicePolicy(), http.NotFoundHandler())
	request := httptest.NewRequest(http.MethodGet, "http://tickets.sandbox.local:18100/.well-known/oauth-protected-resource", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "sandbox-identity.sandbox.local") {
		t.Fatalf("unexpected protected resource metadata: %d %s", response.Code, response.Body.String())
	}

	delivery := environment.IdentityDelivery{ID: "projected-agent", Kind: "projected_jwt", Issuer: "sandbox-identity", Principal: "support-agent", Audience: "local-workload", Path: "/run/interceptor-session/identity/agent.jwt", EnvironmentVariable: "AGENT_TOKEN_FILE"}
	token, claims, err := authority.ProjectedToken(delivery)
	if err != nil || claims.Subject != "support-agent" {
		t.Fatalf("projected token: claims=%#v err=%v", claims, err)
	}
	if _, err := authority.Verify(token); err != nil {
		t.Fatalf("verify projected token: %v", err)
	}
}

func TestAWSContainerCredentialDeliveryUsesSDKAcceptedLoopbackURI(t *testing.T) {
	authority := testAuthority(t)
	delivery := environment.IdentityDelivery{
		ID: "aws-agent", Kind: "aws_container_credentials", Issuer: "sandbox-identity", Principal: "support-agent",
		EndpointEnvironment: "AWS_CONTAINER_CREDENTIALS_FULL_URI", AuthorizationTokenEnvironment: "AWS_CONTAINER_AUTHORIZATION_TOKEN",
	}
	authority.deliveries[delivery.ID] = delivery
	values, err := authority.DeliveryEnvironment(delivery)
	if err != nil {
		t.Fatal(err)
	}
	if values[delivery.EndpointEnvironment] != "http://127.0.0.1:18300/aws/container-credentials/support-agent" {
		t.Fatalf("unexpected AWS credential URI %q", values[delivery.EndpointEnvironment])
	}
	if !strings.HasPrefix(values[delivery.AuthorizationTokenEnvironment], "Bearer interceptor-") {
		t.Fatalf("unexpected AWS credential authorization token %q", values[delivery.AuthorizationTokenEnvironment])
	}
}

func TestIdentityStateReplaysFromSnapshotJournal(t *testing.T) {
	source := testAuthority(t)
	journalPath := filepath.Join(t.TempDir(), "state.jsonl")
	journal, err := statejournal.Open(journalPath, "sess-identity-test")
	if err != nil {
		t.Fatal(err)
	}
	var delegation DelegationState
	if err := json.Unmarshal(source.Snapshot()["delegations"]["alice-support"], &delegation); err != nil {
		t.Fatal(err)
	}
	delegation.Active = false
	encoded, _ := json.Marshal(delegation)
	operation := statejournal.Operation{Kind: "service_put", Service: "sandbox-identity", Collection: "delegations", Key: delegation.ID, Value: encoded}
	if _, err := journal.Append("identity_fidelity_injection", []statejournal.Operation{operation}); err != nil {
		t.Fatal(err)
	}
	if err := source.Apply(operation); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	restored := testAuthority(t)
	transactions, err := statejournal.VerifyFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, transaction := range transactions {
		for _, replay := range transaction.Operations {
			if err := restored.Apply(replay); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !reflect.DeepEqual(source.Snapshot(), restored.Snapshot()) {
		t.Fatalf("restored identity state differs:\nsource=%#v\nrestored=%#v", source.Snapshot(), restored.Snapshot())
	}
}

func testAuthority(t *testing.T) *Authority {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	issuer := environment.IdentityIssuer{ID: "sandbox-identity", Kind: "oidc", Listen: "127.0.0.1:18300", Hostnames: []string{"sandbox-identity.sandbox.local"}, IssuerURL: "http://sandbox-identity.sandbox.local:18300", AccessTokenTTLSeconds: 600}
	principals := []environment.IdentityPrincipal{
		{ID: "support-agent", Kind: "agent", Issuer: issuer.ID, ClientID: "support-client", ClientSecret: "synthetic-secret"},
		{ID: "alice", Kind: "user", Issuer: issuer.ID},
	}
	grants := []environment.IdentityGrant{{ID: "agent-read", Principal: "support-agent", Service: "tickets", Scopes: []string{"tickets:read"}}}
	delegations := []environment.IdentityDelegation{{ID: "alice-support", Agent: "support-agent", User: "alice", Service: "tickets", Scopes: []string{"tickets:read"}}}
	services := []environment.Service{{ID: "tickets", Authorization: &environment.ServiceAuthorization{Issuer: issuer.ID, Audience: "http://tickets.sandbox.local:18100", RequiredScopes: []string{"tickets:read"}, AllowDelegated: true}}}
	authority, err := NewAuthority(issuer, principals, grants, delegations, nil, services, nil, WithSigningKey(key), WithClock(func() time.Time { return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC) }))
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func requestToken(t *testing.T, authority *Authority, form url.Values) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://sandbox-identity.sandbox.local:18300/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	authority.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("token endpoint returned %d: %s", response.Code, response.Body.String())
	}
	var output struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(bytes.NewReader(response.Body.Bytes())).Decode(&output); err != nil || output.AccessToken == "" {
		t.Fatalf("decode token response: %v %#v", err, output)
	}
	return output.AccessToken
}

func servicePolicy() environment.ServiceAuthorization {
	return environment.ServiceAuthorization{Issuer: "sandbox-identity", Audience: "http://tickets.sandbox.local:18100", RequiredScopes: []string{"tickets:read"}, AllowDelegated: true}
}
