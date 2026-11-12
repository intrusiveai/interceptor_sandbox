package identity

import (
	"sort"

	"github.com/intrusive-ai/interceptor-sandbox/internal/delivery"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

// StateEndpoints describes only sealed identifiers and record structure. No
// credentials, principal claims, scopes values or live authorization state leak.
func StateEndpoints(config environment.Identity, issuerID string) []delivery.Endpoint {
	principals := map[string]bool{}
	for _, p := range config.Principals {
		if p.Issuer == issuerID {
			principals[p.ID] = true
		}
	}
	keys := map[string][]string{"faults": {"issuer"}}
	for _, g := range config.Grants {
		if principals[g.Principal] {
			keys["grants"] = append(keys["grants"], g.ID)
		}
	}
	for _, d := range config.Delegations {
		if principals[d.Agent] {
			keys["delegations"] = append(keys["delegations"], d.ID)
		}
	}
	result := []delivery.Endpoint{}
	for _, collection := range []string{"delegations", "faults", "grants"} {
		if len(keys[collection]) == 0 {
			continue
		}
		sort.Strings(keys[collection])
		fields := []string{"active", "scopes"}
		props := map[string]any{"active": map[string]any{"type": "boolean"}, "scopes": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "minLength": 1}}}
		ids := []string{"id", "service", "principal"}
		if collection == "delegations" {
			ids = []string{"id", "service", "agent", "user"}
		}
		if collection == "faults" {
			fields = []string{"issue_expired", "reject_exchange", "unavailable"}
			props = map[string]any{}
			ids = nil
			for _, f := range fields {
				props[f] = map[string]any{"type": "boolean"}
			}
		}
		required := append([]string{}, fields...)
		for _, id := range ids {
			props[id] = map[string]any{"type": "string"}
			required = append(required, id)
		}
		body := delivery.Body{MediaType: "application/json", Encoding: "json", Basis: "source-derived", MaxBytes: 1 << 20, Schema: map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}}
		result = append(result, delivery.Endpoint{ID: collection, Kind: "state-collection", Collection: collection, Keys: keys[collection], MutableFields: fields, InjectionRoot: "service-record", MaximumRequestBytes: 1 << 20,
			Delivery: &delivery.Contract{Description: "Existing identity records only. Input/output describe the complete record, not the injection envelope. Use environment_state with collection/key and a JSON Pointer into mutable_fields. Identifying fields are immutable; no creation or deletion. Runtime validates scopes and record identity.", Input: body, Output: body}})
	}
	return result
}
