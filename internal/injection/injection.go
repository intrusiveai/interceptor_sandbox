package injection

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

type Definition struct {
	ID        string          `json:"id"`
	Surface   string          `json:"surface"`
	Mode      string          `json:"mode"`
	Scope     string          `json:"scope"`
	Enabled   bool            `json:"enabled"`
	Selector  Selector        `json:"selector"`
	Placement Placement       `json:"placement"`
	Payload   json.RawMessage `json:"payload"`
}

type Selector struct {
	ToolName         string          `json:"tool_name"`
	CallOrdinal      string          `json:"call_ordinal"`
	ArgumentsPointer string          `json:"arguments_pointer,omitempty"`
	ArgumentsEqual   json.RawMessage `json:"arguments_equal,omitempty"`
	ArgumentsContain string          `json:"arguments_contain,omitempty"`
	TurnID           string          `json:"turn_id,omitempty"`
	Service          string          `json:"service,omitempty"`
	Collection       string          `json:"collection,omitempty"`
	Key              string          `json:"key,omitempty"`
	Path             string          `json:"path,omitempty"`
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
	if definition.Surface != "model_tool_result" && definition.Surface != "environment_state" {
		return fmt.Errorf("unsupported injection surface %q", definition.Surface)
	}
	if definition.Surface == "model_tool_result" && definition.Mode != "simulation" || definition.Surface == "environment_state" && definition.Mode != "fidelity" {
		return fmt.Errorf("mode %q is invalid for surface %q", definition.Mode, definition.Surface)
	}
	if definition.Scope != "once" && definition.Scope != "next_turn" && definition.Scope != "standing" {
		return fmt.Errorf("unsupported injection scope %q", definition.Scope)
	}
	if len(definition.Payload) == 0 || !json.Valid(definition.Payload) {
		return errors.New("payload must be valid JSON")
	}
	if definition.Surface == "model_tool_result" {
		if definition.Selector.ToolName == "" {
			return errors.New("model_tool_result selector requires tool_name")
		}
		ordinal := definition.Selector.CallOrdinal
		if ordinal != "first" && ordinal != "every" {
			value, err := strconv.Atoi(ordinal)
			if err != nil || value <= 0 {
				return errors.New("call_ordinal must be first, every, or a positive integer")
			}
		}
	}
	if definition.Surface == "environment_state" {
		serviceTarget := definition.Selector.Service != "" || definition.Selector.Collection != "" || definition.Selector.Key != ""
		fileTarget := definition.Selector.Path != ""
		if serviceTarget == fileTarget || serviceTarget && (definition.Selector.Service == "" || definition.Selector.Collection == "" || definition.Selector.Key == "") || fileTarget && !strings.HasPrefix(definition.Selector.Path, "/") {
			return errors.New("environment_state selector requires either service/collection/key or one absolute path")
		}
	}
	if definition.Placement.Pointer != "" && !strings.HasPrefix(definition.Placement.Pointer, "/") {
		return errors.New("placement pointer must be empty or a JSON Pointer")
	}
	switch definition.Placement.Operation {
	case "replace", "append_text", "prepend_text":
	case "insert_array":
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

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, exists := m.items[id]
	if !exists {
		return fmt.Errorf("injection %q does not exist", id)
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
	for _, item := range m.items {
		result = append(result, PublicState{Definition: item.definition})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Definition.ID < result[j].Definition.ID })
	return result
}

func (m *Manager) Evaluation() []EvaluationState {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]EvaluationState, 0, len(m.items))
	for _, item := range m.items {
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
	calls := collectToolCalls(messages)
	results := []Result{}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range m.items {
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
			message := messages[call.messageIndex].(map[string]any)
			original := message["content"]
			updated, err := Apply(original, definition.Placement, definition.Payload)
			if err != nil {
				result.Error = err.Error()
				m.record("injection_placement_failed", definition.ID, map[string]any{"turn_id": turnID, "error": err.Error()})
			} else {
				message["content"] = updated
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

func (m *Manager) CompleteFidelityTurn(turnID string, confirmed []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range m.items {
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
	_, _ = m.recorder.Append(recording.Draft{Channel: recording.Evaluation, Layer: "injection", Kind: kind, Provenance: recording.HarnessInjected, InjectionID: id, Semantic: semantic})
}

type toolCall struct {
	id, name     string
	arguments    any
	ordinal      int
	messageIndex int
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
	for index, raw := range messages {
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
		call.id, call.messageIndex = id, index
		result = append(result, call)
	}
	return result
}

func selectorMatches(selector Selector, call toolCall) bool {
	if call.name != selector.ToolName {
		return false
	}
	if selector.CallOrdinal != "every" && selector.CallOrdinal != "first" {
		ordinal, _ := strconv.Atoi(selector.CallOrdinal)
		if call.ordinal != ordinal {
			return false
		}
	} else if selector.CallOrdinal == "first" && call.ordinal != 1 {
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
