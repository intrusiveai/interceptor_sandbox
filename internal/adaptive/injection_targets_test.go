package adaptive

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/delivery"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

func TestInjectionTargetsAreDeterministicAndSecretFree(t *testing.T) {
	bp := &environment.Blueprint{AttackerServices: []environment.AttackerService{
		{ID: "dynamic", Kind: "http_sink", Response: environment.SinkResponse{Mode: "dynamic", Status: 200, JSON: map[string]any{"secret": "hidden-response"}}, Options: map[string]any{"methods": []any{"post", "GET", "POST"}, "paths": []string{"/b", "/a"}}},
		{ID: "head", Kind: "http_sink", Response: environment.SinkResponse{Mode: "dynamic", Status: 200}, Options: map[string]any{"methods": []string{"HEAD"}}},
		{ID: "static", Kind: "http_sink", Response: environment.SinkResponse{Status: 200}},
	}, Identity: environment.Identity{Issuers: []environment.IdentityIssuer{{ID: "idp", Kind: "oidc"}, {ID: "other", Kind: "oidc"}}, Principals: []environment.IdentityPrincipal{{ID: "agent", Issuer: "idp", ClientSecret: "hidden-secret", Claims: map[string]any{"secret": "hidden-claim"}}, {ID: "other-agent", Issuer: "other"}}, Grants: []environment.IdentityGrant{{ID: "grant", Principal: "agent", Scopes: []string{"hidden-scope"}}, {ID: "other-grant", Principal: "other-agent"}}, Delegations: []environment.IdentityDelegation{{ID: "delegation", Agent: "agent"}}}}
	for _, version := range []string{environment.APIVersion, environment.DeliveryAPIVersion} {
		bp.APIVersion = version
		manifest, err := BuildCapabilityManifest(bp, "environment", "application")
		if err != nil {
			t.Fatal(err)
		}
		again, err := BuildCapabilityManifest(bp, "environment", "application")
		if err != nil || manifest.Digest != again.Digest {
			t.Fatal("nondeterministic manifest", err)
		}
		raw, _ := json.Marshal(manifest)
		for _, secret := range []string{"hidden-response", "hidden-secret", "hidden-claim", "hidden-scope"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatal("leaked", secret)
			}
		}
		for _, service := range manifest.Services {
			if service.ID == "static" || service.ID == "head" {
				if len(service.Endpoints) != 0 || len(service.InjectionSurfaces) != 0 {
					t.Fatal("static sink advertised injection")
				}
				continue
			}
			for _, ep := range service.Endpoints {
				if err := delivery.Validate(ep.Delivery); err != nil {
					t.Fatal(service.ID, ep.ID, err)
				}
			}
			if service.ID == "dynamic" {
				ep := service.Endpoints[0]
				if ep.Kind != "http-route-set" || len(ep.Methods) != 2 || ep.Methods[0] != "GET" || ep.Paths[0] != "/a" || ep.Delivery.Output.Encoding != "json" {
					t.Fatal(ep)
				}
			}
			if service.ID == "other" {
				for _, ep := range service.Endpoints {
					for _, key := range ep.Keys {
						if key == "grant" || key == "delegation" {
							t.Fatal("cross-issuer target")
						}
					}
				}
			}
		}
		// Returned slices/maps cannot change the blueprint or later manifests.
		manifest.Services[0].Endpoints[0].Methods[0] = "DELETE"
		manifest.Services[0].Endpoints[0].Delivery.Output.Schema["type"] = "string"
		fresh, err := BuildCapabilityManifest(bp, "environment", "application")
		if err != nil || fresh.Digest != again.Digest {
			t.Fatal("mutable source alias", err)
		}
	}
}
