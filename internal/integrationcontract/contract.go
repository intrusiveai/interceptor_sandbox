// Package integrationcontract owns the release-independent Interceptor contract.
package integrationcontract

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
)

//go:embed contract.json
var document string

// Version is derived from the embedded document, never a product release flag.
func Version() string { return identity.InterceptorContractVersion }

// JSON returns the complete reference document without runtime dependencies.
func JSON() json.RawMessage { return json.RawMessage(document) }

// SHA256 identifies the complete embedded document, including whitespace and
// its final newline. The digest is not embedded in the document it identifies.
func SHA256() string {
	sum := sha256.Sum256([]byte(document))
	return hex.EncodeToString(sum[:])
}

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
