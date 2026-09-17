package environment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/delivery"
)

func contractBlueprint(t *testing.T) (string, []byte) {
	t.Helper()
	dir := testBlueprint(t, "")
	path := filepath.Join(dir, BlueprintFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(data), APIVersion, DeliveryAPIVersion, 1)
	body = strings.Replace(body, "- {id: invoke, method: POST, path: /invoke}", "- {id: invoke, method: POST, path: /invoke, delivery_asset: assets/delivery.json}", 1)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	contract := delivery.Contract{Description: "Submit a task", Input: delivery.Body{MediaType: "application/json", Encoding: "json", MaxBytes: 4096, Basis: "source-derived", Schema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []any{"query"}}},
		Output: delivery.Body{MediaType: "application/json", Encoding: "json", MaxBytes: 4096, Basis: "unknown"}}
	encoded, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets/delivery.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, encoded
}

func TestDeliveryAssetIsResolvedSealedAndNeverGuessed(t *testing.T) {
	dir, data := contractBlueprint(t)
	lock, report, err := Seal(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	op := report.Blueprint.Application.Interface.Operations[0]
	if op.ResolvedDelivery == nil || op.ResolvedDelivery.Input.Schema == nil || op.ResolvedDelivery.Output.Schema != nil {
		t.Fatal("incorrect resolved contract")
	}
	encoded, _ := json.Marshal(report.Blueprint)
	if strings.Contains(string(encoded), "Submit a task") {
		t.Fatal("derived contract entered Blueprint hash preimage")
	}
	_, verified, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Blueprint.Application.Interface.Operations[0].ResolvedDelivery == nil {
		t.Fatal("verify did not resolve contract")
	}
	mutated := strings.Replace(string(data), "Submit a task", "Submit a reviewed task", 1)
	if err := os.WriteFile(filepath.Join(dir, "assets/delivery.json"), []byte(mutated), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(dir); err == nil {
		t.Fatal("changed contract passed seal verification")
	}
	next, _, err := Seal(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if lock.EnvironmentDigest == next.EnvironmentDigest {
		t.Fatal("contract not bound by seal")
	}
}

func TestDeliveryAssetStrictness(t *testing.T) {
	for _, mode := range []string{"unknown-field", "duplicate", "external-ref", "legacy", "symlink", "traversal"} {
		t.Run(mode, func(t *testing.T) {
			dir, data := contractBlueprint(t)
			asset := filepath.Join(dir, "assets/delivery.json")
			switch mode {
			case "unknown-field":
				data = []byte(strings.Replace(string(data), `"description":`, `"unexpected":true,"description":`, 1))
			case "duplicate":
				data = []byte(strings.Replace(string(data), `"description":`, `"description":"duplicate","description":`, 1))
			case "external-ref":
				data = []byte(strings.Replace(string(data), `"schema":{`, `"schema":{"$ref":"https://example.invalid/schema",`, 1))
			case "legacy", "traversal", "symlink":
				path := filepath.Join(dir, BlueprintFile)
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "legacy" {
					b = []byte(strings.Replace(string(b), DeliveryAPIVersion, APIVersion, 1))
				}
				if mode == "traversal" {
					b = []byte(strings.Replace(string(b), "assets/delivery.json", "../outside.json", 1))
				}
				if mode == "symlink" {
					if err := os.Symlink("delivery.json", filepath.Join(dir, "assets/link.json")); err != nil {
						t.Fatal(err)
					}
					b = []byte(strings.Replace(string(b), "assets/delivery.json", "assets/link.json", 1))
				}
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(asset, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Validate(dir); err == nil {
				t.Fatalf("accepted %s", mode)
			}
		})
	}
}

func TestMissingContractIsAnExplicitPreparationGap(t *testing.T) {
	dir := testBlueprint(t, "")
	path := filepath.Join(dir, BlueprintFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), APIVersion, DeliveryAPIVersion, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := Validate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(report.Warnings, "\n"), "adaptive preparation gap") {
		t.Fatal("missing contract was not reported")
	}
}

func TestServiceContractExportResolvesFromAssets(t *testing.T) {
	dir, _ := contractBlueprint(t)
	path := filepath.Join(dir, BlueprintFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "kind: fixture_http", "kind: fixture_http\n    export_contracts: true", 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	report, err := Validate(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := report.Blueprint.Services[0].ResolvedEndpoints
	if len(e) != 1 || e[0].ID != "default" || e[0].Path != "/fixture" || e[0].Delivery != nil {
		t.Fatalf("incorrect route-only export: %#v", e)
	}
}
