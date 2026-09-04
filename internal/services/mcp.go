package services

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
	"gopkg.in/yaml.v3"
)

const defaultMCPProtocolVersion = "2025-11-25"

var mcpToolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

type MCPDefinition struct {
	Tools []MCPTool `yaml:"tools" json:"tools"`
}

type MCPTool struct {
	Name         string         `yaml:"name" json:"name"`
	Title        string         `yaml:"title,omitempty" json:"title,omitempty"`
	Description  string         `yaml:"description" json:"description"`
	InputSchema  map[string]any `yaml:"input_schema" json:"input_schema"`
	OutputSchema map[string]any `yaml:"output_schema,omitempty" json:"output_schema,omitempty"`
	Behavior     MCPBehavior    `yaml:"behavior" json:"behavior"`
}

type MCPBehavior struct {
	Kind        string           `yaml:"kind" json:"kind"`
	Fixtures    []MCPFixtureRule `yaml:"fixtures,omitempty" json:"fixtures,omitempty"`
	Action      string           `yaml:"action,omitempty" json:"action,omitempty"`
	Collection  string           `yaml:"collection,omitempty" json:"collection,omitempty"`
	KeyArgument string           `yaml:"key_argument,omitempty" json:"key_argument,omitempty"`
	KeyField    string           `yaml:"key_field,omitempty" json:"key_field,omitempty"`
	PatchFields []string         `yaml:"patch_fields,omitempty" json:"patch_fields,omitempty"`
}

type MCPFixtureRule struct {
	ID       string         `yaml:"id" json:"id"`
	Priority int            `yaml:"priority" json:"priority"`
	Match    map[string]any `yaml:"match,omitempty" json:"match,omitempty"`
	Result   MCPToolResult  `yaml:"result" json:"result"`
}

type MCPToolResult struct {
	Content           []MCPContent `yaml:"content,omitempty" json:"content,omitempty"`
	StructuredContent any          `yaml:"structured_content,omitempty" json:"structured_content,omitempty"`
	IsError           bool         `yaml:"is_error,omitempty" json:"is_error,omitempty"`
}

type MCPContent struct {
	Type string `yaml:"type" json:"type"`
	Text string `yaml:"text" json:"text"`
}

type MCPHandler struct {
	serviceID string
	endpoint  string
	tools     []MCPTool
	byName    map[string]*MCPTool
	journal   *statejournal.Writer
	recorder  *recording.Writer
	mutation  sync.Mutex
	mu        sync.RWMutex
	state     map[string]map[string]json.RawMessage
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func LoadMCP(toolsPath, seedPath, endpoint, serviceID string, journal *statejournal.Writer, recorder *recording.Writer) (*MCPHandler, error) {
	data, err := os.ReadFile(toolsPath)
	if err != nil {
		return nil, fmt.Errorf("read MCP tools: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var definition MCPDefinition
	if err := decoder.Decode(&definition); err != nil {
		return nil, fmt.Errorf("decode MCP tools: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("MCP tools must contain exactly one YAML document")
		}
		return nil, fmt.Errorf("decode trailing MCP tool data: %w", err)
	}
	if err := ValidateMCPDefinition(&definition); err != nil {
		return nil, err
	}
	state, err := loadMCPSeed(seedPath)
	if err != nil {
		return nil, err
	}
	if err := validateMCPSeedRequirements(&definition, state, seedPath != ""); err != nil {
		return nil, err
	}
	if endpoint == "" {
		endpoint = "/mcp"
	}
	byName := make(map[string]*MCPTool, len(definition.Tools))
	for index := range definition.Tools {
		tool := &definition.Tools[index]
		byName[tool.Name] = tool
	}
	return &MCPHandler{
		serviceID: serviceID, endpoint: endpoint, tools: definition.Tools, byName: byName,
		journal: journal, recorder: recorder, state: state,
	}, nil
}

func ValidateMCPDefinition(definition *MCPDefinition) error {
	if definition == nil || len(definition.Tools) == 0 {
		return errors.New("MCP definition must contain at least one tool")
	}
	seen := map[string]struct{}{}
	for toolIndex := range definition.Tools {
		tool := &definition.Tools[toolIndex]
		prefix := fmt.Sprintf("MCP tool %d", toolIndex)
		if !mcpToolName.MatchString(tool.Name) {
			return fmt.Errorf("%s name must contain 1-128 letters, digits, dots, underscores, or hyphens", prefix)
		}
		if _, exists := seen[tool.Name]; exists {
			return fmt.Errorf("MCP tool name %q is duplicated", tool.Name)
		}
		seen[tool.Name] = struct{}{}
		if strings.TrimSpace(tool.Description) == "" {
			return fmt.Errorf("MCP tool %q requires a description", tool.Name)
		}
		if err := validateMCPSchema(tool.InputSchema, true); err != nil {
			return fmt.Errorf("MCP tool %q input_schema: %w", tool.Name, err)
		}
		if len(tool.OutputSchema) > 0 {
			if err := validateMCPSchema(tool.OutputSchema, false); err != nil {
				return fmt.Errorf("MCP tool %q output_schema: %w", tool.Name, err)
			}
		}
		switch tool.Behavior.Kind {
		case "fixture":
			if err := validateMCPFixtures(tool.Name, tool.Behavior.Fixtures); err != nil {
				return err
			}
			if tool.Behavior.Action != "" || tool.Behavior.Collection != "" || tool.Behavior.KeyArgument != "" || tool.Behavior.KeyField != "" || len(tool.Behavior.PatchFields) > 0 {
				return fmt.Errorf("MCP fixture tool %q may not declare stateful behavior fields", tool.Name)
			}
		case "stateful":
			if len(tool.Behavior.Fixtures) > 0 || tool.Behavior.Collection == "" {
				return fmt.Errorf("MCP stateful tool %q requires collection and may not declare fixtures", tool.Name)
			}
			switch tool.Behavior.Action {
			case "list":
				if tool.Behavior.KeyArgument != "" || tool.Behavior.KeyField != "" || len(tool.Behavior.PatchFields) > 0 {
					return fmt.Errorf("MCP stateful list tool %q has incompatible key or patch fields", tool.Name)
				}
			case "get", "delete", "replace":
				if tool.Behavior.KeyArgument == "" || tool.Behavior.KeyField != "" || len(tool.Behavior.PatchFields) > 0 {
					return fmt.Errorf("MCP stateful %s tool %q requires key_argument only", tool.Behavior.Action, tool.Name)
				}
			case "patch":
				if tool.Behavior.KeyArgument == "" || len(tool.Behavior.PatchFields) == 0 || tool.Behavior.KeyField != "" {
					return fmt.Errorf("MCP stateful patch tool %q requires key_argument and patch_fields", tool.Name)
				}
			case "create":
				if tool.Behavior.KeyField == "" || tool.Behavior.KeyArgument != "" || len(tool.Behavior.PatchFields) > 0 {
					return fmt.Errorf("MCP stateful create tool %q requires key_field only", tool.Name)
				}
			default:
				return fmt.Errorf("MCP stateful tool %q has unsupported action %q", tool.Name, tool.Behavior.Action)
			}
			if err := validateMCPStatefulSchema(tool); err != nil {
				return err
			}
		default:
			return fmt.Errorf("MCP tool %q has unsupported behavior kind %q", tool.Name, tool.Behavior.Kind)
		}
	}
	return nil
}

func validateMCPSchema(schema map[string]any, requireObject bool) error {
	if len(schema) == 0 {
		return errors.New("schema is required")
	}
	typeName, _ := schema["type"].(string)
	if requireObject && typeName != "object" {
		return errors.New("top-level type must be object")
	}
	if typeName == "" {
		return errors.New("type is required")
	}
	if properties, exists := schema["properties"]; exists {
		if _, ok := properties.(map[string]any); !ok {
			return errors.New("properties must be an object")
		}
	}
	if required, exists := schema["required"]; exists {
		values, ok := stringList(required)
		if !ok {
			return errors.New("required must be an array of strings")
		}
		properties, _ := schema["properties"].(map[string]any)
		for _, name := range values {
			if _, exists := properties[name]; !exists {
				return fmt.Errorf("required property %q is not declared", name)
			}
		}
	}
	return nil
}

func validateMCPFixtures(toolName string, fixtures []MCPFixtureRule) error {
	if len(fixtures) == 0 {
		return fmt.Errorf("MCP fixture tool %q requires at least one fixture", toolName)
	}
	ids := map[string]struct{}{}
	for index := range fixtures {
		fixture := &fixtures[index]
		if fixture.ID == "" {
			return fmt.Errorf("MCP tool %q fixture %d requires id", toolName, index)
		}
		if _, exists := ids[fixture.ID]; exists {
			return fmt.Errorf("MCP tool %q fixture id %q is duplicated", toolName, fixture.ID)
		}
		ids[fixture.ID] = struct{}{}
		if err := validateMCPResult(fixture.Result); err != nil {
			return fmt.Errorf("MCP tool %q fixture %q result: %w", toolName, fixture.ID, err)
		}
		for previous := 0; previous < index; previous++ {
			if fixtures[previous].Priority == fixture.Priority && jsonObjectsCompatible(fixtures[previous].Match, fixture.Match) {
				return fmt.Errorf("MCP tool %q fixtures %q and %q are ambiguous at equal priority", toolName, fixtures[previous].ID, fixture.ID)
			}
		}
	}
	sort.SliceStable(fixtures, func(i, j int) bool { return fixtures[i].Priority > fixtures[j].Priority })
	return nil
}

func validateMCPResult(result MCPToolResult) error {
	if len(result.Content) == 0 && result.StructuredContent == nil {
		return errors.New("content or structured_content is required")
	}
	for index, content := range result.Content {
		if content.Type != "text" || content.Text == "" {
			return fmt.Errorf("content %d must be non-empty text", index)
		}
	}
	if result.StructuredContent != nil {
		if _, err := json.Marshal(result.StructuredContent); err != nil {
			return fmt.Errorf("structured_content is not JSON: %w", err)
		}
	}
	encoded, err := json.Marshal(wireMCPResult(result))
	if err != nil || len(encoded) > maxServiceRequestBody {
		return fmt.Errorf("result must encode to at most %d bytes", maxServiceRequestBody)
	}
	return nil
}

func validateMCPStatefulSchema(tool *MCPTool) error {
	properties, _ := tool.InputSchema["properties"].(map[string]any)
	requireString := func(name, role string) error {
		property, ok := properties[name].(map[string]any)
		if !ok || property["type"] != "string" {
			return fmt.Errorf("MCP stateful tool %q %s %q must be a declared string input property", tool.Name, role, name)
		}
		return nil
	}
	if tool.Behavior.KeyArgument != "" {
		if err := requireString(tool.Behavior.KeyArgument, "key_argument"); err != nil {
			return err
		}
	}
	if tool.Behavior.KeyField != "" {
		if err := requireString(tool.Behavior.KeyField, "key_field"); err != nil {
			return err
		}
	}
	seen := map[string]struct{}{}
	for _, field := range tool.Behavior.PatchFields {
		if field == "" || field == tool.Behavior.KeyArgument {
			return fmt.Errorf("MCP stateful patch tool %q has invalid patch field %q", tool.Name, field)
		}
		if _, exists := seen[field]; exists {
			return fmt.Errorf("MCP stateful patch tool %q repeats patch field %q", tool.Name, field)
		}
		if _, exists := properties[field]; !exists {
			return fmt.Errorf("MCP stateful patch tool %q patch field %q is not an input property", tool.Name, field)
		}
		seen[field] = struct{}{}
	}
	return nil
}

func loadMCPSeed(path string) (map[string]map[string]json.RawMessage, error) {
	state := map[string]map[string]json.RawMessage{}
	if path == "" {
		return state, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read MCP seed: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, fmt.Errorf("decode MCP seed: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("MCP seed must contain exactly one JSON value")
	}
	for collection, resources := range state {
		if collection == "" {
			return nil, errors.New("MCP seed contains an empty collection name")
		}
		for key, value := range resources {
			if key == "" || !json.Valid(value) || len(value) > maxServiceRequestBody {
				return nil, fmt.Errorf("MCP seed collection %q contains invalid resource %q", collection, key)
			}
			resources[key] = append(json.RawMessage(nil), value...)
		}
	}
	return state, nil
}

func validateMCPSeedRequirements(definition *MCPDefinition, state map[string]map[string]json.RawMessage, hasSeed bool) error {
	for _, tool := range definition.Tools {
		if tool.Behavior.Kind != "stateful" {
			continue
		}
		if !hasSeed {
			return fmt.Errorf("MCP stateful tool %q requires a service seed", tool.Name)
		}
		if _, exists := state[tool.Behavior.Collection]; !exists {
			return fmt.Errorf("MCP stateful tool %q references seed collection %q that does not exist", tool.Name, tool.Behavior.Collection)
		}
	}
	return nil
}

func (h *MCPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != h.endpoint {
		h.coverageGap("http", "", map[string]any{"method": request.Method, "path": request.URL.Path, "reason": "unknown MCP endpoint"})
		h.writeHTTPError(writer, http.StatusNotFound, "MCP endpoint not found")
		return
	}
	if request.Method == http.MethodGet || request.Method == http.MethodDelete {
		writer.Header().Set("Allow", http.MethodPost)
		h.writeHTTPError(writer, http.StatusMethodNotAllowed, "streaming and protocol sessions are not supported")
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		h.writeHTTPError(writer, http.StatusMethodNotAllowed, "MCP endpoint accepts POST")
		return
	}
	if request.Header.Get("Origin") != "" {
		h.writeHTTPError(writer, http.StatusForbidden, "browser-origin MCP requests are not supported")
		return
	}
	body, err := readBounded(request.Body, maxServiceRequestBody)
	if err != nil {
		h.writeRPCError(writer, http.StatusRequestEntityTooLarge, json.RawMessage("null"), -32700, err.Error(), nil)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var rpc mcpRequest
	if err := decoder.Decode(&rpc); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		if err == nil {
			err = errors.New("request contains trailing JSON")
		}
		h.writeRPCError(writer, http.StatusBadRequest, json.RawMessage("null"), -32700, "invalid JSON-RPC request", errorText(err))
		return
	}
	if rpc.JSONRPC != "2.0" || rpc.Method == "" {
		h.writeRPCError(writer, http.StatusBadRequest, responseID(rpc.ID), -32600, "invalid JSON-RPC request", nil)
		return
	}
	if len(rpc.ID) == 0 {
		h.handleNotification(writer, rpc)
		return
	}
	handleStatus := http.StatusOK
	switch rpc.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(rpc.Params, &params); err != nil || params.ProtocolVersion == "" {
			h.writeRPCError(writer, handleStatus, rpc.ID, -32602, "initialize requires protocolVersion", nil)
			return
		}
		protocol := supportedMCPProtocol(params.ProtocolVersion)
		writer.Header().Set("MCP-Protocol-Version", protocol)
		h.writeRPCResult(writer, handleStatus, rpc.ID, map[string]any{
			"protocolVersion": protocol,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "interceptor-" + h.serviceID, "version": "v1alpha1"},
		})
		h.record("mcp_initialize", map[string]any{"service": h.serviceID, "requested_protocol": params.ProtocolVersion, "protocol": protocol})
	case "ping":
		h.writeRPCResult(writer, handleStatus, rpc.ID, map[string]any{})
	case "tools/list":
		var params struct {
			Cursor string `json:"cursor,omitempty"`
		}
		if len(rpc.Params) > 0 && string(rpc.Params) != "null" {
			if err := json.Unmarshal(rpc.Params, &params); err != nil || params.Cursor != "" {
				h.coverageGap("rpc", "", map[string]any{"method": rpc.Method, "reason": "tool-list pagination is not supported"})
				h.writeRPCError(writer, handleStatus, rpc.ID, -32602, "tool-list pagination is not supported", nil)
				return
			}
		}
		tools := make([]map[string]any, 0, len(h.tools))
		for _, tool := range h.tools {
			item := map[string]any{"name": tool.Name, "description": tool.Description, "inputSchema": tool.InputSchema}
			if tool.Title != "" {
				item["title"] = tool.Title
			}
			if len(tool.OutputSchema) > 0 {
				item["outputSchema"] = tool.OutputSchema
			}
			tools = append(tools, item)
		}
		h.writeRPCResult(writer, handleStatus, rpc.ID, map[string]any{"tools": tools})
		h.record("mcp_tools_list", map[string]any{"service": h.serviceID, "count": len(tools)})
	case "tools/call":
		h.handleToolCall(writer, rpc)
	default:
		h.coverageGap("rpc", "", map[string]any{"method": rpc.Method, "reason": "unsupported MCP method"})
		h.writeRPCError(writer, handleStatus, rpc.ID, -32601, "MCP method not supported", map[string]any{"method": rpc.Method})
	}
}

func (h *MCPHandler) handleNotification(writer http.ResponseWriter, request mcpRequest) {
	if request.Method == "notifications/initialized" {
		h.record("mcp_notification", map[string]any{"service": h.serviceID, "method": request.Method})
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	h.coverageGap("notification", "", map[string]any{"method": request.Method, "reason": "unsupported MCP notification"})
	writer.WriteHeader(http.StatusAccepted)
}

func (h *MCPHandler) handleToolCall(writer http.ResponseWriter, request mcpRequest) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	decoder := json.NewDecoder(bytes.NewReader(request.Params))
	decoder.UseNumber()
	if err := decoder.Decode(&params); err != nil || params.Name == "" {
		h.writeRPCError(writer, http.StatusOK, request.ID, -32602, "tools/call requires a tool name and object arguments", nil)
		return
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}
	tool := h.byName[params.Name]
	if tool == nil {
		h.coverageGap("tool", params.Name, map[string]any{"reason": "unknown MCP tool", "arguments": params.Arguments})
		h.writeRPCError(writer, http.StatusOK, request.ID, -32602, "unknown MCP tool", map[string]any{"tool": params.Name})
		return
	}
	if err := validateMCPArguments(tool.InputSchema, params.Arguments); err != nil {
		h.record("mcp_tool_error", map[string]any{"service": h.serviceID, "tool": params.Name, "arguments": params.Arguments, "error": err.Error()})
		h.writeRPCResult(writer, http.StatusOK, request.ID, wireMCPResult(errorMCPResult(err.Error())))
		return
	}
	var result MCPToolResult
	var rule string
	var err error
	switch tool.Behavior.Kind {
	case "fixture":
		result, rule, err = h.executeFixture(tool, params.Arguments)
	case "stateful":
		result, err = h.executeStateful(tool, params.Arguments)
	}
	if err != nil {
		if errors.Is(err, errMCPFixtureGap) {
			h.coverageGap("tool", params.Name, map[string]any{"reason": err.Error(), "arguments": params.Arguments})
		} else {
			h.record("mcp_tool_error", map[string]any{"service": h.serviceID, "tool": params.Name, "arguments": params.Arguments, "error": err.Error()})
		}
		result = errorMCPResult(err.Error())
	}
	wire := wireMCPResult(result)
	if encoded, encodeErr := json.Marshal(wire); encodeErr != nil || len(encoded) > maxServiceRequestBody {
		h.record("mcp_tool_error", map[string]any{"service": h.serviceID, "tool": params.Name, "error": "tool result exceeds MCP response limit"})
		result = errorMCPResult("tool result exceeds MCP response limit")
		wire = wireMCPResult(result)
	}
	h.writeRPCResult(writer, http.StatusOK, request.ID, wire)
	h.record("mcp_tool_call", map[string]any{
		"service": h.serviceID, "tool": params.Name, "arguments": params.Arguments,
		"behavior": tool.Behavior.Kind, "fixture": rule, "is_error": result.IsError, "result": wire,
	})
}

var errMCPFixtureGap = errors.New("no sealed MCP fixture matched tool arguments")

func (h *MCPHandler) executeFixture(tool *MCPTool, arguments map[string]any) (MCPToolResult, string, error) {
	for _, fixture := range tool.Behavior.Fixtures {
		if jsonSubset(fixture.Match, arguments) {
			return fixture.Result, fixture.ID, nil
		}
	}
	return MCPToolResult{}, "", errMCPFixtureGap
}

func (h *MCPHandler) executeStateful(tool *MCPTool, arguments map[string]any) (MCPToolResult, error) {
	behavior := tool.Behavior
	mutating := behavior.Action == "create" || behavior.Action == "replace" || behavior.Action == "patch" || behavior.Action == "delete"
	if mutating {
		h.mutation.Lock()
		defer h.mutation.Unlock()
	}
	switch behavior.Action {
	case "list":
		h.mu.RLock()
		resources := h.state[behavior.Collection]
		keys := make([]string, 0, len(resources))
		for key := range resources {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		values := make([]any, 0, len(keys))
		for _, key := range keys {
			var value any
			if err := decodeJSONValue(resources[key], &value); err != nil {
				h.mu.RUnlock()
				return MCPToolResult{}, err
			}
			values = append(values, value)
		}
		h.mu.RUnlock()
		return valueMCPResult(values), nil
	case "get":
		key, err := mcpKey(arguments, behavior.KeyArgument)
		if err != nil {
			return MCPToolResult{}, err
		}
		value, exists := h.get(behavior.Collection, key)
		if !exists {
			return MCPToolResult{}, fmt.Errorf("resource %q was not found", key)
		}
		return rawMCPResult(value)
	case "delete":
		key, err := mcpKey(arguments, behavior.KeyArgument)
		if err != nil {
			return MCPToolResult{}, err
		}
		if _, exists := h.get(behavior.Collection, key); !exists {
			return MCPToolResult{}, fmt.Errorf("resource %q was not found", key)
		}
		operation := statejournal.Operation{Kind: "service_delete", Service: h.serviceID, Collection: behavior.Collection, Key: key}
		if err := h.commit("mcp_stateful_delete", []statejournal.Operation{operation}); err != nil {
			return MCPToolResult{}, err
		}
		if err := h.Apply(operation); err != nil {
			return MCPToolResult{}, err
		}
		return valueMCPResult(map[string]any{"deleted": key}), nil
	case "create", "replace", "patch":
		object := cloneJSONObject(arguments)
		var key string
		var err error
		if behavior.Action == "create" {
			key, err = mcpKey(object, behavior.KeyField)
			if err == nil {
				if _, exists := h.get(behavior.Collection, key); exists {
					err = fmt.Errorf("resource %q already exists", key)
				}
			}
		} else {
			key, err = mcpKey(object, behavior.KeyArgument)
			if err == nil {
				if _, exists := h.get(behavior.Collection, key); !exists {
					err = fmt.Errorf("resource %q was not found", key)
				}
			}
		}
		if err != nil {
			return MCPToolResult{}, err
		}
		if behavior.Action == "patch" {
			current, _ := h.get(behavior.Collection, key)
			var merged map[string]any
			if err := decodeJSONValue(current, &merged); err != nil {
				return MCPToolResult{}, err
			}
			allowed := map[string]struct{}{}
			for _, field := range behavior.PatchFields {
				allowed[field] = struct{}{}
			}
			for field, value := range object {
				if field == behavior.KeyArgument {
					continue
				}
				if _, ok := allowed[field]; !ok {
					return MCPToolResult{}, fmt.Errorf("patch field %q is not allowed", field)
				}
				merged[field] = value
			}
			object = merged
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return MCPToolResult{}, err
		}
		operation := statejournal.Operation{Kind: "service_put", Service: h.serviceID, Collection: behavior.Collection, Key: key, Value: encoded}
		if err := h.commit("mcp_stateful_"+behavior.Action, []statejournal.Operation{operation}); err != nil {
			return MCPToolResult{}, err
		}
		if err := h.Apply(operation); err != nil {
			return MCPToolResult{}, err
		}
		return rawMCPResult(encoded)
	default:
		return MCPToolResult{}, fmt.Errorf("unsupported stateful action %q", behavior.Action)
	}
}

func (h *MCPHandler) Apply(operation statejournal.Operation) error {
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

func (h *MCPHandler) SupportsCreate(collection string) bool {
	for _, tool := range h.tools {
		if tool.Behavior.Kind == "stateful" && tool.Behavior.Action == "create" && tool.Behavior.Collection == collection {
			return true
		}
	}
	return false
}

func (h *MCPHandler) ValidateCreate(operation statejournal.Operation) error {
	if operation.Kind != "service_put" || operation.Service != h.serviceID || operation.Collection == "" || operation.Key == "" || !json.Valid(operation.Value) {
		return errors.New("MCP resource creation requires a valid service_put operation")
	}
	if _, exists := h.get(operation.Collection, operation.Key); exists {
		return errors.New("MCP resource already exists")
	}
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(operation.Value))
	decoder.UseNumber()
	if decoder.Decode(&object) != nil || object == nil || decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("MCP resource creation requires exactly one JSON object")
	}
	for index := range h.tools {
		tool := &h.tools[index]
		behavior := tool.Behavior
		if behavior.Kind != "stateful" || behavior.Action != "create" || behavior.Collection != operation.Collection {
			continue
		}
		if err := validateMCPArguments(tool.InputSchema, object); err != nil {
			continue
		}
		key, err := mcpKey(object, behavior.KeyField)
		if err == nil && key == operation.Key {
			return nil
		}
	}
	return fmt.Errorf("created resource does not satisfy a sealed MCP create tool for collection %q", operation.Collection)
}

func (h *MCPHandler) Snapshot() map[string]map[string]json.RawMessage {
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

func (h *MCPHandler) get(collection, key string) (json.RawMessage, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	value, exists := h.state[collection][key]
	return append(json.RawMessage(nil), value...), exists
}

func (h *MCPHandler) commit(reason string, operations []statejournal.Operation) error {
	if h.journal == nil {
		return errors.New("state journal is unavailable")
	}
	_, err := h.journal.Append(reason, operations)
	return err
}

func (h *MCPHandler) coverageGap(surface, tool string, details map[string]any) {
	details["service"] = h.serviceID
	details["surface"] = surface
	if tool != "" {
		details["tool"] = tool
	}
	h.record("coverage_gap", details)
}

func (h *MCPHandler) record(kind string, semantic any) {
	if h.recorder == nil {
		return
	}
	_, _ = h.recorder.Append(recording.Draft{
		Channel: recording.Evaluation, Layer: "mcp_service", Kind: kind,
		Provenance: recording.EnvironmentSealed, Semantic: semantic,
	})
}

func (h *MCPHandler) writeHTTPError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]any{"code": "mcp_http_error", "message": message}})
}

func (h *MCPHandler) writeRPCResult(writer http.ResponseWriter, status int, id json.RawMessage, result any) {
	h.writeRPC(writer, status, mcpResponse{JSONRPC: "2.0", ID: responseID(id), Result: result})
}

func (h *MCPHandler) writeRPCError(writer http.ResponseWriter, status int, id json.RawMessage, code int, message string, data any) {
	h.writeRPC(writer, status, mcpResponse{JSONRPC: "2.0", ID: responseID(id), Error: &mcpError{Code: code, Message: message, Data: data}})
}

func (h *MCPHandler) writeRPC(writer http.ResponseWriter, status int, response mcpResponse) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(response)
}

func supportedMCPProtocol(requested string) string {
	switch requested {
	case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
		return requested
	default:
		return defaultMCPProtocolVersion
	}
}

func responseID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 || !json.Valid(id) {
		return json.RawMessage("null")
	}
	return id
}

func errorText(err error) any {
	if err == nil {
		return nil
	}
	return map[string]any{"detail": err.Error()}
}

func wireMCPResult(result MCPToolResult) map[string]any {
	content := result.Content
	if len(content) == 0 && result.StructuredContent != nil {
		encoded, _ := json.Marshal(result.StructuredContent)
		content = []MCPContent{{Type: "text", Text: string(encoded)}}
	}
	wire := map[string]any{"content": content, "isError": result.IsError}
	if result.StructuredContent != nil {
		wire["structuredContent"] = result.StructuredContent
	}
	return wire
}

func errorMCPResult(message string) MCPToolResult {
	return MCPToolResult{Content: []MCPContent{{Type: "text", Text: message}}, IsError: true}
}

func valueMCPResult(value any) MCPToolResult {
	encoded, _ := json.Marshal(value)
	return MCPToolResult{Content: []MCPContent{{Type: "text", Text: string(encoded)}}, StructuredContent: value}
}

func rawMCPResult(value json.RawMessage) (MCPToolResult, error) {
	var decoded any
	if err := decodeJSONValue(value, &decoded); err != nil {
		return MCPToolResult{}, err
	}
	return valueMCPResult(decoded), nil
}

func decodeJSONValue(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("value contains trailing JSON")
	}
	return nil
}

func mcpKey(arguments map[string]any, field string) (string, error) {
	value, exists := arguments[field]
	if !exists {
		return "", fmt.Errorf("argument %q is required", field)
	}
	key, ok := value.(string)
	if !ok || strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("argument %q must be a non-empty string", field)
	}
	return key, nil
}

func cloneJSONObject(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func validateMCPArguments(schema, arguments map[string]any) error {
	required, _ := stringList(schema["required"])
	for _, name := range required {
		if _, exists := arguments[name]; !exists {
			return fmt.Errorf("required argument %q is missing", name)
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	additionalAllowed := true
	if value, ok := schema["additionalProperties"].(bool); ok {
		additionalAllowed = value
	}
	for name, value := range arguments {
		rawProperty, declared := properties[name]
		if !declared {
			if !additionalAllowed {
				return fmt.Errorf("argument %q is not allowed", name)
			}
			continue
		}
		property, _ := rawProperty.(map[string]any)
		expected, _ := property["type"].(string)
		if expected != "" && !mcpJSONType(value, expected) {
			return fmt.Errorf("argument %q must have JSON type %s", name, expected)
		}
	}
	return nil
}

func mcpJSONType(value any, expected string) bool {
	switch expected {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := number.Int64()
		return err == nil
	case "null":
		return value == nil
	default:
		return true
	}
}

func stringList(value any) ([]string, bool) {
	switch values := value.(type) {
	case []string:
		return values, true
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok || text == "" {
				return nil, false
			}
			result = append(result, text)
		}
		return result, true
	default:
		return nil, false
	}
}
