package identity

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

const tokenExchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange"

type GrantState struct {
	ID        string   `json:"id"`
	Principal string   `json:"principal"`
	Service   string   `json:"service"`
	Scopes    []string `json:"scopes"`
	Active    bool     `json:"active"`
}

type DelegationState struct {
	ID      string   `json:"id"`
	Agent   string   `json:"agent"`
	User    string   `json:"user"`
	Service string   `json:"service"`
	Scopes  []string `json:"scopes"`
	Active  bool     `json:"active"`
}

type FaultState struct {
	Unavailable    bool `json:"unavailable"`
	RejectExchange bool `json:"reject_exchange"`
	IssueExpired   bool `json:"issue_expired"`
}

type Actor struct {
	Subject string `json:"sub"`
}

type Claims struct {
	Issuer        string
	Subject       string
	Audience      []string
	ExpiresAt     time.Time
	IssuedAt      time.Time
	TokenID       string
	Scopes        []string
	ClientID      string
	PrincipalKind string
	Actor         *Actor
	Raw           map[string]any
}

type contextKey struct{}

func ClaimsFromContext(ctx context.Context) (Claims, bool) {
	claims, ok := ctx.Value(contextKey{}).(Claims)
	return claims, ok
}

type Option func(*Authority)

func WithClock(now func() time.Time) Option {
	return func(authority *Authority) { authority.now = now }
}

func WithSigningKey(key *rsa.PrivateKey) Option {
	return func(authority *Authority) { authority.key = key }
}

type Authority struct {
	issuer      environment.IdentityIssuer
	principals  map[string]environment.IdentityPrincipal
	clients     map[string]string
	audiences   map[string]string
	deliveries  map[string]environment.IdentityDelivery
	recorder    *recording.Writer
	now         func() time.Time
	key         *rsa.PrivateKey
	kid         string
	mu          sync.RWMutex
	grants      map[string]GrantState
	delegations map[string]DelegationState
	faults      FaultState
}

func NewAuthority(
	issuer environment.IdentityIssuer,
	principals []environment.IdentityPrincipal,
	grants []environment.IdentityGrant,
	delegations []environment.IdentityDelegation,
	deliveries []environment.IdentityDelivery,
	services []environment.Service,
	recorder *recording.Writer,
	options ...Option,
) (*Authority, error) {
	authority := &Authority{
		issuer: issuer, principals: map[string]environment.IdentityPrincipal{}, clients: map[string]string{},
		audiences: map[string]string{}, deliveries: map[string]environment.IdentityDelivery{}, recorder: recorder,
		now: time.Now, grants: map[string]GrantState{}, delegations: map[string]DelegationState{},
	}
	for _, option := range options {
		option(authority)
	}
	if authority.key == nil {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, fmt.Errorf("generate synthetic issuer key: %w", err)
		}
		authority.key = key
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&authority.key.PublicKey)
	if err != nil {
		return nil, err
	}
	authority.kid = strings.TrimPrefix(digest.Bytes(publicDER), "sha256:")[:16]
	for _, principal := range principals {
		if principal.Issuer != issuer.ID {
			continue
		}
		authority.principals[principal.ID] = principal
		if principal.ClientID != "" {
			authority.clients[principal.ClientID] = principal.ID
		}
	}
	for _, service := range services {
		if service.Authorization != nil && service.Authorization.Issuer == issuer.ID {
			authority.audiences[service.Authorization.Audience] = service.ID
		}
	}
	for _, grant := range grants {
		if _, exists := authority.principals[grant.Principal]; exists {
			authority.grants[grant.ID] = GrantState{ID: grant.ID, Principal: grant.Principal, Service: grant.Service, Scopes: cloneStrings(grant.Scopes), Active: true}
		}
	}
	for _, delegation := range delegations {
		if _, exists := authority.principals[delegation.Agent]; exists {
			authority.delegations[delegation.ID] = DelegationState{ID: delegation.ID, Agent: delegation.Agent, User: delegation.User, Service: delegation.Service, Scopes: cloneStrings(delegation.Scopes), Active: true}
		}
	}
	for _, delivery := range deliveries {
		if delivery.Issuer == issuer.ID {
			authority.deliveries[delivery.ID] = delivery
		}
	}
	return authority, nil
}

func (a *Authority) ID() string { return a.issuer.ID }

func (a *Authority) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	a.mu.RLock()
	unavailable := a.faults.Unavailable
	a.mu.RUnlock()
	if unavailable {
		a.writeOAuthError(writer, http.StatusServiceUnavailable, "temporarily_unavailable", "synthetic identity issuer is unavailable")
		a.record("identity_issuer_unavailable", map[string]any{"method": request.Method, "path": request.URL.Path})
		return
	}
	switch {
	case request.Method == http.MethodGet && (request.URL.Path == "/.well-known/openid-configuration" || request.URL.Path == "/.well-known/oauth-authorization-server"):
		a.discovery(writer)
	case request.Method == http.MethodGet && request.URL.Path == "/jwks":
		a.jwks(writer)
	case request.Method == http.MethodPost && request.URL.Path == "/token":
		a.token(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/aws/container-credentials/"):
		a.awsCredentials(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

func (a *Authority) discovery(writer http.ResponseWriter) {
	issuer := strings.TrimSuffix(a.issuer.IssuerURL, "/")
	writeJSON(writer, http.StatusOK, map[string]any{
		"issuer": issuer, "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks",
		"grant_types_supported":                 []string{"client_credentials", tokenExchangeGrant},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"scopes_supported":                      a.allScopes(),
		"response_types_supported":              []string{},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (a *Authority) jwks(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusOK, map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": a.kid,
		"n": base64.RawURLEncoding.EncodeToString(a.key.PublicKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(bigEndian(a.key.PublicKey.E)),
	}}})
}

func (a *Authority) token(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	if err := request.ParseForm(); err != nil {
		a.writeOAuthError(writer, http.StatusBadRequest, "invalid_request", "token request is invalid")
		return
	}
	principal, err := a.authenticateClient(request)
	if err != nil {
		writer.Header().Set("WWW-Authenticate", `Basic realm="synthetic-identity"`)
		a.writeOAuthError(writer, http.StatusUnauthorized, "invalid_client", err.Error())
		a.record("identity_client_rejected", map[string]any{"client_id": request.Form.Get("client_id")})
		return
	}
	requested := splitScope(request.Form.Get("scope"))
	audience := firstNonempty(request.Form.Get("resource"), request.Form.Get("audience"))
	var token string
	var claims Claims
	switch request.Form.Get("grant_type") {
	case "client_credentials":
		token, claims, err = a.issueClientCredentials(principal, audience, requested)
	case tokenExchangeGrant:
		a.mu.RLock()
		reject := a.faults.RejectExchange
		a.mu.RUnlock()
		if reject {
			err = errors.New("synthetic token exchange is currently rejected")
			break
		}
		token, claims, err = a.exchange(principal, request.Form.Get("subject_token"), audience, requested)
	default:
		a.writeOAuthError(writer, http.StatusBadRequest, "unsupported_grant_type", "grant_type is not supported")
		return
	}
	if err != nil {
		a.writeOAuthError(writer, http.StatusBadRequest, "invalid_grant", err.Error())
		a.record("identity_token_rejected", map[string]any{"principal": principal.ID, "grant_type": request.Form.Get("grant_type"), "reason": err.Error()})
		return
	}
	expiresIn := int64(claims.ExpiresAt.Sub(a.now().UTC()).Seconds())
	if expiresIn < 0 {
		expiresIn = 0
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"access_token": token, "issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"token_type": "Bearer", "expires_in": expiresIn, "scope": strings.Join(claims.Scopes, " "),
	})
}

func (a *Authority) authenticateClient(request *http.Request) (environment.IdentityPrincipal, error) {
	clientID, secret, basic := request.BasicAuth()
	if !basic {
		clientID, secret = request.Form.Get("client_id"), request.Form.Get("client_secret")
	}
	a.mu.RLock()
	principalID := a.clients[clientID]
	principal, exists := a.principals[principalID]
	a.mu.RUnlock()
	if !exists || principal.ClientSecret == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(principal.ClientSecret)) != 1 {
		return environment.IdentityPrincipal{}, errors.New("client authentication failed")
	}
	return principal, nil
}

func (a *Authority) issueClientCredentials(principal environment.IdentityPrincipal, audience string, requested []string) (string, Claims, error) {
	services, scopes := a.directAuthorization(principal.ID, audience, requested)
	if len(services) == 0 {
		return "", Claims{}, errors.New("principal has no matching active grant")
	}
	claims := Claims{Subject: principal.ID, Audience: a.audiencesForServices(services), Scopes: scopes, ClientID: principal.ClientID, PrincipalKind: principal.Kind}
	return a.sign(claims, principal.Claims)
}

func (a *Authority) exchange(agent environment.IdentityPrincipal, subjectToken, audience string, requested []string) (string, Claims, error) {
	if subjectToken == "" {
		return "", Claims{}, errors.New("subject_token is required")
	}
	subject, err := a.Verify(subjectToken)
	if err != nil {
		return "", Claims{}, fmt.Errorf("subject_token: %w", err)
	}
	user, exists := a.principals[subject.Subject]
	if !exists || user.Kind != "user" || subject.Actor != nil {
		return "", Claims{}, errors.New("subject_token must represent a user principal")
	}
	if !contains(subject.Audience, agent.ID) {
		return "", Claims{}, errors.New("subject_token was not issued for this agent")
	}
	services, scopes := a.delegatedAuthorization(agent.ID, user.ID, audience, requested)
	if len(services) == 0 {
		return "", Claims{}, errors.New("agent has no matching active delegation from this user")
	}
	claims := Claims{
		Subject: user.ID, Audience: a.audiencesForServices(services), Scopes: scopes,
		ClientID: agent.ClientID, PrincipalKind: user.Kind, Actor: &Actor{Subject: agent.ID},
	}
	return a.sign(claims, user.Claims)
}

func (a *Authority) IssueCallerToken(principalID, audience string, scopes []string) (string, Claims, error) {
	a.mu.RLock()
	principal, exists := a.principals[principalID]
	audiencePrincipal, audienceExists := a.principals[audience]
	a.mu.RUnlock()
	if !exists || principal.Kind != "user" {
		return "", Claims{}, fmt.Errorf("caller principal %q is not a user", principalID)
	}
	if !audienceExists || audiencePrincipal.Kind != "agent" {
		return "", Claims{}, errors.New("caller audience must identify an agent principal")
	}
	if len(scopes) > 0 && !validScopes(scopes) {
		return "", Claims{}, errors.New("caller scopes are invalid")
	}
	claims := Claims{Subject: principal.ID, Audience: []string{audience}, Scopes: uniqueSorted(scopes), PrincipalKind: principal.Kind}
	token, issued, err := a.sign(claims, principal.Claims)
	if err == nil {
		a.record("identity_caller_token_issued", tokenSemantics(issued, token))
	}
	return token, issued, err
}

func (a *Authority) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("token is not a compact JWT")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, errors.New("token header is invalid")
	}
	var header map[string]any
	if json.Unmarshal(headerBytes, &header) != nil || header["alg"] != "RS256" || header["kid"] != a.kid {
		return Claims{}, errors.New("token signing header is not accepted")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, errors.New("token signature is invalid")
	}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&a.key.PublicKey, crypto.SHA256, hash[:], signature) != nil {
		return Claims{}, errors.New("token signature is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("token payload is invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	var raw map[string]any
	if decoder.Decode(&raw) != nil {
		return Claims{}, errors.New("token claims are invalid")
	}
	claims, err := claimsFromMap(raw)
	if err != nil {
		return Claims{}, err
	}
	if claims.Issuer != strings.TrimSuffix(a.issuer.IssuerURL, "/") {
		return Claims{}, errors.New("token issuer is not accepted")
	}
	now := a.now().UTC()
	if !claims.ExpiresAt.After(now) {
		return Claims{}, errors.New("token is expired")
	}
	if claims.IssuedAt.After(now.Add(30 * time.Second)) {
		return Claims{}, errors.New("token was issued in the future")
	}
	return claims, nil
}

func (a *Authority) Authorize(serviceID string, policy environment.ServiceAuthorization, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/.well-known/oauth-protected-resource") {
			writeJSON(writer, http.StatusOK, map[string]any{
				"resource": policy.Audience, "authorization_servers": []string{strings.TrimSuffix(a.issuer.IssuerURL, "/")},
				"scopes_supported": policy.RequiredScopes, "bearer_methods_supported": []string{"header"},
			})
			return
		}
		claims, err := a.authorizeRequest(request, serviceID, policy)
		if err != nil {
			metadataURL := "http://" + request.Host + "/.well-known/oauth-protected-resource"
			writer.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q, scope=%q`, metadataURL, strings.Join(policy.RequiredScopes, " ")))
			a.writeOAuthError(writer, http.StatusUnauthorized, "invalid_token", err.Error())
			a.record("identity_authorization_denied", map[string]any{"service": serviceID, "method": request.Method, "path": request.URL.Path, "reason": err.Error()})
			return
		}
		a.record("identity_authorization_allowed", map[string]any{
			"service": serviceID, "method": request.Method, "path": request.URL.Path,
			"subject": claims.Subject, "actor": actorSubject(claims.Actor), "scopes": claims.Scopes, "token_fingerprint": digest.Bytes([]byte(bearerToken(request))),
		})
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), contextKey{}, claims)))
	})
}

func (a *Authority) authorizeRequest(request *http.Request, serviceID string, policy environment.ServiceAuthorization) (Claims, error) {
	token := bearerToken(request)
	if token == "" {
		return Claims{}, errors.New("bearer token is required")
	}
	claims, err := a.Verify(token)
	if err != nil {
		return Claims{}, err
	}
	if !contains(claims.Audience, policy.Audience) {
		return Claims{}, errors.New("token audience does not include this service")
	}
	if !containsAll(claims.Scopes, policy.RequiredScopes) {
		return Claims{}, errors.New("token does not contain the required scopes")
	}
	if claims.Actor == nil {
		if !a.directGrantActive(claims.Subject, serviceID, policy.RequiredScopes) {
			return Claims{}, errors.New("principal grant is inactive or insufficient")
		}
	} else {
		if !policy.AllowDelegated {
			return Claims{}, errors.New("delegated access is not accepted by this service")
		}
		if !a.delegationActive(claims.Actor.Subject, claims.Subject, serviceID, policy.RequiredScopes) {
			return Claims{}, errors.New("delegation is inactive or insufficient")
		}
	}
	return claims, nil
}

func (a *Authority) DeliveryEnvironment(delivery environment.IdentityDelivery) (map[string]string, error) {
	principal, exists := a.principals[delivery.Principal]
	if !exists || delivery.Issuer != a.issuer.ID {
		return nil, errors.New("identity delivery does not belong to this issuer")
	}
	issuer := strings.TrimSuffix(a.issuer.IssuerURL, "/")
	result := map[string]string{}
	switch delivery.Kind {
	case "oauth_client":
		result[delivery.ClientIDEnvironment] = principal.ClientID
		result[delivery.ClientSecretEnvironment] = principal.ClientSecret
		result[delivery.TokenURLEnvironment] = issuer + "/token"
	case "projected_jwt":
		result[delivery.EnvironmentVariable] = delivery.Path
	case "aws_container_credentials":
		// AWS SDKs intentionally restrict plain-HTTP container credential URIs
		// to loopback and ECS metadata addresses. Use the issuer's loopback
		// listener rather than its synthetic discovery hostname.
		result[delivery.EndpointEnvironment] = "http://" + a.issuer.Listen + "/aws/container-credentials/" + url.PathEscape(principal.ID)
		if delivery.AuthorizationTokenEnvironment != "" {
			result[delivery.AuthorizationTokenEnvironment] = a.deliveryAuthorizationToken(delivery.ID)
		}
	default:
		return nil, fmt.Errorf("unsupported identity delivery kind %q", delivery.Kind)
	}
	return result, nil
}

func (a *Authority) ProjectedToken(delivery environment.IdentityDelivery) (string, Claims, error) {
	if delivery.Kind != "projected_jwt" || delivery.Issuer != a.issuer.ID {
		return "", Claims{}, errors.New("delivery is not a projected token for this issuer")
	}
	principal, exists := a.principals[delivery.Principal]
	if !exists {
		return "", Claims{}, errors.New("projected token principal is unknown")
	}
	claims := Claims{Subject: principal.ID, Audience: []string{delivery.Audience}, PrincipalKind: principal.Kind}
	token, issued, err := a.sign(claims, principal.Claims)
	if err == nil {
		a.record("identity_projected_token_issued", tokenSemantics(issued, token))
	}
	return token, issued, err
}

func (a *Authority) Snapshot() map[string]map[string]json.RawMessage {
	a.mu.RLock()
	defer a.mu.RUnlock()
	result := map[string]map[string]json.RawMessage{"grants": {}, "delegations": {}, "faults": {}}
	for id, grant := range a.grants {
		result["grants"][id] = mustJSON(grant)
	}
	for id, delegation := range a.delegations {
		result["delegations"][id] = mustJSON(delegation)
	}
	result["faults"]["issuer"] = mustJSON(a.faults)
	return result
}

func (a *Authority) ValidateApply(operation statejournal.Operation) error {
	if operation.Service != a.issuer.ID || operation.Kind != "service_put" {
		return errors.New("identity state supports service_put only")
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	switch operation.Collection {
	case "grants":
		existing, exists := a.grants[operation.Key]
		var updated GrantState
		if !exists || json.Unmarshal(operation.Value, &updated) != nil || updated.ID != existing.ID || updated.Principal != existing.Principal || updated.Service != existing.Service || !validScopes(updated.Scopes) {
			return errors.New("identity grant mutation is invalid or changes immutable fields")
		}
	case "delegations":
		existing, exists := a.delegations[operation.Key]
		var updated DelegationState
		if !exists || json.Unmarshal(operation.Value, &updated) != nil || updated.ID != existing.ID || updated.Agent != existing.Agent || updated.User != existing.User || updated.Service != existing.Service || !validScopes(updated.Scopes) {
			return errors.New("identity delegation mutation is invalid or changes immutable fields")
		}
	case "faults":
		var updated FaultState
		if operation.Key != "issuer" || json.Unmarshal(operation.Value, &updated) != nil {
			return errors.New("identity fault mutation is invalid")
		}
	default:
		return fmt.Errorf("unknown identity state collection %q", operation.Collection)
	}
	return nil
}

func (a *Authority) Apply(operation statejournal.Operation) error {
	if err := a.ValidateApply(operation); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch operation.Collection {
	case "grants":
		var value GrantState
		_ = json.Unmarshal(operation.Value, &value)
		a.grants[operation.Key] = value
	case "delegations":
		var value DelegationState
		_ = json.Unmarshal(operation.Value, &value)
		a.delegations[operation.Key] = value
	case "faults":
		_ = json.Unmarshal(operation.Value, &a.faults)
	}
	return nil
}

func (a *Authority) awsCredentials(writer http.ResponseWriter, request *http.Request) {
	principalID, err := url.PathUnescape(strings.TrimPrefix(request.URL.Path, "/aws/container-credentials/"))
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	principal, exists := a.principals[principalID]
	if !exists || principal.Kind == "user" {
		http.NotFound(writer, request)
		return
	}
	var deliveryID string
	for id, delivery := range a.deliveries {
		if delivery.Kind == "aws_container_credentials" && delivery.Principal == principalID {
			deliveryID = id
			if delivery.AuthorizationTokenEnvironment != "" && subtle.ConstantTimeCompare([]byte(request.Header.Get("Authorization")), []byte(a.deliveryAuthorizationToken(id))) != 1 {
				a.writeOAuthError(writer, http.StatusUnauthorized, "invalid_token", "container credential authorization token is invalid")
				return
			}
			break
		}
	}
	if deliveryID == "" {
		http.NotFound(writer, request)
		return
	}
	seed := sha256.Sum256([]byte(a.issuer.ID + ":" + principalID))
	accessKey := "ASIA" + strings.ToUpper(hex.EncodeToString(seed[:8]))
	secret := base64.RawURLEncoding.EncodeToString(seed[:])
	sessionToken, claims, err := a.sign(Claims{Subject: principal.ID, Audience: []string{"aws"}, PrincipalKind: principal.Kind}, principal.Claims)
	if err != nil {
		a.writeOAuthError(writer, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"AccessKeyId": accessKey, "SecretAccessKey": secret, "Token": sessionToken,
		"Expiration": claims.ExpiresAt.Format(time.RFC3339), "Code": "Success", "Type": "AWS-HMAC",
	})
	a.record("identity_aws_credentials_issued", tokenSemantics(claims, sessionToken))
}

func (a *Authority) sign(claims Claims, custom map[string]any) (string, Claims, error) {
	now := a.now().UTC()
	expiration := now.Add(a.ttl())
	a.mu.RLock()
	issueExpired := a.faults.IssueExpired
	a.mu.RUnlock()
	if issueExpired {
		expiration = now.Add(-time.Second)
	}
	claims.Issuer, claims.IssuedAt, claims.ExpiresAt = strings.TrimSuffix(a.issuer.IssuerURL, "/"), now, expiration
	randomID := make([]byte, 16)
	if _, err := rand.Read(randomID); err != nil {
		return "", Claims{}, err
	}
	claims.TokenID = base64.RawURLEncoding.EncodeToString(randomID)
	payload := map[string]any{}
	for name, value := range custom {
		payload[name] = value
	}
	payload["iss"], payload["sub"] = claims.Issuer, claims.Subject
	if len(claims.Audience) == 1 {
		payload["aud"] = claims.Audience[0]
	} else {
		payload["aud"] = claims.Audience
	}
	payload["iat"], payload["exp"], payload["jti"] = claims.IssuedAt.Unix(), claims.ExpiresAt.Unix(), claims.TokenID
	payload["interceptor_principal_kind"] = claims.PrincipalKind
	if len(claims.Scopes) > 0 {
		payload["scope"] = strings.Join(uniqueSorted(claims.Scopes), " ")
	}
	if claims.ClientID != "" {
		payload["client_id"], payload["azp"] = claims.ClientID, claims.ClientID
	}
	if claims.Actor != nil {
		payload["act"] = map[string]any{"sub": claims.Actor.Subject}
	}
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": a.kid})
	body, err := json.Marshal(payload)
	if err != nil {
		return "", Claims{}, err
	}
	encoded := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	hash := sha256.Sum256([]byte(encoded))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, hash[:])
	if err != nil {
		return "", Claims{}, err
	}
	token := encoded + "." + base64.RawURLEncoding.EncodeToString(signature)
	claims.Raw = payload
	a.record("identity_token_issued", tokenSemantics(claims, token))
	return token, claims, nil
}

func claimsFromMap(raw map[string]any) (Claims, error) {
	issuer, _ := raw["iss"].(string)
	subject, _ := raw["sub"].(string)
	if issuer == "" || subject == "" {
		return Claims{}, errors.New("token is missing issuer or subject")
	}
	audience := []string{}
	switch value := raw["aud"].(type) {
	case string:
		audience = []string{value}
	case []any:
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				return Claims{}, errors.New("token audience is invalid")
			}
			audience = append(audience, text)
		}
	}
	expiration, expOK := numberInt64(raw["exp"])
	issuedAt, iatOK := numberInt64(raw["iat"])
	if len(audience) == 0 || !expOK || !iatOK {
		return Claims{}, errors.New("token is missing audience or timestamps")
	}
	claims := Claims{
		Issuer: issuer, Subject: subject, Audience: audience, ExpiresAt: time.Unix(expiration, 0).UTC(), IssuedAt: time.Unix(issuedAt, 0).UTC(),
		Scopes: splitScope(stringValue(raw["scope"])), ClientID: stringValue(raw["client_id"]), PrincipalKind: stringValue(raw["interceptor_principal_kind"]),
		TokenID: stringValue(raw["jti"]), Raw: raw,
	}
	if actor, ok := raw["act"].(map[string]any); ok && stringValue(actor["sub"]) != "" {
		claims.Actor = &Actor{Subject: stringValue(actor["sub"])}
	}
	return claims, nil
}

func (a *Authority) directAuthorization(principal, audience string, requested []string) ([]string, []string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	services, scopes := []string{}, []string{}
	for _, grant := range a.grants {
		if !grant.Active || grant.Principal != principal || audience != "" && a.serviceAudience(grant.Service) != audience || len(requested) > 0 && !containsAll(grant.Scopes, requested) {
			continue
		}
		services = append(services, grant.Service)
		if len(requested) == 0 {
			scopes = append(scopes, grant.Scopes...)
		} else {
			scopes = append(scopes, requested...)
		}
	}
	return uniqueSorted(services), uniqueSorted(scopes)
}

func (a *Authority) delegatedAuthorization(agent, user, audience string, requested []string) ([]string, []string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	services, scopes := []string{}, []string{}
	for _, delegation := range a.delegations {
		if !delegation.Active || delegation.Agent != agent || delegation.User != user || audience != "" && a.serviceAudience(delegation.Service) != audience || len(requested) > 0 && !containsAll(delegation.Scopes, requested) {
			continue
		}
		services = append(services, delegation.Service)
		if len(requested) == 0 {
			scopes = append(scopes, delegation.Scopes...)
		} else {
			scopes = append(scopes, requested...)
		}
	}
	return uniqueSorted(services), uniqueSorted(scopes)
}

func (a *Authority) directGrantActive(principal, service string, scopes []string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, grant := range a.grants {
		if grant.Active && grant.Principal == principal && grant.Service == service && containsAll(grant.Scopes, scopes) {
			return true
		}
	}
	return false
}

func (a *Authority) delegationActive(agent, user, service string, scopes []string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, delegation := range a.delegations {
		if delegation.Active && delegation.Agent == agent && delegation.User == user && delegation.Service == service && containsAll(delegation.Scopes, scopes) {
			return true
		}
	}
	return false
}

func (a *Authority) serviceAudience(service string) string {
	for audience, id := range a.audiences {
		if id == service {
			return audience
		}
	}
	return service
}

func (a *Authority) audiencesForServices(services []string) []string {
	result := make([]string, 0, len(services))
	for _, service := range services {
		result = append(result, a.serviceAudience(service))
	}
	return uniqueSorted(result)
}

func (a *Authority) allScopes() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	result := []string{}
	for _, grant := range a.grants {
		result = append(result, grant.Scopes...)
	}
	for _, delegation := range a.delegations {
		result = append(result, delegation.Scopes...)
	}
	return uniqueSorted(result)
}

func (a *Authority) ttl() time.Duration {
	seconds := a.issuer.AccessTokenTTLSeconds
	if seconds == 0 {
		seconds = 900
	}
	return time.Duration(seconds) * time.Second
}

func (a *Authority) deliveryAuthorizationToken(deliveryID string) string {
	value := sha256.Sum256([]byte(a.issuer.ID + ":" + deliveryID + ":container-credentials"))
	return "Bearer interceptor-" + base64.RawURLEncoding.EncodeToString(value[:])
}

func (a *Authority) record(kind string, semantic any) {
	if a.recorder == nil {
		return
	}
	_, _ = a.recorder.Append(recording.Draft{Channel: recording.Evaluation, Layer: "identity", Kind: kind, Provenance: recording.EnvironmentSealed, Semantic: semantic})
}

func (a *Authority) writeOAuthError(writer http.ResponseWriter, status int, code, description string) {
	writeJSON(writer, status, map[string]any{"error": code, "error_description": description})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func tokenSemantics(claims Claims, token string) map[string]any {
	return map[string]any{
		"subject": claims.Subject, "actor": actorSubject(claims.Actor), "audience": claims.Audience,
		"scopes": claims.Scopes, "expires_at": claims.ExpiresAt, "token_fingerprint": digest.Bytes([]byte(token)),
	}
}

func actorSubject(actor *Actor) string {
	if actor == nil {
		return ""
	}
	return actor.Subject
}

func bearerToken(request *http.Request) string {
	value := strings.TrimSpace(request.Header.Get("Authorization"))
	if len(value) < 8 || !strings.EqualFold(value[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(value[7:])
}

func splitScope(value string) []string { return uniqueSorted(strings.Fields(value)) }
func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
func containsAll(values, wanted []string) bool {
	for _, item := range wanted {
		if !contains(values, item) {
			return false
		}
	}
	return true
}
func uniqueSorted(values []string) []string {
	seen := map[string]struct{}{}
	result := []string{}
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; !exists {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
func cloneStrings(values []string) []string { return append([]string(nil), values...) }
func validScopes(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if strings.TrimSpace(value) != value || value == "" || strings.ContainsAny(value, " \t\r\n") {
			return false
		}
	}
	return true
}
func mustJSON(value any) json.RawMessage { encoded, _ := json.Marshal(value); return encoded }
func stringValue(value any) string       { text, _ := value.(string); return text }
func numberInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil
	case float64:
		return int64(number), true
	default:
		return 0, false
	}
}
func bigEndian(value int) []byte {
	if value == 0 {
		return []byte{0}
	}
	result := []byte{}
	for value > 0 {
		result = append([]byte{byte(value)}, result...)
		value >>= 8
	}
	return result
}
