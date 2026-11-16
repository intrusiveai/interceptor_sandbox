package contracttest

import (
	"encoding/json"
	"github.com/intrusive-ai/interceptor-sandbox/internal/delivery"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/identity"
	"github.com/intrusive-ai/interceptor-sandbox/internal/services"
	"testing"
)

func TestOperationVariantsEnforceBodiesAndResponses(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	header := map[string]any{"api_version": "interceptor.dev/operation-request/v1alpha2", "request_id": "r", "operation_id": "o", "operation": "artifact.read", "session_id": "s", "campaign_id": "c", "worker_instance_id": "w", "run_revision": 1, "body_digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "deadline": "2026-01-01T00:00:00Z"}
	input := map[string]any{"request": header, "body": map[string]any{"digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	raw, _ := json.Marshal(input)
	good := []byte(`{"status":200,"body":{"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","content":"aGk="},"session_revision":1}`)
	if err := v.Exchange("/v1/operations", raw, 200, good); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func()
	}{
		{"unknown operation", func() { header["operation"] = "shell.exec" }},
		{"wrong operation body", func() { header["operation"] = "snapshot.create" }},
		{"missing field", func() { delete(header, "campaign_id") }},
		{"wrong field type", func() { header["run_revision"] = "1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var copy map[string]any
			json.Unmarshal(raw, &copy)
			header = copy["request"].(map[string]any)
			tc.change()
			changed, _ := json.Marshal(copy)
			if v.Exchange("/v1/operations", changed, 200, good) == nil {
				t.Fatal("accepted incompatible request")
			}
		})
	}
	// A valid response for another operation must not satisfy artifact.read.
	wrong := []byte(`{"status":200,"body":{"session_id":"s","phase":"stopped"},"session_revision":1}`)
	if v.Exchange("/v1/operations", raw, 200, wrong) == nil {
		t.Fatal("accepted wrong operation response")
	}
	for _, bad := range []string{`{"status":200,"body":{"digest":42,"content":""},"session_revision":1}`, `{"status":200,"body":{"content":""},"session_revision":1}`, `{"status":200,"body":{"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","content":"not-base64!"},"session_revision":1}`} {
		if v.Exchange("/v1/operations", raw, 200, []byte(bad)) == nil {
			t.Fatal("accepted malformed response")
		}
	}
}

func TestBuiltInInjectionEndpointDefinitions(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []struct {
		endpoint delivery.Endpoint
		change   func(*delivery.Endpoint)
	}{
		{*services.SinkEndpoint(nil, nil, services.SinkResponse{}, 1024), func(ep *delivery.Endpoint) { ep.Method = "POST"; ep.Path = "/made-up" }},
		{identity.StateEndpoints(environment.Identity{}, "issuer")[0], func(ep *delivery.Endpoint) { ep.Keys = nil }},
		{*services.SinkEndpoint(nil, nil, services.SinkResponse{}, 1024), func(ep *delivery.Endpoint) { ep.Kind = "invented-route" }},
	}
	for _, fixture := range fixtures {
		raw, err := json.Marshal(fixture.endpoint)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Validate("#/components/schemas/delivery.Endpoint", raw); err != nil {
			t.Fatal("valid endpoint rejected", err)
		}
		fixture.change(&fixture.endpoint)
		raw, err = json.Marshal(fixture.endpoint)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Validate("#/components/schemas/delivery.Endpoint", raw); err == nil {
			t.Fatal("invalid endpoint accepted", string(raw))
		}
	}
}
