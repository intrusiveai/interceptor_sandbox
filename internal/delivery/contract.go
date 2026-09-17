// Package delivery owns the bounded, offline descriptions shared by Blueprint
// validation and capability export. A response description is not an attack policy.
package delivery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	MaxContractBytes = 256 << 10
	MaxSchemaBytes   = 64 << 10
	MaxBodyBytes     = 2 << 20
	SchemaProfile    = "interceptor.delivery-schema/v1"
)

type Body struct {
	MediaType string         `yaml:"media_type" json:"media_type"`
	Encoding  string         `yaml:"encoding" json:"encoding"`
	MaxBytes  int64          `yaml:"max_bytes" json:"max_bytes"`
	Basis     string         `yaml:"basis" json:"basis"`
	Schema    map[string]any `yaml:"schema,omitempty" json:"schema,omitempty"`
}

type Example struct {
	ID     string `yaml:"id" json:"id"`
	Input  any    `yaml:"input" json:"input"`
	Output any    `yaml:"output" json:"output"`
}

type Contract struct {
	Description string    `yaml:"description" json:"description"`
	Input       Body      `yaml:"input" json:"input"`
	Output      Body      `yaml:"output" json:"output"`
	Examples    []Example `yaml:"examples,omitempty" json:"examples,omitempty"`
}

// Endpoint describes a selectable service interaction, never a destination.
// For MCP, Delivery.Output describes structuredContent, not the result envelope.
type Endpoint struct {
	ID                  string    `json:"id"`
	Kind                string    `json:"kind"`
	Method              string    `json:"method,omitempty"`
	Path                string    `json:"path,omitempty"`
	Collection          string    `json:"collection,omitempty"`
	Delivery            *Contract `json:"delivery,omitempty"`
	InjectionRoot       string    `json:"injection_root"`
	OutputPointer       string    `json:"output_pointer,omitempty"`
	MaximumRequestBytes int64     `json:"maximum_request_bytes"`
}

// Clone retains JSON numbers exactly while detaching map/slice storage.
func Clone(source, target any) error {
	data, err := json.Marshal(source)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(target)
}

type denyLoader struct{}

func (denyLoader) Load(string) (any, error) {
	return nil, errors.New("external schema resolution is prohibited")
}

func Validate(c *Contract) error {
	if c == nil {
		return nil
	}
	encoded, err := json.Marshal(c)
	if err != nil || len(encoded) > MaxContractBytes {
		return errors.New("delivery contract exceeds encoding/size limits")
	}
	if strings.TrimSpace(c.Description) == "" || len(c.Description) > 4096 {
		return errors.New("delivery description must contain 1..4096 bytes")
	}
	for name, body := range map[string]Body{"input": c.Input, "output": c.Output} {
		if err := ValidateBody(body); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if c.Input.Basis == "unknown" || (c.Input.Encoding == "json" && c.Input.Schema == nil) {
		return errors.New("input requires a reviewed packaging contract; JSON input requires an explicit schema")
	}
	if len(c.Examples) > 8 {
		return errors.New("at most eight delivery examples are permitted")
	}
	seen := map[string]bool{}
	for _, example := range c.Examples {
		if example.ID == "" || len(example.ID) > 128 || seen[example.ID] {
			return errors.New("example IDs must be bounded and unique")
		}
		seen[example.ID] = true
		for name, item := range map[string]struct {
			body  Body
			value any
		}{
			"input": {c.Input, example.Input}, "output": {c.Output, example.Output},
		} {
			encoded, err := EncodeExample(item.body, item.value)
			if err != nil || len(encoded) > 16384 {
				return fmt.Errorf("example %s %s is invalid or exceeds 16 KiB", example.ID, name)
			}
			if err := ValidateBytes(item.body, encoded); err != nil {
				return fmt.Errorf("example %s %s: %w", example.ID, name, err)
			}
		}
	}
	return nil
}

func ValidateBody(body Body) error {
	media, _, err := mime.ParseMediaType(body.MediaType)
	if err != nil || len(body.MediaType) > 128 || media == "" {
		return errors.New("invalid bounded media_type")
	}
	if body.MaxBytes < 1 || body.MaxBytes > MaxBodyBytes {
		return errors.New("max_bytes must be 1..2097152")
	}
	switch body.Encoding {
	case "json":
		if media != "application/json" && !strings.HasSuffix(media, "+json") {
			return errors.New("json encoding requires a JSON media type")
		}
	case "utf8", "bytes":
	default:
		return errors.New("encoding must be json, utf8 or bytes")
	}
	switch body.Basis {
	case "provider-declared", "source-derived", "observed", "synthetic", "unknown":
	default:
		return errors.New("invalid contract basis")
	}
	if body.Basis == "unknown" && body.Schema != nil {
		return errors.New("unknown body cannot assert a schema")
	}
	if body.Encoding == "bytes" && body.Schema != nil {
		return errors.New("opaque bytes cannot assert a JSON schema")
	}
	if body.Schema != nil {
		if _, err := Compile(body.Schema); err != nil {
			return err
		}
	}
	return nil
}

func EncodeExample(body Body, value any) ([]byte, error) {
	if body.Encoding == "json" {
		return json.Marshal(value)
	}
	text, ok := value.(string)
	if !ok {
		return nil, errors.New("text/byte examples must be strings")
	}
	return []byte(text), nil
}

// ValidateBytes validates input packaging or reports an advisory output mismatch.
// It never supplies defaults, coerces types, rewrites bytes or dereferences URLs.
func ValidateBytes(body Body, data []byte) error {
	if int64(len(data)) > body.MaxBytes {
		return errors.New("body exceeds delivery byte limit")
	}
	var value any
	switch body.Encoding {
	case "bytes":
		return nil
	case "utf8":
		if !utf8.Valid(data) {
			return errors.New("body is not UTF-8")
		}
		value = string(data)
	case "json":
		var err error
		value, err = DecodeJSON(data)
		if err != nil {
			return errors.New("body is not bounded strict JSON")
		}
	default:
		return errors.New("unsupported body encoding")
	}
	if body.Schema != nil {
		compiled, err := Compile(body.Schema)
		if err != nil {
			return err
		}
		if compiled.Validate(value) != nil {
			return errors.New("body does not match delivery schema")
		}
	}
	return nil
}

func Compile(schema map[string]any) (*jsonschema.Schema, error) {
	data, err := json.Marshal(schema)
	if err != nil || len(data) > MaxSchemaBytes {
		return nil, errors.New("schema exceeds encoding/size limits")
	}
	normalized, err := DecodeJSON(data)
	if err != nil {
		return nil, errors.New("invalid schema JSON")
	}
	nodes := 0
	if err := checkSchema(normalized, 0, &nodes); err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(denyLoader{})
	const id = "urn:interceptor:delivery:local"
	if err := compiler.AddResource(id, normalized); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile(id)
	if err != nil {
		return nil, errors.New("invalid delivery JSON Schema")
	}
	return compiled, nil
}

// Deliberately closed profile. Unsupported semantic keywords are not ignored.
// Local contract assets provide reuse; recursive/reference schemas are rejected.
func checkSchema(value any, depth int, nodes *int) error {
	*nodes++
	if depth > 8 || *nodes > 256 {
		return errors.New("schema depth/node limit exceeded")
	}
	if _, ok := value.(bool); ok {
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("schema must be an object or boolean")
	}
	for key, child := range object {
		switch key {
		case "$schema":
			if child != "https://json-schema.org/draft/2020-12/schema" {
				return errors.New("unsupported schema dialect")
			}
		case "type", "title", "description", "enum", "const", "required",
			"minLength", "maxLength", "minimum", "maximum", "exclusiveMinimum",
			"exclusiveMaximum", "minItems", "maxItems", "minProperties", "maxProperties":
		case "properties":
			properties, ok := child.(map[string]any)
			if !ok || len(properties) > 128 {
				return errors.New("invalid bounded schema properties")
			}
			for _, property := range properties {
				if err := checkSchema(property, depth+1, nodes); err != nil {
					return err
				}
			}
		case "items", "additionalProperties":
			if err := checkSchema(child, depth+1, nodes); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported delivery schema keyword %q", key)
		}
	}
	return nil
}

// DecodeJSON rejects duplicate keys, excess nesting/nodes and trailing values.
func DecodeJSON(data []byte) (any, error) {
	if !utf8.Valid(data) || len(data) > MaxBodyBytes {
		return nil, errors.New("invalid JSON encoding/size")
	}
	if err := validateEscapes(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	count := 0
	value, err := readValue(decoder, 0, &count)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return value, nil
}
func readValue(d *json.Decoder, depth int, count *int) (any, error) {
	*count++
	if depth > 32 || *count > 100000 {
		return nil, errors.New("JSON complexity exceeded")
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	switch delimiter {
	case '{':
		result := map[string]any{}
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := token.(string)
			if !ok {
				return nil, errors.New("invalid object key")
			}
			if _, found := result[key]; found {
				return nil, errors.New("duplicate JSON key")
			}
			result[key], err = readValue(d, depth+1, count)
			if err != nil {
				return nil, err
			}
		}
		if _, err := d.Token(); err != nil {
			return nil, err
		}
		return result, nil
	case '[':
		result := []any{}
		for d.More() {
			value, err := readValue(d, depth+1, count)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		if _, err := d.Token(); err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, errors.New("unexpected JSON delimiter")
}
