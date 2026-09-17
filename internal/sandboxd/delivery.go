package sandboxd

import (
	"errors"
	"mime"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/delivery"
)

func (s *Server) validateDeliveryInput(operationID, mediaType string, data []byte) error {
	operation, ok := s.operation(operationID)
	if !ok {
		return nil
	} // Existing route/operation validation owns rejection.
	contract := operation.ResolvedDelivery
	if contract == nil {
		contract = operation.Delivery
	}
	if contract == nil {
		return nil
	} // Legacy/direct use; capability reports the gap.
	if len(data) > delivery.MaxBodyBytes || (s.blueprint.Limits.MaxBlobBytes > 0 && int64(len(data)) > s.blueprint.Limits.MaxBlobBytes) {
		return errors.New("input exceeds effective native byte limit")
	}
	supplied, parameters, err := mime.ParseMediaType(mediaType)
	expected, _, _ := mime.ParseMediaType(contract.Input.MediaType)
	if err != nil || supplied != expected {
		return errors.New("input media type does not match delivery contract")
	}
	if charset := parameters["charset"]; charset != "" && contract.Input.Encoding != "bytes" && !strings.EqualFold(charset, "utf-8") {
		return errors.New("input charset must be UTF-8")
	}
	return delivery.ValidateBytes(contract.Input, data)
}

// Output conformance is advisory. Preserve the actual bounded response even
// when an injected response deliberately differs from its benign contract.
func (s *Server) outputContractStatus(operationID, mediaType string, data []byte) string {
	operation, ok := s.operation(operationID)
	if !ok {
		return ""
	}
	contract := operation.ResolvedDelivery
	if contract == nil {
		contract = operation.Delivery
	}
	if contract == nil {
		return ""
	}
	supplied, _, err := mime.ParseMediaType(mediaType)
	expected, _, _ := mime.ParseMediaType(contract.Output.MediaType)
	if err != nil || supplied != expected || delivery.ValidateBytes(contract.Output, data) != nil {
		return "mismatch"
	}
	if contract.Output.Basis == "unknown" {
		return "unknown"
	}
	return "matched"
}
