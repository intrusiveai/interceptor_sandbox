package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

func decodeJSONObject(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("request body must contain exactly one JSON object")
	}
	return value, nil
}

func rejectUnsupportedKeys(object map[string]any, allowed []string, label string) error {
	set := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		set[key] = true
	}
	for key := range object {
		if !set[key] {
			return fmt.Errorf("unsupported %s %q", label, key)
		}
	}
	return nil
}

func numberString(value any) string {
	switch typed := value.(type) {
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	default:
		return fmt.Sprint(value)
	}
}

func int32Value(raw any) (int32, error) {
	value, err := strconv.ParseInt(numberString(raw), 10, 32)
	return int32(value), err
}

func float32Value(raw any) (float32, error) {
	value, err := strconv.ParseFloat(numberString(raw), 32)
	return float32(value), err
}

func stringListValue(raw any) ([]string, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, errors.New("not a string list")
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		value, ok := item.(string)
		if !ok {
			return nil, errors.New("not a string list")
		}
		values = append(values, value)
	}
	return values, nil
}
