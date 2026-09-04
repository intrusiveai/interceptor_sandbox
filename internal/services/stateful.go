package services

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
	"gopkg.in/yaml.v3"
)

type StatefulRoutes struct {
	Routes []StatefulRoute `yaml:"routes" json:"routes"`
}

var serviceRequestSequence atomic.Uint64

type StatefulRoute struct {
	ID          string   `yaml:"id" json:"id"`
	Method      string   `yaml:"method" json:"method"`
	Path        string   `yaml:"path" json:"path"`
	Action      string   `yaml:"action" json:"action"`
	Collection  string   `yaml:"collection" json:"collection"`
	KeyField    string   `yaml:"key_field,omitempty" json:"key_field,omitempty"`
	PatchFields []string `yaml:"patch_fields,omitempty" json:"patch_fields,omitempty"`
}

type StatefulHandler struct {
	serviceID string
	routes    []StatefulRoute
	journal   *statejournal.Writer
	recorder  *recording.Writer
	mutation  sync.Mutex
	mu        sync.RWMutex
	state     map[string]map[string]json.RawMessage
}

func LoadStateful(seedPath, routesPath string, serviceID string, journal *statejournal.Writer, recorder *recording.Writer) (*StatefulHandler, error) {
	seedData, err := os.ReadFile(seedPath)
	if err != nil {
		return nil, fmt.Errorf("read stateful seed: %w", err)
	}
	var seed map[string]map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(seedData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&seed); err != nil {
		return nil, fmt.Errorf("decode stateful seed: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("stateful seed must contain exactly one JSON value")
	}
	routeData, err := os.ReadFile(routesPath)
	if err != nil {
		return nil, fmt.Errorf("read stateful routes: %w", err)
	}
	yamlDecoder := yaml.NewDecoder(bytes.NewReader(routeData))
	yamlDecoder.KnownFields(true)
	var routes StatefulRoutes
	if err := yamlDecoder.Decode(&routes); err != nil {
		return nil, fmt.Errorf("decode stateful routes: %w", err)
	}
	var trailing any
	if err := yamlDecoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("stateful routes must contain exactly one YAML document")
		}
		return nil, fmt.Errorf("decode trailing route content: %w", err)
	}
	if err := ValidateStatefulRoutes(&routes); err != nil {
		return nil, err
	}
	state := map[string]map[string]json.RawMessage{}
	for collection, resources := range seed {
		state[collection] = map[string]json.RawMessage{}
		for key, value := range resources {
			if key == "" || !json.Valid(value) {
				return nil, fmt.Errorf("seed collection %q contains invalid resource %q", collection, key)
			}
			state[collection][key] = append(json.RawMessage(nil), value...)
		}
	}
	return &StatefulHandler{serviceID: serviceID, routes: routes.Routes, journal: journal, recorder: recorder, state: state}, nil
}

func ValidateStatefulRoutes(routes *StatefulRoutes) error {
	if len(routes.Routes) == 0 {
		return errors.New("stateful route set must not be empty")
	}
	ids, signatures := map[string]struct{}{}, map[string]string{}
	for index := range routes.Routes {
		route := &routes.Routes[index]
		route.Method = strings.ToUpper(strings.TrimSpace(route.Method))
		if route.ID == "" || route.Method == "" || !strings.HasPrefix(route.Path, "/") || route.Collection == "" {
			return fmt.Errorf("stateful route %d requires id, method, absolute path, and collection", index)
		}
		if _, exists := ids[route.ID]; exists {
			return fmt.Errorf("stateful route id %q is duplicated", route.ID)
		}
		ids[route.ID] = struct{}{}
		signature := route.Method + " " + route.Path
		if previous, exists := signatures[signature]; exists {
			return fmt.Errorf("stateful routes %q and %q have the same method and path", previous, route.ID)
		}
		signatures[signature] = route.ID
		hasKey := strings.Count(route.Path, "{") == 1 && strings.Count(route.Path, "}") == 1
		switch route.Action {
		case "list":
			if hasKey {
				return fmt.Errorf("stateful list route %q must not have a key placeholder", route.ID)
			}
		case "get", "replace", "patch", "delete":
			if !hasKey {
				return fmt.Errorf("stateful route %q action %s requires one key placeholder", route.ID, route.Action)
			}
			if route.Action == "patch" && len(route.PatchFields) == 0 {
				return fmt.Errorf("stateful patch route %q requires patch_fields", route.ID)
			}
		case "create":
			if route.KeyField == "" || hasKey {
				return fmt.Errorf("stateful create route %q requires key_field and no path placeholder", route.ID)
			}
		default:
			return fmt.Errorf("stateful route %q has unsupported action %q", route.ID, route.Action)
		}
	}
	return nil
}

func (h *StatefulHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	for _, route := range h.routes {
		key, matched := matchRoute(route, request.Method, request.URL.Path)
		if !matched {
			continue
		}
		if err := h.handle(writer, request, route, key); err != nil {
			h.record("stateful_error", map[string]any{"route": route.ID, "error": err.Error()})
			writeServiceError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		}
		return
	}
	h.record("coverage_gap", map[string]any{"service": h.serviceID, "method": request.Method, "path": request.URL.Path})
	writeServiceError(writer, http.StatusNotImplemented, "coverage_gap", "no sealed stateful route matched request")
}

func (h *StatefulHandler) handle(writer http.ResponseWriter, request *http.Request, route StatefulRoute, key string) error {
	if route.Action == "create" || route.Action == "replace" || route.Action == "patch" || route.Action == "delete" {
		h.mutation.Lock()
		defer h.mutation.Unlock()
	}
	switch route.Action {
	case "list":
		h.mu.RLock()
		resources := h.state[route.Collection]
		keys := make([]string, 0, len(resources))
		for resourceKey := range resources {
			keys = append(keys, resourceKey)
		}
		sort.Strings(keys)
		items := make([]json.RawMessage, 0, len(keys))
		for _, resourceKey := range keys {
			items = append(items, append(json.RawMessage(nil), resources[resourceKey]...))
		}
		h.mu.RUnlock()
		h.writeJSON(writer, http.StatusOK, items)
		h.record("stateful_read", map[string]any{"route": route.ID, "collection": route.Collection, "count": len(items)})
		return nil
	case "get":
		value, exists := h.get(route.Collection, key)
		if !exists {
			writeServiceError(writer, http.StatusNotFound, "not_found", "resource not found")
			return nil
		}
		h.writeRawJSON(writer, http.StatusOK, value)
		h.record("stateful_read", map[string]any{"route": route.ID, "collection": route.Collection, "key": key})
		return nil
	case "delete":
		if _, exists := h.get(route.Collection, key); !exists {
			writeServiceError(writer, http.StatusNotFound, "not_found", "resource not found")
			return nil
		}
		operation := statejournal.Operation{Kind: "service_delete", Service: h.serviceID, Collection: route.Collection, Key: key}
		if err := h.commit("stateful_delete", []statejournal.Operation{operation}); err != nil {
			return err
		}
		h.mu.Lock()
		delete(h.state[route.Collection], key)
		h.mu.Unlock()
		writer.WriteHeader(http.StatusNoContent)
		h.record("stateful_mutation", map[string]any{"route": route.ID, "action": route.Action, "collection": route.Collection, "key": key})
		return nil
	case "create", "replace", "patch":
		body, err := readBounded(request.Body, maxServiceRequestBody)
		if err != nil {
			return err
		}
		var object map[string]any
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if err := decoder.Decode(&object); err != nil {
			return fmt.Errorf("decode JSON object: %w", err)
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return errors.New("request body must contain exactly one JSON object")
		}
		if route.Action == "create" {
			keyValue, exists := object[route.KeyField]
			if !exists || fmt.Sprint(keyValue) == "" {
				return fmt.Errorf("create requires key field %q", route.KeyField)
			}
			key = fmt.Sprint(keyValue)
			if _, exists := h.get(route.Collection, key); exists {
				writeServiceError(writer, http.StatusConflict, "conflict", "resource already exists")
				return nil
			}
		} else if _, exists := h.get(route.Collection, key); !exists {
			writeServiceError(writer, http.StatusNotFound, "not_found", "resource not found")
			return nil
		}
		if route.Action == "patch" {
			current, _ := h.get(route.Collection, key)
			var merged map[string]any
			if err := json.Unmarshal(current, &merged); err != nil {
				return err
			}
			allowed := map[string]struct{}{}
			for _, field := range route.PatchFields {
				allowed[field] = struct{}{}
			}
			for field, value := range object {
				if _, ok := allowed[field]; !ok {
					return fmt.Errorf("patch field %q is not allowed", field)
				}
				merged[field] = value
			}
			object = merged
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return err
		}
		operation := statejournal.Operation{Kind: "service_put", Service: h.serviceID, Collection: route.Collection, Key: key, Value: encoded}
		if err := h.commit("stateful_"+route.Action, []statejournal.Operation{operation}); err != nil {
			return err
		}
		h.mu.Lock()
		if h.state[route.Collection] == nil {
			h.state[route.Collection] = map[string]json.RawMessage{}
		}
		h.state[route.Collection][key] = append(json.RawMessage(nil), encoded...)
		h.mu.Unlock()
		status := http.StatusOK
		if route.Action == "create" {
			status = http.StatusCreated
		}
		h.writeRawJSON(writer, status, encoded)
		h.record("stateful_mutation", map[string]any{"route": route.ID, "action": route.Action, "collection": route.Collection, "key": key})
		return nil
	default:
		return fmt.Errorf("unsupported action %q", route.Action)
	}
}

func (h *StatefulHandler) Apply(operation statejournal.Operation) error {
	if operation.Service != h.serviceID {
		return fmt.Errorf("operation service %q does not match %q", operation.Service, h.serviceID)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch operation.Kind {
	case "service_put":
		if h.state[operation.Collection] == nil {
			h.state[operation.Collection] = map[string]json.RawMessage{}
		}
		h.state[operation.Collection][operation.Key] = append(json.RawMessage(nil), operation.Value...)
	case "service_delete":
		delete(h.state[operation.Collection], operation.Key)
	default:
		return fmt.Errorf("unsupported service operation %q", operation.Kind)
	}
	return nil
}

func (h *StatefulHandler) SupportsCreate(collection string) bool {
	for _, route := range h.routes {
		if route.Action == "create" && route.Collection == collection {
			return true
		}
	}
	return false
}

func (h *StatefulHandler) ValidateCreate(operation statejournal.Operation) error {
	if operation.Kind != "service_put" || operation.Service != h.serviceID || operation.Collection == "" || operation.Key == "" || !json.Valid(operation.Value) {
		return errors.New("stateful resource creation requires a valid service_put operation")
	}
	if _, exists := h.get(operation.Collection, operation.Key); exists {
		return errors.New("stateful resource already exists")
	}
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(operation.Value))
	decoder.UseNumber()
	if decoder.Decode(&object) != nil || object == nil || decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("stateful resource creation requires exactly one JSON object")
	}
	for _, route := range h.routes {
		if route.Action != "create" || route.Collection != operation.Collection {
			continue
		}
		key, exists := object[route.KeyField]
		if exists && fmt.Sprint(key) == operation.Key {
			return nil
		}
	}
	return fmt.Errorf("created resource does not satisfy a sealed create route for collection %q", operation.Collection)
}

func (h *StatefulHandler) Snapshot() map[string]map[string]json.RawMessage {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := map[string]map[string]json.RawMessage{}
	for collection, resources := range h.state {
		result[collection] = map[string]json.RawMessage{}
		for key, value := range resources {
			result[collection][key] = append(json.RawMessage(nil), value...)
		}
	}
	return result
}

func (h *StatefulHandler) get(collection, key string) (json.RawMessage, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	value, exists := h.state[collection][key]
	return append(json.RawMessage(nil), value...), exists
}

func (h *StatefulHandler) commit(reason string, operations []statejournal.Operation) error {
	if h.journal == nil {
		return errors.New("state journal is unavailable")
	}
	_, err := h.journal.Append(reason, operations)
	return err
}

func (h *StatefulHandler) record(kind string, semantic any) {
	if h.recorder == nil {
		return
	}
	_, _ = h.recorder.Append(recording.Draft{Channel: recording.Evaluation, Layer: "stateful_service", Kind: kind, Provenance: recording.EnvironmentSealed, Semantic: semantic})
}

func (h *StatefulHandler) writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func (h *StatefulHandler) writeRawJSON(writer http.ResponseWriter, status int, value []byte) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(value)
}

func matchRoute(route StatefulRoute, method, path string) (string, bool) {
	if route.Method != method {
		return "", false
	}
	open, close := strings.Index(route.Path, "{"), strings.Index(route.Path, "}")
	if open < 0 || close < 0 {
		return "", route.Path == path
	}
	prefix, suffix := route.Path[:open], route.Path[close+1:]
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) || len(path) < len(prefix)+len(suffix) {
		return "", false
	}
	key := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if key == "" || strings.Contains(key, "/") {
		return "", false
	}
	return key, true
}

func writeServiceError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]any{"code": code, "message": message, "request_id": fmt.Sprintf("req-%d", serviceRequestSequence.Add(1))}})
}
