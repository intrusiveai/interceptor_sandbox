package bridge

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// Four copies (request/response plus evidence), base64-expanded in the bridge,
// fit below its 32 MiB frame limit with room for headers and metadata.
const maxEmbeddingBodyBytes int64 = 4 << 20
const maxEmbeddingBatch = 128

func embeddingInteger(value any) (int64, bool) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil && i >= 0
}

func validateEmbeddingRequest(body []byte) (int, error) {
	if int64(len(body)) > maxEmbeddingBodyBytes {
		return 0, errors.New("embeddings body exceeds 4 MiB limit")
	}
	object, err := decodeJSONObject(body)
	if err != nil {
		return 0, err
	}
	if err := rejectUnsupportedKeys(object, []string{"model", "input", "dimensions", "encoding_format", "user"}, "embeddings parameter"); err != nil {
		return 0, err
	}
	if model, ok := object["model"].(string); !ok || model == "" {
		return 0, errors.New("embeddings require a model identifier")
	}
	if value, exists := object["dimensions"]; exists {
		if n, ok := embeddingInteger(value); !ok || n == 0 {
			return 0, errors.New("embeddings dimensions must be a positive integer")
		}
	}
	if value, exists := object["encoding_format"]; exists && value != "float" && value != "base64" {
		return 0, errors.New("embeddings encoding_format must be float or base64")
	}
	if value, exists := object["user"]; exists {
		if _, ok := value.(string); !ok {
			return 0, errors.New("embeddings user must be a string")
		}
	}
	count, ok := embeddingInputCount(object["input"])
	if !ok {
		return 0, errors.New("embeddings input must be nonempty text, a text batch, integer tokens, or a batch of integer token arrays (maximum 128 inputs)")
	}
	return count, nil
}

func embeddingInputCount(input any) (int, bool) {
	if text, ok := input.(string); ok {
		return 1, text != ""
	}
	items, ok := input.([]any)
	if !ok || len(items) == 0 {
		return 0, false
	}
	if _, ok := items[0].(json.Number); ok {
		return 1, embeddingTokens(items)
	}
	if len(items) > maxEmbeddingBatch {
		return 0, false
	}
	switch items[0].(type) {
	case string:
		for _, item := range items {
			if text, ok := item.(string); !ok || text == "" {
				return 0, false
			}
		}
	case []any:
		for _, item := range items {
			tokens, ok := item.([]any)
			if !ok || !embeddingTokens(tokens) {
				return 0, false
			}
		}
	default:
		return 0, false
	}
	return len(items), true
}

func embeddingTokens(tokens []any) bool {
	if len(tokens) == 0 {
		return false
	}
	for _, token := range tokens {
		if n, ok := embeddingInteger(token); !ok || n > math.MaxInt32 {
			return false
		}
	}
	return true
}

// Validate without rewriting response bytes, including the provider's index order
// and reported model (which can differ from a deployment alias).
func embeddingResponseMetadata(request, response []byte) (map[string]any, error) {
	invalid := errors.New("provider returned an invalid embeddings response")
	count, err := validateEmbeddingRequest(request)
	if err != nil {
		return nil, err
	}
	if int64(len(response)) > maxEmbeddingBodyBytes {
		return nil, errors.New("embeddings response exceeds 4 MiB limit")
	}
	req, _ := decodeJSONObject(request)
	obj, err := decodeJSONObject(response)
	if err != nil {
		return nil, invalid
	}
	model, ok := obj["model"].(string)
	if !ok || model == "" || obj["object"] != "list" {
		return nil, invalid
	}
	data, ok := obj["data"].([]any)
	if !ok || len(data) != count {
		return nil, invalid
	}
	seen := make(map[int64]bool, count)
	width := 0
	for _, raw := range data {
		item, ok := raw.(map[string]any)
		if !ok || item["object"] != "embedding" {
			return nil, invalid
		}
		index, ok := embeddingInteger(item["index"])
		if !ok || index >= int64(count) || seen[index] {
			return nil, invalid
		}
		seen[index] = true
		n := 0
		switch vector := item["embedding"].(type) {
		case []any:
			if req["encoding_format"] == "base64" {
				return nil, invalid
			}
			n = len(vector)
			for _, raw := range vector {
				value, ok := raw.(json.Number)
				if !ok {
					return nil, invalid
				}
				f, err := value.Float64()
				if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
					return nil, invalid
				}
			}
		case string:
			if req["encoding_format"] == "float" {
				return nil, invalid
			}
			decoded, err := base64.StdEncoding.Strict().DecodeString(vector)
			if err != nil || len(decoded)%4 != 0 {
				return nil, invalid
			}
			n = len(decoded) / 4
			for i := 0; i < len(decoded); i += 4 {
				f := float64(math.Float32frombits(binary.LittleEndian.Uint32(decoded[i : i+4])))
				if math.IsNaN(f) || math.IsInf(f, 0) {
					return nil, invalid
				}
			}
		default:
			return nil, invalid
		}
		if n == 0 || width != 0 && n != width {
			return nil, invalid
		}
		width = n
	}
	if dimensions, exists := req["dimensions"]; exists {
		n, _ := embeddingInteger(dimensions)
		if int64(width) != n {
			return nil, fmt.Errorf("provider embedding dimensions do not match requested dimensions")
		}
	}
	usage, ok := obj["usage"].(map[string]any)
	if !ok {
		return nil, invalid
	}
	prompt, pOK := embeddingInteger(usage["prompt_tokens"])
	total, tOK := embeddingInteger(usage["total_tokens"])
	if !pOK || !tOK || total != prompt || total > 1<<53-1 {
		return nil, invalid
	}
	return map[string]any{"reported_model": model, "input_count": count, "dimensions": width}, nil
}
