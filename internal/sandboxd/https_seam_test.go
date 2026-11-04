package sandboxd

import (
	"encoding/json"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
)

func TestHTTPSSeamsExposeNoInjectionOrHostPolicy(t *testing.T) {
	blueprint := &environment.Blueprint{Network: environment.NetworkSimulation{HTTPSSeams: []environment.HTTPSSeam{{ID: "tracing", Hostname: "private.example"}}}}
	blueprint.Limits.ApplyDefaults()
	blueprint.Adaptive.ApplyDefaults()
	s := &Server{blueprint: blueprint}
	def := injection.Definition{ID: "inject", Surface: "service_response", Mode: "simulation", Scope: "once", Enabled: true, Selector: injection.Selector{Service: "tracing", CallOrdinal: "first"}, Placement: injection.Placement{Operation: "replace"}, Payload: json.RawMessage(`"payload"`)}
	if err := injection.Validate(def); err != nil {
		t.Fatal(err)
	}
	if err := s.validateInjectionTarget(def); err == nil {
		t.Fatal("seam admitted to response injection")
	}
	manifest, err := adaptive.BuildCapabilityManifest(blueprint, "env", "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Network.HTTPSSeams) != 1 || manifest.Network.HTTPSSeams[0].ID != "tracing" || len(manifest.Services) != 0 {
		t.Fatal("seam projected as simulated service")
	}
	for _, surface := range manifest.InjectionSurfaces {
		if surface == "service_response" {
			t.Fatal("seam introduced response injection")
		}
	}
}
