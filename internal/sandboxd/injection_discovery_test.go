package sandboxd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/contracttest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/delivery"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/identity"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/services"
)

func discoveryCampaign(t *testing.T, s *Server) (*adaptive.Gateway, func(string, any, *adaptive.AttemptContext) adaptive.GatewayResponse) {
	t.Helper()
	g := nativeGateway(t, s)
	serial := 0
	call := func(op string, body any, attempt *adaptive.AttemptContext) adaptive.GatewayResponse {
		serial++
		return nativeCall(t, g, s, fmt.Sprintf("discovery-%d", serial), op, body, attempt)
	}
	result := call("session.owner", adaptive.OwnerRequest{Action: "bind", SessionID: s.metadata.ID, CampaignID: "campaign", WorkerInstanceID: "worker"}, nil)
	if result.Status != 200 {
		t.Fatal(result)
	}
	return g, call
}

func discoveryAttempt(t *testing.T, call func(string, any, *adaptive.AttemptContext) adaptive.GatewayResponse, id, payload string, index uint64) adaptive.AttemptContext {
	t.Helper()
	bytes := []byte(payload)
	descriptor := adaptive.ArtifactDescriptor{Digest: digest.Bytes(bytes), SizeBytes: int64(len(bytes)), MediaType: "application/json", Canonicalization: "raw"}
	if r := call("artifact.register", artifactRegistration{Descriptor: descriptor, Content: bytes}, nil); r.Status != 201 {
		t.Fatal(r)
	}
	attempt := adaptive.AttemptContext{APIVersion: adaptive.AttemptContextVersion, CampaignID: "campaign", ThreadID: "thread", AttemptID: id, AttemptIndex: index, Generation: 1, Payload: descriptor, Generator: adaptive.Generator{Kind: "custom-harness"}, FeedbackProfile: "black-box", CreatedAt: time.Now().UTC()}
	if r := call("attempt.register", attempt, &attempt); r.Status != 201 {
		t.Fatal(r)
	}
	return attempt
}

func discoverServices(t *testing.T, call func(string, any, *adaptive.AttemptContext) adaptive.GatewayResponse) []adaptive.ServiceCapability {
	t.Helper()
	result := call("capabilities.read", struct{}{}, nil)
	if result.Status != 200 {
		t.Fatalf("discovery: %+v", result)
	}
	validator, err := contracttest.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.Validate("#/components/schemas/adaptive.CapabilityManifest", result.Body); err != nil {
		t.Fatal(err)
	}
	var manifest adaptive.CapabilityManifest
	if err := json.Unmarshal(result.Body, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.Services
}

func TestDiscoveredAttackerTargetsThroughNativeAPI(t *testing.T) {
	for _, transport := range []string{"unix", "spool"} {
		for _, variant := range []string{"restricted", "any-method", "any-path", "unrestricted", "json"} {
			t.Run(transport+"/"+variant, func(t *testing.T) {
				s, _ := nativeOperationServer(t, transport)
				options := map[string]any{}
				if variant == "restricted" || variant == "any-path" {
					options["methods"] = []string{"POST"}
				}
				if variant == "restricted" || variant == "any-method" {
					options["paths"] = []string{"/collect"}
				}
				s.blueprint.AttackerServices = []environment.AttackerService{{ID: "sink", Kind: "http_sink", Response: environment.SinkResponse{Mode: "dynamic", Status: 200, Body: "idle"}, Options: options}, {ID: "static", Kind: "http_sink", Response: environment.SinkResponse{Status: 200}}}
				if variant == "json" {
					s.blueprint.AttackerServices[0].Response.Body = ""
					s.blueprint.AttackerServices[0].Response.JSON = map[string]any{"task": "idle"}
				}
				_, call := discoveryCampaign(t, s)
				svc := discoverServices(t, call)[0]
				ep := svc.Endpoints[0]
				method, path := "GET", "/instructions"
				if len(ep.Methods) > 0 {
					method = ep.Methods[0]
				}
				if len(ep.Paths) > 0 {
					path = ep.Paths[0]
				}
				if ep.Kind != "http-route-set" {
					t.Fatal(ep)
				}
				attempt := discoveryAttempt(t, call, "attempt", `"injected task"`, 1)
				definition := injection.Definition{ID: "response", Surface: "service_response", Mode: "simulation", Scope: "once", Enabled: true, AttemptID: attempt.AttemptID, PayloadDigest: attempt.Payload.Digest, Payload: json.RawMessage(`"injected task"`), Selector: injection.Selector{Service: svc.ID, Method: method, Path: path, CallOrdinal: "first"}, Placement: injection.Placement{Operation: "replace"}}
				if r := call("injection.arm", injectionArm{Definition: definition}, &attempt); r.Status != 201 {
					t.Fatal(r)
				}
				methods, paths, err := services.SinkAllowlists(options)
				if err != nil {
					t.Fatal(err)
				}
				sink, err := services.NewSinkHandler(svc.ID, methods, paths, services.SinkResponse{Status: 200, Body: s.blueprint.AttackerServices[0].Response.Body, JSON: s.blueprint.AttackerServices[0].Response.JSON}, 1024, s.blobs, s.recorder)
				if err != nil {
					t.Fatal(err)
				}
				endpoint := httptest.NewServer(services.WithResponseInjection(svc.ID, "http_sink", sink, s.injections, s.recorder))
				defer endpoint.Close()
				request, err := http.NewRequest(method, endpoint.URL+path, strings.NewReader("captured"))
				if err != nil {
					t.Fatal(err)
				}
				response, err := endpoint.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode != 200 || !bytes.Contains(body, []byte("injected task")) || len(sink.Observations()) != 1 {
					t.Fatalf("response/capture: %d %s", response.StatusCode, body)
				}
				for _, bad := range []string{"static", "path", "method", "head"} {
					invalid := definition
					invalid.ID = "bad-" + bad
					switch bad {
					case "head":
						invalid.Selector.Method = "HEAD"
					case "static":
						invalid.Selector.Service = "static"
					case "path":
						if len(paths) == 0 {
							continue
						}
						invalid.Selector.Path = "/not-allowed"
					case "method":
						if len(methods) == 0 {
							continue
						}
						invalid.Selector.Method = "DELETE"
					}
					r := call("injection.arm", injectionArm{Definition: invalid}, &attempt)
					if r.Status != 400 || !bytes.Contains(r.Body, []byte("invalid_injection")) {
						t.Fatalf("bad %s: %+v", bad, r)
					}
				}
			})
		}
	}
}

func TestDiscoveredIdentityTargetsThroughNativeAPI(t *testing.T) {
	for _, transport := range []string{"unix", "spool"} {
		t.Run(transport, func(t *testing.T) {
			s, _ := nativeOperationServer(t, transport)
			issuer := environment.IdentityIssuer{ID: "identity", Kind: "oidc", IssuerURL: "http://identity.test", AccessTokenTTLSeconds: 600}
			config := environment.Identity{Issuers: []environment.IdentityIssuer{issuer}, Principals: []environment.IdentityPrincipal{{ID: "agent", Kind: "agent", Issuer: issuer.ID, ClientID: "client", ClientSecret: "synthetic-secret"}, {ID: "alice", Kind: "user", Issuer: issuer.ID}}, Grants: []environment.IdentityGrant{{ID: "grant", Principal: "agent", Service: "tickets", Scopes: []string{"read"}}}, Delegations: []environment.IdentityDelegation{{ID: "delegation", Agent: "agent", User: "alice", Service: "tickets", Scopes: []string{"read"}}}}
			s.blueprint.Identity = config
			s.blueprint.AttackerServices = nil
			policy := environment.ServiceAuthorization{Issuer: issuer.ID, Audience: "tickets", RequiredScopes: []string{"read"}, AllowDelegated: true}
			authority, err := identity.NewAuthority(issuer, config.Principals, config.Grants, config.Delegations, nil, []environment.Service{{ID: "tickets", Authorization: &policy}}, s.recorder)
			if err != nil {
				t.Fatal(err)
			}
			s.services = map[string]services.ManagedHandler{issuer.ID: authority}
			_, call := discoveryCampaign(t, s)
			svc := discoverServices(t, call)[0]
			state := authority.Snapshot()
			for _, ep := range svc.Endpoints {
				for _, key := range ep.Keys {
					if _, ok := state[ep.Collection][key]; !ok {
						t.Fatal("advertised nonexistent record")
					}
					if err := delivery.ValidateBytes(ep.Delivery.Output, state[ep.Collection][key]); err != nil {
						t.Fatal(err)
					}
				}
			}
			tokenRequest := func() int {
				form := url.Values{"grant_type": {"client_credentials"}, "client_id": {"client"}, "client_secret": {"synthetic-secret"}, "resource": {"tickets"}, "scope": {"read"}}
				r := httptest.NewRequest("POST", "http://identity.test/token", strings.NewReader(form.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				w := httptest.NewRecorder()
				authority.ServeHTTP(w, r)
				return w.Code
			}
			if code := tokenRequest(); code != 200 {
				t.Fatalf("initial issuance %d", code)
			}
			attempt := discoveryAttempt(t, call, "attempt", `false`, 1)
			for _, ep := range svc.Endpoints {
				if ep.Collection == "faults" {
					continue
				}
				if !slices.Contains(ep.MutableFields, "active") {
					t.Fatal(ep)
				}
				d := injection.Definition{ID: ep.ID, Surface: "environment_state", Mode: "fidelity", Scope: "standing", Enabled: true, AttemptID: attempt.AttemptID, PayloadDigest: attempt.Payload.Digest, Payload: json.RawMessage(`false`), Selector: injection.Selector{Service: svc.ID, Collection: ep.Collection, Key: ep.Keys[0]}, Placement: injection.Placement{Operation: "replace", Pointer: "/active"}}
				if r := call("injection.arm", injectionArm{Definition: d}, &attempt); r.Status != 201 {
					t.Fatal(r)
				}
				if !bytes.Contains(authority.Snapshot()[ep.Collection][ep.Keys[0]], []byte(`"active":false`)) {
					t.Fatal("mutation not applied")
				}
				for _, bad := range []string{"immutable", "missing", "wrong-collection"} {
					invalid := d
					invalid.ID = ep.ID + "-" + bad
					switch bad {
					case "immutable":
						invalid.Placement.Pointer = "/id"
					case "missing":
						invalid.Selector.Key = "missing"
					case "wrong-collection":
						invalid.Selector.Collection = "unknown"
					}
					if r := call("injection.arm", injectionArm{Definition: invalid}, &attempt); r.Status != 400 || !bytes.Contains(r.Body, []byte("invalid_injection")) {
						t.Fatal(bad, r)
					}
				}
			}
			if tokenRequest() == 200 {
				t.Fatal("revoked grant still issued token")
			}
			fault := discoveryAttempt(t, call, "fault", `true`, 2)
			for _, ep := range svc.Endpoints {
				if ep.Collection != "faults" {
					continue
				}
				for _, field := range ep.MutableFields {
					d := injection.Definition{ID: field, Surface: "environment_state", Mode: "fidelity", Scope: "standing", Enabled: true, AttemptID: fault.AttemptID, PayloadDigest: fault.Payload.Digest, Payload: json.RawMessage(`true`), Selector: injection.Selector{Service: svc.ID, Collection: ep.Collection, Key: ep.Keys[0]}, Placement: injection.Placement{Operation: "replace", Pointer: "/" + field}}
					if r := call("injection.arm", injectionArm{Definition: d}, &fault); r.Status != 201 {
						t.Fatal(r)
					}
				}
			}
			if code := tokenRequest(); code != 503 {
				t.Fatalf("issuer fault did not affect application request: %d", code)
			}
		})
	}
}
