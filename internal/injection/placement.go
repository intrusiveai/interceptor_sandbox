package injection

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func Apply(original any, placement Placement, rawPayload json.RawMessage) (any, error) {
	var payload any
	decoder := json.NewDecoder(strings.NewReader(string(rawPayload)))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	root := cloneJSON(original)
	if text, ok := root.(string); ok && placement.Pointer != "" {
		var decoded any
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		if decoder.Decode(&decoded) == nil {
			updated, err := applyAtPointer(decoded, placement, payload)
			if err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(updated)
			return string(encoded), err
		}
	}
	return applyAtPointer(root, placement, payload)
}

func applyAtPointer(root any, placement Placement, payload any) (any, error) {
	if placement.Pointer == "" {
		return place(root, placement, payload)
	}
	tokens, err := pointerTokens(placement.Pointer)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return place(root, placement, payload)
	}
	parent, err := pointerGet(root, "/"+strings.Join(tokens[:len(tokens)-1], "/"))
	if len(tokens) == 1 {
		parent = root
		err = nil
	}
	if err != nil {
		return nil, err
	}
	key := unescape(tokens[len(tokens)-1])
	switch value := parent.(type) {
	case map[string]any:
		current, exists := value[key]
		if !exists {
			return nil, fmt.Errorf("placement target %s is absent", placement.Pointer)
		}
		updated, err := place(current, placement, payload)
		if err != nil {
			return nil, err
		}
		value[key] = updated
	case []any:
		index, err := strconv.Atoi(key)
		if err != nil || index < 0 || index >= len(value) {
			return nil, fmt.Errorf("placement array target %s is absent", placement.Pointer)
		}
		updated, err := place(value[index], placement, payload)
		if err != nil {
			return nil, err
		}
		value[index] = updated
	default:
		return nil, errors.New("placement parent is not an object or array")
	}
	return root, nil
}

func place(current any, placement Placement, payload any) (any, error) {
	switch placement.Operation {
	case "replace":
		return cloneJSON(payload), nil
	case "append_text", "prepend_text":
		text, ok := current.(string)
		if !ok {
			return nil, errors.New("text placement target is not a string")
		}
		addition, ok := payload.(string)
		if !ok {
			return nil, errors.New("text placement payload is not a string")
		}
		if placement.Operation == "prepend_text" {
			return addition + text, nil
		}
		return text + addition, nil
	case "insert_array":
		array, ok := current.([]any)
		if !ok {
			return nil, errors.New("array placement target is not an array")
		}
		if placement.Index < 0 || placement.Index > len(array) {
			return nil, errors.New("array placement index is out of range")
		}
		array = append(array, nil)
		copy(array[placement.Index+1:], array[placement.Index:])
		array[placement.Index] = cloneJSON(payload)
		return array, nil
	case "merge_object":
		object, ok := current.(map[string]any)
		if !ok {
			return nil, errors.New("merge placement target is not an object")
		}
		addition, ok := payload.(map[string]any)
		if !ok {
			return nil, errors.New("merge placement payload is not an object")
		}
		allowed := map[string]struct{}{}
		for _, field := range placement.AllowedFields {
			allowed[field] = struct{}{}
		}
		for field, value := range addition {
			if _, exists := allowed[field]; !exists {
				return nil, fmt.Errorf("merge field %q is not allowed", field)
			}
			object[field] = cloneJSON(value)
		}
		return object, nil
	default:
		return nil, fmt.Errorf("unsupported placement operation %q", placement.Operation)
	}
}

func pointerGet(root any, pointer string) (any, error) {
	if pointer == "" {
		return root, nil
	}
	tokens, err := pointerTokens(pointer)
	if err != nil {
		return nil, err
	}
	current := root
	for _, token := range tokens {
		key := unescape(token)
		switch value := current.(type) {
		case map[string]any:
			var exists bool
			current, exists = value[key]
			if !exists {
				return nil, fmt.Errorf("JSON Pointer %s is absent", pointer)
			}
		case []any:
			index, err := strconv.Atoi(key)
			if err != nil || index < 0 || index >= len(value) {
				return nil, fmt.Errorf("JSON Pointer %s has invalid array index", pointer)
			}
			current = value[index]
		default:
			return nil, fmt.Errorf("JSON Pointer %s traverses a scalar", pointer)
		}
	}
	return current, nil
}

func pointerTokens(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, errors.New("invalid JSON Pointer")
	}
	tokens := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for _, token := range tokens {
		for index := 0; index < len(token); index++ {
			if token[index] == '~' && (index+1 >= len(token) || token[index+1] != '0' && token[index+1] != '1') {
				return nil, errors.New("invalid JSON Pointer escape")
			}
		}
	}
	return tokens, nil
}

func unescape(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
}

func cloneJSON(value any) any {
	encoded, _ := json.Marshal(value)
	var clone any
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	_ = decoder.Decode(&clone)
	return clone
}
