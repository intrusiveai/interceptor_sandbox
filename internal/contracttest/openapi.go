// Package contracttest validates qualification traffic against the embedded
// OpenAPI contract. Production API handlers do not depend on this package.
package contracttest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/integrationcontract"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type Validator struct {
	Document map[string]any
	compiler *jsonschema.Compiler
}
type offlineLoader struct{}

func (offlineLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external contract reference is forbidden: %s", url)
}

const documentURL = "urn:interceptor:openapi"

func New() (*Validator, error) {
	var doc map[string]any
	if err := json.Unmarshal(integrationcontract.JSON(), &doc); err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	c.AssertContent()
	c.UseLoader(offlineLoader{})
	if err := c.AddResource(documentURL, doc); err != nil {
		return nil, err
	}
	return &Validator{doc, c}, nil
}
func (v *Validator) Schema(pointer string) (*jsonschema.Schema, error) {
	return v.compiler.Compile(documentURL + pointer)
}
func (v *Validator) Validate(pointer string, raw []byte) error {
	s, err := v.Schema(pointer)
	if err != nil {
		return err
	}
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	return s.Validate(value)
}
func escape(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1") }

// Exchange checks actual request and response bytes. Invalid requests are allowed
// only when the server rejects them, which lets negative qualification proceed.
func (v *Validator) Exchange(path string, request []byte, status int, response []byte) error {
	routes := v.Document["paths"].(map[string]any)
	route, ok := routes[path].(map[string]any)
	if !ok {
		return fmt.Errorf("undocumented route %s", path)
	}
	post := route["post"].(map[string]any)
	prefix := "#/paths/" + escape(path) + "/post"
	if status < 300 {
		if err := v.Validate(prefix+"/requestBody/content/application~1json/schema", request); err != nil {
			return fmt.Errorf("%s request: %w", path, err)
		}
	}
	rs := post["responses"].(map[string]any)
	code := strconv.Itoa(status)
	if _, ok := rs[code]; !ok {
		code = "default"
	}
	pointer := prefix + "/responses/" + code + "/content/application~1json/schema"
	if path == "/v1/operations" && status < 300 && status != 202 {
		var input struct {
			Request struct {
				Operation string `json:"operation"`
			} `json:"request"`
		}
		if err := json.Unmarshal(request, &input); err != nil {
			return err
		}
		schemas := v.Document["components"].(map[string]any)["schemas"].(map[string]any)
		for _, branch := range schemas["NativeRequest"].(map[string]any)["oneOf"].([]any) {
			ref := branch.(map[string]any)["$ref"].(string)
			variant := schemas[strings.TrimPrefix(ref, "#/components/schemas/")].(map[string]any)
			props := variant["properties"].(map[string]any)
			header := props["request"].(map[string]any)["properties"].(map[string]any)
			if header["operation"].(map[string]any)["const"] == input.Request.Operation {
				pointer = variant["x-response-schema"].(map[string]any)["$ref"].(string)
				break
			}
		}
	}
	if err := v.Validate(pointer, response); err != nil {
		return fmt.Errorf("%s HTTP %d response: %w", path, status, err)
	}
	var wrapped struct {
		Status int `json:"status"`
	}
	if err := json.Unmarshal(response, &wrapped); err != nil {
		return err
	}
	if wrapped.Status != status && !(status == 200 && wrapped.Status == 204) {
		return fmt.Errorf("HTTP %d disagrees with envelope status %d", status, wrapped.Status)
	}
	return nil
}
