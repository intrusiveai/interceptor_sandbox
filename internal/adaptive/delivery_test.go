package adaptive

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/delivery"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

func TestDeliveryCapabilityVersionDigestBoundsAndIsolation(t *testing.T) {
	contract := &delivery.Contract{Description: "Task", Input: delivery.Body{MediaType: "application/json", Encoding: "json", MaxBytes: 4096, Basis: "source-derived", Schema: map[string]any{"type": "object"}},
		Output: delivery.Body{MediaType: "application/json", Encoding: "json", MaxBytes: 4096, Basis: "unknown"}}
	bp := &environment.Blueprint{APIVersion: environment.DeliveryAPIVersion,
		Application: environment.Application{Interface: environment.ApplicationInterface{Operations: []environment.ApplicationOperation{{ID: "invoke", Delivery: contract}}}},
		Limits:      environment.Limits{MaxBlobBytes: 1024}}
	first, err := BuildCapabilityManifest(bp, "sha256:environment", "sha256:application")
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildCapabilityManifest(bp, "sha256:environment", "sha256:application")
	if err != nil {
		t.Fatal(err)
	}
	if first.APIVersion != DeliveryCapabilityManifestVersion || first.DeliverySchemaProfile != delivery.SchemaProfile || first.Digest != second.Digest {
		t.Fatal("wrong version or nondeterministic digest")
	}
	if first.Operations[0].MaximumInputBytes != 1024 || first.Operations[0].DeliveryStatus != "described" {
		t.Fatal("effective bound/status incorrect")
	}
	wantDigest := first.Digest
	first.Digest = ""
	preimage, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if digest.Bytes(preimage) != wantDigest {
		t.Fatal("wrong native digest recipe")
	}
	contract.Input.Schema["type"] = "string"
	if first.Operations[0].Delivery.Input.Schema["type"] != "object" {
		t.Fatal("public contract shares mutable source maps")
	}
	bp.Application.Interface.Operations[0].Delivery = nil
	missing, err := BuildCapabilityManifest(bp, "sha256:environment", "sha256:application")
	if err != nil {
		t.Fatal(err)
	}
	if missing.Operations[0].DeliveryStatus != "missing-input-contract" || missing.Operations[0].Delivery != nil {
		t.Fatal("invented missing contract")
	}
	bp.APIVersion = environment.APIVersion
	legacy, err := BuildCapabilityManifest(bp, "sha256:environment", "sha256:application")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(legacy)
	if legacy.APIVersion != CapabilityManifestVersion || strings.Contains(string(data), `"delivery":`) || strings.Contains(string(data), `"delivery_status":`) || strings.Contains(string(data), "maximum_input_bytes") {
		t.Fatal("legacy shape changed")
	}
}

func TestCapabilityBuilderRejectsUnresolvedAssets(t *testing.T) {
	bp := &environment.Blueprint{APIVersion: environment.DeliveryAPIVersion,
		Application: environment.Application{Interface: environment.ApplicationInterface{Operations: []environment.ApplicationOperation{{ID: "invoke", DeliveryAsset: "contract.json"}}}}}
	if _, err := BuildCapabilityManifest(bp, "x", "y"); err == nil {
		t.Fatal("unresolved operation accepted")
	}
	bp.Application.Interface.Operations = nil
	bp.Services = []environment.Service{{ID: "mcp", Kind: "mcp_http", ExportContracts: true}}
	if _, err := BuildCapabilityManifest(bp, "x", "y"); err == nil {
		t.Fatal("unresolved service accepted")
	}
}
