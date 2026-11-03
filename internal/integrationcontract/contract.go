// Package integrationcontract owns the release-independent Interceptor contract.
package integrationcontract

import (
	_ "embed"
	"encoding/json"
)

//go:embed contract.json
var document string

// Version is derived from the embedded document, never a product release flag.
func Version() string { return identity.InterceptorContractVersion }

// JSON returns the complete reference document without runtime dependencies.
func JSON() json.RawMessage { return json.RawMessage(document) }

var identity = func() struct {
	InterceptorContractVersion string `json:"interceptor_contract_version"`
} {
	var value struct {
		InterceptorContractVersion string `json:"interceptor_contract_version"`
	}
	if err := json.Unmarshal([]byte(document), &value); err != nil || value.InterceptorContractVersion == "" {
		panic("invalid embedded Interceptor contract")
	}
	return value
}()
