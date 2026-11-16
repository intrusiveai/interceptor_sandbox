package injection

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

// GenerationCodecs is the set of application-facing protocols this surface can
// mutate. Provider selection and credentials remain outside this package.
func GenerationCodecs() []string {
	return []string{"anthropic_messages", "aws_bedrock_converse", "google_generate_content", "openai_chat_completions", "openai_responses"}
}

// ModelResponseSelection freezes membership, not armed state. Pointer identity
// prevents delete/re-arm of the same ID from targeting an older in-flight call.
type ModelResponseSelection struct {
	codec, turn string
	items       map[string]*managed
	definitions map[string]Definition
}

func (m *Manager) SelectModelResponse(codec, turn string, request []byte) (ModelResponseSelection, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(request))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return ModelResponseSelection{}, err
	}
	selection := ModelResponseSelection{codec: codec, turn: turn, items: map[string]*managed{}, definitions: map[string]Definition{}}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range m.orderedItems() {
		d := item.definition
		if d.Surface == "model_response" && d.Enabled && item.evaluation.Armed && d.Selector.ModelCodec == codec && (d.Selector.TurnID == "" || turn != "" && d.Selector.TurnID == turn) && responseRequestMatches(d.Selector, ResponseContext{Body: value}) {
			selection.items[d.ID] = item
			selection.definitions[d.ID] = d
		}
	}
	return selection, nil
}

// RewriteModelResponse deliberately validates JSON encoding/size, never the
// provider response schema. Placement failure retains existing once semantics.
func (m *Manager) RewriteModelResponse(ctx context.Context, selection ModelResponseSelection, body []byte, limit int64) ([]byte, []Result, error) {
	var candidate any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&candidate) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return body, nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	output := body
	results := []Result{}
	for _, item := range m.orderedItems() {
		d := item.definition
		if selection.items[d.ID] != item || !d.Enabled || !item.evaluation.Armed {
			continue
		}
		next := *item
		ordinal, progress := nextResponseOrdinal(&next)
		if ordinal == 0 {
			continue
		}
		if !selectorMatches(d.Selector, toolCall{ordinal: ordinal}) {
			if progress {
				if err := m.persistProgress(&next); err != nil {
					return nil, nil, err
				}
				*item = next
			}
			continue
		}
		result := Result{InjectionID: d.ID, Matched: true}
		next.evaluation.Matched = true
		updated, err := Apply(candidate, d.Placement, d.Payload)
		if err != nil {
			result.Error = err.Error()
		} else {
			encoded, err := json.Marshal(updated)
			if err != nil {
				return nil, nil, err
			}
			if int64(len(encoded)) > limit {
				return nil, nil, errors.New("model response injection exceeds response byte limit")
			}
			candidate, output = updated, encoded
			result.Applied = true
			next.evaluation.Fired = true
		}
		if d.Scope == "once" {
			err = m.persistDisabled(&next)
		} else {
			err = m.persistProgress(&next)
		}
		if err != nil {
			return nil, nil, err
		}
		*item = next
		if err := m.recordModelResponse(ctx, selection, d, "injection_applied", map[string]any{"applied": result.Applied, "error": result.Error}); err != nil {
			return nil, nil, err
		}
		results = append(results, result)
	}
	return output, results, nil
}

// RecordModelResponseWrite describes only the write to the application-facing
// transport. It does not prove SDK acceptance, tool execution or model delivery.
func (m *Manager) RecordModelResponseWrite(ctx context.Context, selection ModelResponseSelection, results []Result, written, expected int, writeErr error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := "response_written"
	if writeErr != nil || written != expected {
		state = "write_failed"
	}
	for _, result := range results {
		if !result.Applied {
			continue
		}
		d := selection.definitions[result.InjectionID]
		if item := m.items[d.ID]; item != nil && item == selection.items[d.ID] {
			next := *item
			next.evaluation.Delivery = state
			if err := m.persistProgress(&next); err != nil {
				m.recorder.Fail(err)
				return err
			}
			*item = next
		}
		if err := m.recordModelResponse(ctx, selection, d, "model_response_write", map[string]any{"write_state": state, "bytes_written": written, "expected_bytes": expected}); err != nil {
			return err
		}
	}
	if m.recorder != nil {
		return m.recorder.Flush()
	}
	return nil
}

func (m *Manager) recordModelResponse(ctx context.Context, selection ModelResponseSelection, d Definition, kind string, semantic map[string]any) error {
	if m.recorder == nil {
		return nil
	}
	semantic["surface"] = "model_response"
	semantic["direction"] = "model_to_application"
	semantic["codec"] = selection.codec
	_, err := m.recorder.AppendContext(ctx, recording.Draft{Channel: recording.Evaluation, Layer: "injection", Kind: kind, Provenance: recording.HarnessInjected, InjectionID: d.ID, AttemptID: d.AttemptID, PayloadDigest: d.PayloadDigest, TurnID: selection.turn, Semantic: semantic})
	return err
}

func validateModelResponse(definition Definition) error {
	s := definition.Selector
	if definition.Mode != "simulation" || (definition.Scope != "once" && definition.Scope != "standing") {
		return errors.New("model_response requires simulation mode and once or standing scope")
	}
	valid := false
	for _, codec := range GenerationCodecs() {
		valid = valid || s.ModelCodec == codec
	}
	if !valid {
		return fmt.Errorf("unsupported model_response codec %q", s.ModelCodec)
	}
	if s.ToolName != "" || s.Service != "" || s.Collection != "" || s.Key != "" || s.Path != "" || s.Method != "" || len(s.HeadersEqual) > 0 || len(s.QueryEqual) > 0 || s.ArgumentsPointer != "" || len(s.ArgumentsEqual) > 0 || s.ArgumentsContain != "" || s.FileNamespace != "" || s.RelativePath != "" {
		return errors.New("model_response uses codec, ordinal, observed turn and request-body selectors only")
	}
	return validateHTTPSelector(s)
}
