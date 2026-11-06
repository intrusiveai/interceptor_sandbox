package integrationcontract_test

import (
	"encoding/json"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/contracttest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/localapi"
	"github.com/intrusive-ai/interceptor-sandbox/internal/sandboxd"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

// Catch wire-field additions/removals even when an existing traffic fixture does
// not exercise that optional field. Semantic constraints are tested separately.
func TestOpenAPIModelFieldsMatchWireTypes(t *testing.T) {
	v, err := contracttest.New()
	if err != nil {
		t.Fatal(err)
	}
	schemas := v.Document["components"].(map[string]any)["schemas"].(map[string]any)
	// Resolve only the outer JSON types; fields can use nullable/reference unions.
	var schemaTypes func(map[string]any) map[string]bool
	schemaTypes = func(s map[string]any) map[string]bool {
		out := map[string]bool{}
		if r, ok := s["$ref"].(string); ok {
			for k := range schemaTypes(schemas[strings.TrimPrefix(r, "#/components/schemas/")].(map[string]any)) {
				out[k] = true
			}
		}
		switch types := s["type"].(type) {
		case string:
			out[types] = true
		case []any:
			for _, x := range types {
				out[x.(string)] = true
			}
		}
		for _, keyword := range []string{"anyOf", "oneOf", "allOf"} {
			if branches, ok := s[keyword].([]any); ok {
				for _, branch := range branches {
					for k := range schemaTypes(branch.(map[string]any)) {
						out[k] = true
					}
				}
			}
		}
		return out
	}
	checkType := func(typ reflect.Type, s map[string]any, name string) {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ == reflect.TypeFor[json.RawMessage]() || typ.Kind() == reflect.Interface {
			return
		}
		want := ""
		switch typ.Kind() {
		case reflect.Struct, reflect.Map:
			want = "object"
		case reflect.Slice:
			want = "array"
			if typ.Elem().Kind() == reflect.Uint8 {
				want = "string"
			}
		case reflect.String:
			want = "string"
		case reflect.Bool:
			want = "boolean"
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			want = "integer"
		case reflect.Float32, reflect.Float64:
			want = "number"
		}
		if typ == reflect.TypeFor[time.Time]() {
			want = "string"
		}
		if !schemaTypes(s)[want] {
			t.Errorf("%s wire type %s disagrees with schema JSON types %v", name, typ, schemaTypes(s))
		}
	}
	seen := map[reflect.Type]bool{}
	var visit func(reflect.Type)
	visit = func(typ reflect.Type) {
		if typ == reflect.TypeFor[time.Time]() || typ == reflect.TypeFor[json.RawMessage]() {
			return
		}
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map:
			visit(typ.Elem())
			return
		case reflect.Struct:
		default:
			return
		}
		if seen[typ] {
			return
		}
		seen[typ] = true
		name := path.Base(typ.PkgPath()) + "." + typ.Name()
		model, ok := schemas[name].(map[string]any)
		if !ok {
			t.Errorf("missing model %s", name)
			return
		}
		variants := []map[string]any{model}
		if name == "localapi.LifecycleRequest" {
			variants = nil
			for _, n := range []string{"LifecycleStopRequest", "LifecycleRestoreRequest"} {
				variants = append(variants, schemas[n].(map[string]any))
			}
		}
		if name == "adaptive.GatewayResponse" {
			return
		} // Explicit typed union, exercised through actual traffic.
		for _, model := range variants {
			props := model["properties"].(map[string]any)
			want := map[string]bool{}
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				tag := strings.Split(f.Tag.Get("json"), ",")[0]
				if f.PkgPath != "" || tag == "-" {
					continue
				}
				if tag == "" {
					tag = f.Name
				}
				want[tag] = true
				if _, ok := props[tag]; !ok {
					t.Errorf("%s missing %s", name, tag)
				}
				if schema, ok := props[tag].(map[string]any); ok {
					checkType(f.Type, schema, name+"."+tag)
				}
				visit(f.Type)
			}
			for field := range props {
				if !want[field] {
					t.Errorf("%s invents wire field %s", name, field)
				}
			}
		}
	}
	for _, value := range []any{adaptive.OperationRequest{}, adaptive.AttemptContext{}, adaptive.ObservationView{}, adaptive.ObservationRequest{}, adaptive.FeedbackReadRequest{}, adaptive.FeedbackChunk{}, adaptive.OwnerRequest{}, adaptive.ExecutionOwner{}, adaptive.OperationRecord{}, adaptive.CapabilityManifest{}, localapi.AttachRequest{}, localapi.LifecycleRequest{}, localapi.Binding{}, localapi.Record{}, localapi.ExecutionFailure{}, localapi.SnapshotListRequest{}, localapi.SnapshotInspectRequest{}, localapi.SnapshotListResponse{}, sandboxd.TurnRequest{}, sandboxd.Turn{}, injection.Definition{}, injection.PublicState{}, snapshot.Checkpoint{}, session.Metadata{}} {
		visit(reflect.TypeOf(value))
	}
}
