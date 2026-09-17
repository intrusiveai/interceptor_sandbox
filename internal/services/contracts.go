package services

import (
	"sort"

	"github.com/intrusive-ai/interceptor-sandbox/internal/delivery"
)

// This is a closed release-owned implementation registry, not a plugin loader.
// Named implementations must add validated configuration, native factory,
// contract export and conformance together. No third-party implementation ships.
func SupportsMCPImplementation(id string) bool { return id == "" || id == "generic" }

// DeliveryEndpoints exports only explicitly reviewed descriptions. No fixture
// matches/results, seed records, credentials or service listeners enter this view.
func (h *MCPHandler) DeliveryEndpoints() ([]delivery.Endpoint, error) {
	result := []delivery.Endpoint{}
	for _, tool := range h.tools {
		basis := tool.ContractBasis
		if basis == "" {
			basis = "synthetic"
		}
		contract := &delivery.Contract{
			Description: tool.Description,
			Input:       delivery.Body{MediaType: "application/json", Encoding: "json", MaxBytes: maxServiceRequestBody, Basis: basis, Schema: tool.InputSchema},
			Output:      delivery.Body{MediaType: "application/json", Encoding: "json", MaxBytes: maxServiceRequestBody, Basis: "unknown"},
			Examples:    tool.DeliveryExamples,
		}
		if tool.OutputSchema != nil {
			contract.Output.Basis = basis
			contract.Output.Schema = tool.OutputSchema
		}
		if err := delivery.Validate(contract); err != nil {
			return nil, err
		}
		result = append(result, delivery.Endpoint{ID: tool.Name, Kind: "mcp-tool", Collection: tool.Behavior.Collection,
			Delivery: contract, InjectionRoot: "mcp-tool-result", OutputPointer: "/structuredContent", MaximumRequestBytes: maxServiceRequestBody})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	// Detach mutable implementation maps from the public representation.
	var copied []delivery.Endpoint
	err := delivery.Clone(result, &copied)
	return copied, err
}

func (h *StatefulHandler) DeliveryEndpoints() []delivery.Endpoint {
	result := []delivery.Endpoint{}
	for _, route := range h.routes {
		result = append(result, delivery.Endpoint{ID: route.ID, Kind: "http-route", Method: route.Method,
			Path: route.Path, Collection: route.Collection, Delivery: route.Delivery, InjectionRoot: "response-body", MaximumRequestBytes: maxServiceRequestBody})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (s *FixtureSet) DeliveryEndpoints() []delivery.Endpoint {
	result := []delivery.Endpoint{}
	for _, rule := range s.Rules {
		result = append(result, delivery.Endpoint{ID: rule.ID, Kind: "http-route", Method: rule.Match.Method,
			Path: rule.Match.Path, Delivery: rule.Delivery, InjectionRoot: "response-body", MaximumRequestBytes: maxServiceRequestBody})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
