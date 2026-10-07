package injection

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

type Definition struct {
	ID            string          `json:"id"`
	Surface       string          `json:"surface"`
	Mode          string          `json:"mode"`
	Scope         string          `json:"scope"`
	Enabled       bool            `json:"enabled"`
	Selector      Selector        `json:"selector"`
	Placement     Placement       `json:"placement"`
	Payload       json.RawMessage `json:"payload"`
	AttemptID     string          `json:"attempt_id,omitempty"`
	PayloadDigest string          `json:"payload_digest,omitempty"`
}

type Selector struct {
	ToolName           string            `json:"tool_name"`
	CallOrdinal        string            `json:"call_ordinal"`
	ArgumentsPointer   string            `json:"arguments_pointer,omitempty"`
	ArgumentsEqual     json.RawMessage   `json:"arguments_equal,omitempty"`
	ArgumentsContain   string            `json:"arguments_contain,omitempty"`
	TurnID             string            `json:"turn_id,omitempty"`
	Service            string            `json:"service,omitempty"`
	Collection         string            `json:"collection,omitempty"`
	Key                string            `json:"key,omitempty"`
	Path               string            `json:"path,omitempty"`
	Method             string            `json:"method,omitempty"`
	QueryEqual         map[string]string `json:"query_equal,omitempty"`
	HeadersEqual       map[string]string `json:"headers_equal,omitempty"`
	RequestBodyPointer string            `json:"request_body_pointer,omitempty"`
	RequestBodyEqual   json.RawMessage   `json:"request_body_equal,omitempty"`
	RequestBodyContain string            `json:"request_body_contain,omitempty"`
	FileNamespace      string            `json:"file_namespace,omitempty"`
	RelativePath       string            `json:"relative_path,omitempty"`
}

type Placement struct {
	Operation     string   `json:"operation"`
	Pointer       string   `json:"pointer"`
	Index         int      `json:"index,omitempty"`
	AllowedFields []string `json:"allowed_fields,omitempty"`
}

type PublicState struct {
	Definition Definition `json:"definition"`
}

type EvaluationState struct {
	ID                 string `json:"id"`
	Armed              bool   `json:"armed"`
	Matched            bool   `json:"matched"`
	Fired              bool   `json:"fired"`
	Consumed           bool   `json:"consumed"`
	Delivery           string `json:"delivery"`
	DeliveryObservable bool   `json:"delivery_observable"`
	Fingerprint        string `json:"fingerprint"`
	ResponseCandidates int    `json:"response_candidates,omitempty"`
}

type ResponseContext struct {
	Method    string
	Path      string
	Query     map[string][]string
	Headers   map[string][]string
	Body      any
	Arguments any
}

type OperatorState struct {
	Definition Definition      `json:"definition"`
	Evaluation EvaluationState `json:"evaluation"`
}

type Result struct {
	InjectionID string `json:"injection_id"`
	Matched     bool   `json:"matched"`
	Applied     bool   `json:"applied"`
	Error       string `json:"error,omitempty"`
}

type managed struct {
	definition   Definition
	evaluation   EvaluationState
	eligibleTurn string
}

type Manager struct {
	mu       sync.Mutex
	items    map[string]*managed
	journal  *statejournal.Writer
	recorder *recording.Writer
}

func NewManager(journal *statejournal.Writer, recorder *recording.Writer) *Manager {
	return &Manager{items: map[string]*managed{}, journal: journal, recorder: recorder}
}

func Validate(definition Definition) error {
	if definition.ID == "" || strings.ContainsAny(definition.ID, "/\\") {
		return errors.New("injection id is required and may not contain path separators")
	}
	if definition.Surface != "model_tool_result" && definition.Surface != "environment_state" && definition.Surface != "service_response" && definition.Surface != "mcp_tool_result" {
		return fmt.Errorf("unsupported injection surface %q", definition.Surface)
	}
	if (definition.Surface == "model_tool_result" || definition.Surface == "service_response" || definition.Surface == "mcp_tool_result") && definition.Mode != "simulation" || definition.Surface == "environment_state" && definition.Mode != "fidelity" {
		return fmt.Errorf("mode %q is invalid for surface %q", definition.Mode, definition.Surface)
	}
	if definition.Scope != "once" && definition.Scope != "next_turn" && definition.Scope != "standing" {
		return fmt.Errorf("unsupported injection scope %q", definition.Scope)
	}
	if (definition.Surface == "service_response" || definition.Surface == "mcp_tool_result") && definition.Scope == "next_turn" {
		return errors.New("response injection cannot use next_turn because managed service requests have no trusted turn binding")
	}
	if len(definition.Payload) == 0 || !json.Valid(definition.Payload) {
		return errors.New("payload must be valid JSON")
	}
	if definition.Surface == "model_tool_result" || definition.Surface == "mcp_tool_result" || definition.Surface == "service_response" {
		if definition.Surface != "model_tool_result" && definition.Selector.TurnID != "" {
			return errors.New("response injection cannot select an untrusted turn ID")
		}
		if definition.Surface != "service_response" && definition.Selector.ToolName == "" {
			return errors.New("tool-result selector requires tool_name")
		}
		if definition.Surface != "model_tool_result" && definition.Selector.Service == "" {
			return errors.New("response selector requires stable service ID")
		}
		if definition.Surface == "service_response" && definition.Selector.ToolName != "" {
			return errors.New("service response selector may not name a tool")
		}
		ordinal := definition.Selector.CallOrdinal
		if ordinal != "first" && ordinal != "every" {
			value, err := strconv.Atoi(ordinal)
			if err != nil || value <= 0 {
				return errors.New("call_ordinal must be first, every, or a positive integer")
			}
		}
	}
	if definition.Surface == "service_response" {
		if err := validateHTTPSelector(definition.Selector); err != nil {
			return err
		}
	} else if definition.Surface != "environment_state" && (definition.Selector.Path != "" || definition.Selector.Method != "" || len(definition.Selector.QueryEqual) > 0 || len(definition.Selector.HeadersEqual) > 0 || definition.Selector.RequestBodyPointer != "" || len(definition.Selector.RequestBodyEqual) > 0 || definition.Selector.RequestBodyContain != "") {
		return errors.New("HTTP request predicates are valid only for service_response")
	}
	if definition.Surface != "environment_state" && (definition.Selector.Collection != "" || definition.Selector.Key != "") {
		return errors.New("collection and key selectors are valid only for environment_state")
	}
	if definition.Surface == "model_tool_result" && definition.Selector.Service != "" {
		return errors.New("model tool-result selector may not name a service")
	}
	if (definition.AttemptID == "") != (definition.PayloadDigest == "") || definition.PayloadDigest != "" && !digest.Valid(definition.PayloadDigest) {
		return errors.New("attempt_id and valid payload_digest must be supplied together")
	}
	if definition.Surface == "environment_state" {
		serviceTarget := definition.Selector.Service != "" || definition.Selector.Collection != "" || definition.Selector.Key != ""
		fileTarget := definition.Selector.Path != ""
		namespaceTarget := definition.Selector.FileNamespace != "" || definition.Selector.RelativePath != ""
		if boolCount(serviceTarget, fileTarget, namespaceTarget) != 1 || serviceTarget && (definition.Selector.Service == "" || definition.Selector.Collection == "" || definition.Selector.Key == "") || fileTarget && !strings.HasPrefix(definition.Selector.Path, "/") || namespaceTarget && (definition.Selector.FileNamespace == "" || !validRelativePath(definition.Selector.RelativePath)) {
			return errors.New("environment_state selector requires service/collection/key, one absolute path, or file_namespace/relative_path")
		}
	} else if definition.Selector.FileNamespace != "" || definition.Selector.RelativePath != "" {
		return errors.New("file namespace selectors are valid only for environment_state")
	}
	if definition.Placement.Pointer != "" && !strings.HasPrefix(definition.Placement.Pointer, "/") {
		return errors.New("placement pointer must be empty or a JSON Pointer")
	}
	switch definition.Placement.Operation {
	case "replace", "append_text", "prepend_text":
	case "insert_array":
		if definition.Surface == "service_response" || definition.Surface == "mcp_tool_result" {
			return errors.New("response injection does not support insert_array")
		}
		if definition.Placement.Index < 0 || definition.Placement.Index > 10000 {
			return errors.New("array insertion index is out of bounds")
		}
	case "merge_object":
		if len(definition.Placement.AllowedFields) == 0 {
			return errors.New("object merge requires allowed_fields")
		}
	default:
		return fmt.Errorf("unsupported placement operation %q", definition.Placement.Operation)
	}
	return nil
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func validRelativePath(value string) bool {
	return value != "" && !strings.HasPrefix(value, "/") && path.Clean(value) == value && value != "." && value != ".." && !strings.HasPrefix(value, "../") && !strings.ContainsAny(value, "\\\x00\r\n")
}

func validateHTTPSelector(selector Selector) error {
	if selector.Method != "" {
		if len(selector.Method) > 16 || strings.ToUpper(selector.Method) != selector.Method {
			return errors.New("HTTP selector method must be an uppercase token")
		}
		for _, value := range selector.Method {
			if value < 'A' || value > 'Z' {
				return errors.New("HTTP selector method must contain only A-Z")
			}
		}
	}
	if selector.Path != "" && (!strings.HasPrefix(selector.Path, "/") || len(selector.Path) > 2048 || strings.ContainsAny(selector.Path, "\x00\r\n?#")) {
		return errors.New("HTTP selector path must be a clean absolute request path")
	}
	if len(selector.QueryEqual) > 16 || len(selector.HeadersEqual) > 16 {
		return errors.New("HTTP selectors permit at most 16 query and 16 header predicates")
	}
	for name, value := range selector.QueryEqual {
		if name == "" || len(name) > 128 || len(value) > 1024 || strings.ContainsAny(name+value, "\x00\r\n") {
			return errors.New("HTTP query predicate is invalid")
		}
	}
	for name, value := range selector.HeadersEqual {
		canonical := httpHeaderName(name)
		if canonical == "" || len(name) > 128 || len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("HTTP header predicate is invalid")
		}
		switch canonical {
		case "Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie":
			return fmt.Errorf("HTTP selector may not inspect sensitive header %q", canonical)
		}
	}
	if selector.RequestBodyPointer != "" && !strings.HasPrefix(selector.RequestBodyPointer, "/") {
		return errors.New("request_body_pointer must be a JSON Pointer")
	}
	if len(selector.RequestBodyEqual) > 0 && !json.Valid(selector.RequestBodyEqual) {
		return errors.New("request_body_equal must be valid JSON")
	}
	if len(selector.RequestBodyContain) > 4096 {
		return errors.New("request_body_contain exceeds 4096 bytes")
	}
	return nil
}

func httpHeaderName(value string) string {
	parts := strings.Split(strings.ToLower(value), "-")
	for index, part := range parts {
		if part == "" {
			return ""
		}
		for _, character := range part {
			if character < 'a' || character > 'z' && (character < '0' || character > '9') {
				return ""
			}
		}
		parts[index] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, "-")
}

// RewriteResponse applies simulation-mode ingress mutation after a successful
// managed service or native MCP operation and before the application sees it.
func (m *Manager) RewriteResponse(surface, service, tool string, request ResponseContext, candidate any) (any, []Result, error) {
	if surface != "service_response" && surface != "mcp_tool_result" {
		return nil, nil, errors.New("invalid response-injection surface")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	updated := candidate
	results := []Result{}
	for _, item := range m.orderedItems() {
		definition := item.definition
		if !item.evaluation.Armed || !definition.Enabled || definition.Surface != surface || definition.Selector.Service != service || !responseRequestMatches(definition.Selector, request) {
			continue
		}
		if !selectorTargetMatches(definition.Selector, toolCall{name: tool, arguments: request.Arguments}) {
			continue
		}
		ordinal, needsProgress := nextResponseOrdinal(item)
		if ordinal == 0 {
			continue
		}
		call := toolCall{name: tool, arguments: request.Arguments, ordinal: ordinal}
		if !selectorMatches(definition.Selector, call) {
			if needsProgress {
				if err := m.persistProgress(item); err != nil {
					return nil, nil, err
				}
			}
			continue
		}
		result := Result{InjectionID: definition.ID, Matched: true}
		item.evaluation.Matched = true
		value, err := Apply(updated, definition.Placement, definition.Payload)
		if err != nil {
			result.Error = err.Error()
		} else {
			updated = value
			result.Applied = true
			item.evaluation.Fired = true
		}
		results = append(results, result)
		if definition.Scope != "standing" {
			if err := m.persistDisabled(item); err != nil {
				return nil, nil, err
			}
		} else {
			if err := m.persistProgress(item); err != nil {
				return nil, nil, err
			}
		}
		m.record("injection_applied", definition.ID, map[string]any{"surface": surface, "service": service, "tool_name": tool, "method": request.Method, "path": request.Path, "applied": result.Applied, "error": result.Error})
	}
	return updated, results, nil
}

func nextResponseOrdinal(item *managed) (int, bool) {
	ordinal := item.definition.Selector.CallOrdinal
	if ordinal == "every" {
		return 1, false
	}
	wanted := 1
	if ordinal != "first" {
		wanted, _ = strconv.Atoi(ordinal)
	}
	if item.evaluation.ResponseCandidates >= wanted {
		return 0, false
	}
	item.evaluation.ResponseCandidates++
	return item.evaluation.ResponseCandidates, true
}

func responseRequestMatches(selector Selector, request ResponseContext) bool {
	if selector.Path != "" && selector.Path != request.Path || selector.Method != "" && selector.Method != request.Method {
		return false
	}
	for name, expected := range selector.QueryEqual {
		values := request.Query[name]
		if len(values) != 1 || values[0] != expected {
			return false
		}
	}
	for name, expected := range selector.HeadersEqual {
		values := request.Headers[httpHeaderName(name)]
		if len(values) != 1 || values[0] != expected {
			return false
		}
	}
	if selector.RequestBodyPointer == "" && len(selector.RequestBodyEqual) == 0 && selector.RequestBodyContain == "" {
		return true
	}
	value := request.Body
	if selector.RequestBodyPointer != "" {
		var err error
		value, err = pointerGet(value, selector.RequestBodyPointer)
		if err != nil {
			return false
		}
	}
	if len(selector.RequestBodyEqual) > 0 {
		encoded, _ := json.Marshal(value)
		if !bytes.Equal(encoded, selector.RequestBodyEqual) {
			return false
		}
	}
	if selector.RequestBodyContain != "" {
		encoded, _ := json.Marshal(value)
		text := string(encoded)
		if stringValue, ok := value.(string); ok {
			text = stringValue
		}
		if !strings.Contains(text, selector.RequestBodyContain) {
			return false
		}
	}
	return true
}

func (m *Manager) persistProgress(item *managed) error {
	encoded, _ := json.Marshal(struct {
		Definition   Definition      `json:"definition"`
		EligibleTurn string          `json:"eligible_turn,omitempty"`
		Evaluation   EvaluationState `json:"evaluation"`
	}{item.definition, item.eligibleTurn, item.evaluation})
	_, err := m.journal.Append("injection_progress", []statejournal.Operation{{Kind: "injection_put", ID: item.definition.ID, Value: encoded}})
	return err
}

func (m *Manager) Arm(definition Definition, eligibleTurn string) error {
	return m.ArmTransaction(definition, eligibleTurn, nil, nil)
}

func (m *Manager) ArmTransaction(definition Definition, eligibleTurn string, extra []statejournal.Operation, apply func() error) error {
	if err := Validate(definition); err != nil {
		return err
	}
	if definition.Scope == "next_turn" && eligibleTurn == "" {
		return errors.New("next_turn injection requires an eligible turn ID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	encoded, _ := json.Marshal(struct {
		Definition   Definition      `json:"definition"`
		EligibleTurn string          `json:"eligible_turn,omitempty"`
		Evaluation   EvaluationState `json:"evaluation"`
	}{definition, eligibleTurn, EvaluationState{ID: definition.ID, Armed: definition.Enabled, Delivery: "not_confirmed", DeliveryObservable: true, Fingerprint: digest.Bytes(definition.Payload)}})
	if m.journal == nil {
		return errors.New("state journal is unavailable")
	}
	operations := append([]statejournal.Operation{{Kind: "injection_put", ID: definition.ID, Value: encoded}}, extra...)
	if _, err := m.journal.Append("injection_arm", operations); err != nil {
		return err
	}
	m.items[definition.ID] = &managed{definition: definition, eligibleTurn: eligibleTurn, evaluation: EvaluationState{
		ID: definition.ID, Armed: definition.Enabled, Delivery: "not_confirmed", DeliveryObservable: true, Fingerprint: digest.Bytes(definition.Payload),
	}}
	if apply != nil {
		if err := apply(); err != nil {
			return fmt.Errorf("apply committed fidelity mutation: %w", err)
		}
	}
	m.record("injection_armed", definition.ID, map[string]any{"surface": definition.Surface, "mode": definition.Mode, "scope": definition.Scope})
	return nil
}

// ErrNotFound identifies confirmed absence, distinct from a failed durable deletion.
var ErrNotFound = errors.New("injection does not exist")

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, exists := m.items[id]
	if !exists {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if _, err := m.journal.Append("injection_delete", []statejournal.Operation{{Kind: "injection_delete", ID: id}}); err != nil {
		return err
	}
	delete(m.items, id)
	m.record("injection_deleted", id, map[string]any{})
	return nil
}

func (m *Manager) ListPublic() []PublicState {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]PublicState, 0, len(m.items))
	for _, item := range m.orderedItems() {
		result = append(result, PublicState{Definition: item.definition})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Definition.ID < result[j].Definition.ID })
	return result
}

func (m *Manager) Evaluation() []EvaluationState {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]EvaluationState, 0, len(m.items))
	for _, item := range m.orderedItems() {
		result = append(result, item.evaluation)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (m *Manager) OperatorEvaluation() []OperatorState {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]OperatorState, 0, len(m.items))
	for _, item := range m.items {
		result = append(result, OperatorState{Definition: item.definition, Evaluation: item.evaluation})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Definition.ID < result[j].Definition.ID })
	return result
}

func (m *Manager) Attribution(ids []string) (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	attempt, payload := "", ""
	for _, id := range ids {
		item := m.items[id]
		if item == nil || item.definition.AttemptID == "" {
			continue
		}
		if attempt != "" && (attempt != item.definition.AttemptID || payload != item.definition.PayloadDigest) {
			return "", ""
		}
		attempt, payload = item.definition.AttemptID, item.definition.PayloadDigest
	}
	return attempt, payload
}

func (m *Manager) RewriteOpenAI(request []byte, turnID string) ([]byte, []Result, error) {
	var envelope map[string]any
	decoder := json.NewDecoder(bytes.NewReader(request))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, nil, fmt.Errorf("decode model request: %w", err)
	}
	messages, ok := envelope["messages"].([]any)
	if !ok {
		return nil, nil, errors.New("model request messages must be an array")
	}
	return m.rewriteToolResults(envelope, collectToolCalls(messages), turnID)
}

func (m *Manager) RewriteOpenAIResponses(request []byte, turnID string) ([]byte, []Result, error) {
	envelope, err := decodeEnvelope(request)
	if err != nil {
		return nil, nil, err
	}
	input, ok := envelope["input"].([]any)
	if !ok {
		// A string input cannot contain a completed function call, so it is a
		// valid Responses request with nothing for this injection surface to do.
		if _, stringInput := envelope["input"].(string); stringInput {
			return request, nil, nil
		}
		return nil, nil, errors.New("Responses input must be a string or array")
	}
	return m.rewriteToolResults(envelope, collectResponsesToolCalls(input), turnID)
}

func (m *Manager) RewriteBedrockConverse(request []byte, turnID string) ([]byte, []Result, error) {
	envelope, err := decodeEnvelope(request)
	if err != nil {
		return nil, nil, err
	}
	messages, ok := envelope["messages"].([]any)
	if !ok {
		if _, exists := envelope["messages"]; !exists {
			return request, nil, nil
		}
		return nil, nil, errors.New("Bedrock Converse messages must be an array")
	}
	return m.rewriteToolResults(envelope, collectBedrockToolCalls(messages), turnID)
}

func (m *Manager) RewriteAnthropicMessages(request []byte, turnID string) ([]byte, []Result, error) {
	envelope, err := decodeEnvelope(request)
	if err != nil {
		return nil, nil, err
	}
	messages, ok := envelope["messages"].([]any)
	if !ok {
		return request, nil, nil
	}
	return m.rewriteToolResults(envelope, collectAnthropicToolCalls(messages), turnID)
}

func (m *Manager) RewriteGoogleGenerateContent(request []byte, turnID string) ([]byte, []Result, error) {
	envelope, err := decodeEnvelope(request)
	if err != nil {
		return nil, nil, err
	}
	contents, ok := envelope["contents"].([]any)
	if !ok {
		return request, nil, nil
	}
	return m.rewriteToolResults(envelope, collectGoogleToolCalls(contents), turnID)
}

func decodeEnvelope(request []byte) (map[string]any, error) {
	var envelope map[string]any
	decoder := json.NewDecoder(bytes.NewReader(request))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode model request: %w", err)
	}
	return envelope, nil
}

func (m *Manager) rewriteToolResults(envelope map[string]any, calls []toolCall, turnID string) ([]byte, []Result, error) {
	results := []Result{}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range m.orderedItems() {
		definition := item.definition
		if !item.evaluation.Armed || !definition.Enabled || definition.Surface != "model_tool_result" || definition.Selector.TurnID != "" && definition.Selector.TurnID != turnID {
			continue
		}
		if definition.Scope == "next_turn" && item.eligibleTurn != turnID {
			continue
		}
		result := Result{InjectionID: definition.ID}
		for _, call := range calls {
			if !selectorMatches(definition.Selector, call) {
				continue
			}
			result.Matched, item.evaluation.Matched = true, true
			if definition.Scope != "standing" {
				if err := m.persistDisabled(item); err != nil {
					return nil, nil, fmt.Errorf("persist injection consumption: %w", err)
				}
			}
			original := call.target[call.targetField]
			updated, err := Apply(original, definition.Placement, definition.Payload)
			if err != nil {
				result.Error = err.Error()
				m.record("injection_placement_failed", definition.ID, map[string]any{"turn_id": turnID, "error": err.Error()})
			} else {
				call.target[call.targetField] = updated
				result.Applied, item.evaluation.Fired = true, true
				m.record("injection_applied", definition.ID, map[string]any{"turn_id": turnID, "tool_name": call.name, "tool_call_id": call.id})
			}
			if definition.Scope != "standing" || definition.Selector.CallOrdinal != "every" {
				break
			}
		}
		if definition.Scope == "next_turn" && item.evaluation.Armed {
			if err := m.persistDisabled(item); err != nil {
				return nil, nil, fmt.Errorf("persist next-turn injection expiration: %w", err)
			}
		}
		if result.Matched || definition.Scope == "next_turn" {
			results = append(results, result)
		}
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, nil, err
	}
	return encoded, results, nil
}

func (m *Manager) persistDisabled(item *managed) error {
	definition := item.definition
	definition.Enabled = false
	evaluation := item.evaluation
	evaluation.Armed, evaluation.Consumed = false, true
	encoded, _ := json.Marshal(struct {
		Definition   Definition      `json:"definition"`
		EligibleTurn string          `json:"eligible_turn,omitempty"`
		Evaluation   EvaluationState `json:"evaluation"`
	}{definition, item.eligibleTurn, evaluation})
	if _, err := m.journal.Append("injection_consume", []statejournal.Operation{{Kind: "injection_put", ID: definition.ID, Value: encoded}}); err != nil {
		return err
	}
	item.definition = definition
	item.evaluation = evaluation
	return nil
}

func (m *Manager) ConfirmDelivery(finalProviderRequest []byte, ids []string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	confirmed := []string{}
	operations := []statejournal.Operation{}
	for _, id := range ids {
		item := m.items[id]
		if item == nil {
			continue
		}
		var payload any
		_ = json.Unmarshal(item.definition.Payload, &payload)
		candidates := [][]byte{item.definition.Payload}
		if text, ok := payload.(string); ok {
			candidates = append(candidates, []byte(text))
		}
		for _, candidate := range candidates {
			if len(candidate) > 0 && bytes.Contains(finalProviderRequest, candidate) {
				item.evaluation.Delivery = "confirmed"
				confirmed = append(confirmed, id)
				m.record("injection_delivered", id, map[string]any{"surface": item.definition.Surface, "mode": item.definition.Mode})
				break
			}
		}
		encoded, _ := json.Marshal(struct {
			Definition   Definition      `json:"definition"`
			EligibleTurn string          `json:"eligible_turn,omitempty"`
			Evaluation   EvaluationState `json:"evaluation"`
		}{item.definition, item.eligibleTurn, item.evaluation})
		operations = append(operations, statejournal.Operation{Kind: "injection_put", ID: id, Value: encoded})
	}
	if len(operations) > 0 {
		if _, err := m.journal.Append("injection_delivery", operations); err != nil {
			return nil, err
		}
	}
	return confirmed, nil
}

func (m *Manager) ObservableFidelityIDs(turnID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := []string{}
	for id, item := range m.items {
		if item.definition.Enabled && item.definition.Mode == "fidelity" && item.evaluation.DeliveryObservable && (item.definition.Scope != "next_turn" || item.eligibleTurn == turnID) {
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result
}

// ObservableResponseIDs returns only successfully applied ingress injections.
// Consumed once injections remain candidates: application consumption precedes
// provider delivery. Fired/consumed state is already journaled and replayable.
func (m *Manager) ObservableResponseIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := []string{}
	for _, item := range m.orderedItems() {
		if (item.definition.Surface == "service_response" || item.definition.Surface == "mcp_tool_result") && item.evaluation.Fired && item.evaluation.DeliveryObservable {
			result = append(result, item.definition.ID)
		}
	}
	return result
}

func (m *Manager) CompleteFidelityTurn(turnID string, confirmed []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range m.orderedItems() {
		if !item.definition.Enabled || item.definition.Mode != "fidelity" {
			continue
		}
		consume := item.definition.Scope == "next_turn" && item.eligibleTurn == turnID || item.definition.Scope == "once" && containsID(confirmed, item.definition.ID)
		if consume {
			if err := m.persistDisabled(item); err != nil {
				return err
			}
		}
	}
	return nil
}

func containsID(values []string, id string) bool {
	for _, value := range values {
		if value == id {
			return true
		}
	}
	return false
}

// orderedItems is called only while m.mu is held. Stable ID order makes the
// result deterministic when more than one standing injection matches.
func (m *Manager) orderedItems() []*managed {
	ids := make([]string, 0, len(m.items))
	for id := range m.items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]*managed, 0, len(ids))
	for _, id := range ids {
		items = append(items, m.items[id])
	}
	return items
}

func (m *Manager) ApplyJournal(operation statejournal.Operation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch operation.Kind {
	case "injection_put":
		var value struct {
			Definition   Definition      `json:"definition"`
			EligibleTurn string          `json:"eligible_turn,omitempty"`
			Evaluation   EvaluationState `json:"evaluation"`
		}
		if err := json.Unmarshal(operation.Value, &value); err != nil {
			return err
		}
		if err := Validate(value.Definition); err != nil {
			return err
		}
		if value.Evaluation.ID == "" {
			value.Evaluation = EvaluationState{ID: value.Definition.ID, Armed: value.Definition.Enabled, Delivery: "not_confirmed", DeliveryObservable: true, Fingerprint: digest.Bytes(value.Definition.Payload)}
		}
		m.items[value.Definition.ID] = &managed{definition: value.Definition, eligibleTurn: value.EligibleTurn, evaluation: value.Evaluation}
	case "injection_delete":
		delete(m.items, operation.ID)
	default:
		return fmt.Errorf("unsupported injection operation %q", operation.Kind)
	}
	return nil
}

func (m *Manager) record(kind, id string, semantic map[string]any) {
	if m.recorder == nil {
		return
	}
	item := m.items[id]
	draft := recording.Draft{Channel: recording.Evaluation, Layer: "injection", Kind: kind, Provenance: recording.HarnessInjected, InjectionID: id, Semantic: semantic}
	if item != nil {
		draft.AttemptID = item.definition.AttemptID
		draft.PayloadDigest = item.definition.PayloadDigest
	}
	_, _ = m.recorder.Append(draft)
}

type toolCall struct {
	id, name    string
	arguments   any
	ordinal     int
	target      map[string]any
	targetField string
}

func collectToolCalls(messages []any) []toolCall {
	metadata := map[string]toolCall{}
	counts := map[string]int{}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		calls, _ := message["tool_calls"].([]any)
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			function, _ := call["function"].(map[string]any)
			name, _ := function["name"].(string)
			id, _ := call["id"].(string)
			arguments := function["arguments"]
			if text, ok := arguments.(string); ok {
				var decoded any
				if json.Unmarshal([]byte(text), &decoded) == nil {
					arguments = decoded
				}
			}
			counts[name]++
			metadata[id] = toolCall{id: id, name: name, arguments: arguments, ordinal: counts[name]}
		}
	}
	result := []toolCall{}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || message["role"] != "tool" {
			continue
		}
		id, _ := message["tool_call_id"].(string)
		call := metadata[id]
		if call.name == "" {
			call.name, _ = message["name"].(string)
			counts[call.name]++
			call.ordinal = counts[call.name]
		}
		call.id, call.target, call.targetField = id, message, "content"
		result = append(result, call)
	}
	return result
}

func collectResponsesToolCalls(items []any) []toolCall {
	metadata := map[string]toolCall{}
	counts := map[string]int{}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] != "function_call" {
			continue
		}
		id, _ := item["call_id"].(string)
		name, _ := item["name"].(string)
		arguments := decodeArguments(item["arguments"])
		counts[name]++
		metadata[id] = toolCall{id: id, name: name, arguments: arguments, ordinal: counts[name]}
	}
	result := []toolCall{}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] != "function_call_output" {
			continue
		}
		id, _ := item["call_id"].(string)
		call := metadata[id]
		call.id, call.target, call.targetField = id, item, "output"
		result = append(result, call)
	}
	return result
}

func collectBedrockToolCalls(messages []any) []toolCall {
	metadata := map[string]toolCall{}
	counts := map[string]int{}
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		content, _ := message["content"].([]any)
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			use, _ := block["toolUse"].(map[string]any)
			if use == nil {
				continue
			}
			id, _ := use["toolUseId"].(string)
			name, _ := use["name"].(string)
			counts[name]++
			metadata[id] = toolCall{id: id, name: name, arguments: use["input"], ordinal: counts[name]}
		}
	}
	result := []toolCall{}
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		content, _ := message["content"].([]any)
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			toolResult, _ := block["toolResult"].(map[string]any)
			if toolResult == nil {
				continue
			}
			id, _ := toolResult["toolUseId"].(string)
			parts, _ := toolResult["content"].([]any)
			for _, rawPart := range parts {
				part, _ := rawPart.(map[string]any)
				field := ""
				if _, exists := part["text"]; exists {
					field = "text"
				} else if _, exists := part["json"]; exists {
					field = "json"
				}
				if field != "" {
					call := metadata[id]
					call.id, call.target, call.targetField = id, part, field
					result = append(result, call)
					break
				}
			}
		}
	}
	return result
}

func collectAnthropicToolCalls(messages []any) []toolCall {
	metadata := map[string]toolCall{}
	counts := map[string]int{}
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		content, _ := message["content"].([]any)
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			if block["type"] != "tool_use" {
				continue
			}
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			counts[name]++
			metadata[id] = toolCall{id: id, name: name, arguments: block["input"], ordinal: counts[name]}
		}
	}
	result := []toolCall{}
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		content, _ := message["content"].([]any)
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			if block["type"] != "tool_result" {
				continue
			}
			id, _ := block["tool_use_id"].(string)
			call := metadata[id]
			call.target = block
			call.targetField = "content"
			result = append(result, call)
		}
	}
	return result
}

func collectGoogleToolCalls(contents []any) []toolCall {
	counts := map[string]int{}
	pending := map[string][]toolCall{}
	for _, raw := range contents {
		content, _ := raw.(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			function, _ := part["functionCall"].(map[string]any)
			if function == nil {
				continue
			}
			name, _ := function["name"].(string)
			counts[name]++
			pending[name] = append(pending[name], toolCall{name: name, arguments: function["args"], ordinal: counts[name]})
		}
	}
	result := []toolCall{}
	for _, raw := range contents {
		content, _ := raw.(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			response, _ := part["functionResponse"].(map[string]any)
			if response == nil {
				continue
			}
			name, _ := response["name"].(string)
			queue := pending[name]
			call := toolCall{name: name, ordinal: 1}
			if len(queue) > 0 {
				call = queue[0]
				pending[name] = queue[1:]
			}
			call.target = response
			call.targetField = "response"
			result = append(result, call)
		}
	}
	return result
}

func decodeArguments(raw any) any {
	if text, ok := raw.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) == nil {
			return decoded
		}
	}
	return raw
}

func selectorMatches(selector Selector, call toolCall) bool {
	return selectorTargetMatches(selector, call) && selectorOrdinalMatches(selector, call.ordinal)
}

func selectorTargetMatches(selector Selector, call toolCall) bool {
	if call.name != selector.ToolName {
		return false
	}
	if selector.ArgumentsPointer == "" {
		return len(selector.ArgumentsEqual) == 0 && selector.ArgumentsContain == ""
	}
	value, err := pointerGet(call.arguments, selector.ArgumentsPointer)
	if err != nil {
		return false
	}
	if len(selector.ArgumentsEqual) > 0 {
		encoded, _ := json.Marshal(value)
		if !bytes.Equal(encoded, selector.ArgumentsEqual) {
			return false
		}
	}
	if selector.ArgumentsContain != "" && !strings.Contains(fmt.Sprint(value), selector.ArgumentsContain) {
		return false
	}
	return true
}

func selectorOrdinalMatches(selector Selector, callOrdinal int) bool {
	if selector.CallOrdinal == "every" {
		return true
	}
	if selector.CallOrdinal == "first" {
		return callOrdinal == 1
	}
	ordinal, _ := strconv.Atoi(selector.CallOrdinal)
	return callOrdinal == ordinal
}
