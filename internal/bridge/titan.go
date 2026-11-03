package bridge

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

const TitanEmbeddingModel = "amazon.titan-embed-text-v2:0"

type titanOptions struct {
	dimensions int
	normalize  bool
	types      []string
}

func isEmbeddingCodec(codec string) bool {
	return codec == CodecOpenAIEmbeddings || codec == CodecTitanEmbeddings
}

func validateEmbeddingCodec(codec string, body []byte) (int, error) {
	if codec == CodecTitanEmbeddings {
		_, err := titanRequest(body)
		return 1, err
	}
	return validateEmbeddingRequest(body)
}

func embeddingCodecMetadata(codec string, request, response []byte) (map[string]any, error) {
	if codec == CodecTitanEmbeddings {
		return titanResponseMetadata(request, response)
	}
	return embeddingResponseMetadata(request, response)
}

func titanModel(path string) (string, error) {
	const prefix, suffix = "/model/", "/invoke"
	invalid := errors.New("Titan path must be /model/" + TitanEmbeddingModel + "/invoke")
	if len(path) <= len(prefix)+len(suffix) || !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", invalid
	}
	model, err := url.PathUnescape(path[len(prefix) : len(path)-len(suffix)])
	if err != nil || model != TitanEmbeddingModel {
		return "", invalid
	}
	return model, nil
}

func titanRequest(body []byte) (titanOptions, error) {
	options := titanOptions{dimensions: 1024, normalize: true, types: []string{"float"}}
	if int64(len(body)) > maxEmbeddingBodyBytes {
		return options, errors.New("embeddings body exceeds 4 MiB limit")
	}
	if !utf8.Valid(body) {
		return options, errors.New("Titan input must be valid UTF-8")
	}
	object, err := decodeJSONObject(body)
	if err != nil {
		return options, err
	}
	if err := rejectUnsupportedKeys(object, []string{"inputText", "dimensions", "normalize", "embeddingTypes"}, "Titan embeddings parameter"); err != nil {
		return options, err
	}
	text, ok := object["inputText"].(string)
	if !ok || text == "" || utf8.RuneCountInString(text) > 50000 {
		return options, errors.New("Titan inputText must contain 1 to 50000 characters")
	}
	if raw, exists := object["dimensions"]; exists {
		n, ok := embeddingInteger(raw)
		if !ok || (n != 256 && n != 512 && n != 1024) {
			return options, errors.New("Titan dimensions must be 256, 512, or 1024")
		}
		options.dimensions = int(n)
	}
	if raw, exists := object["normalize"]; exists {
		value, ok := raw.(bool)
		if !ok {
			return options, errors.New("Titan normalize must be a Boolean")
		}
		options.normalize = value
	}
	if raw, exists := object["embeddingTypes"]; exists {
		values, ok := raw.([]any)
		if !ok || len(values) == 0 || len(values) > 2 {
			return options, errors.New("Titan embeddingTypes must select float, binary, or both")
		}
		options.types = nil
		for _, raw := range values {
			value, ok := raw.(string)
			if !ok || (value != "float" && value != "binary") || contains(options.types, value) {
				return options, errors.New("Titan embeddingTypes contains an invalid or duplicate representation")
			}
			options.types = append(options.types, value)
		}
	}
	return options, nil
}

func titanResponseMetadata(request, response []byte) (map[string]any, error) {
	options, err := titanRequest(request)
	if err != nil {
		return nil, err
	}
	invalid := errors.New("provider returned an invalid Titan embeddings response")
	if int64(len(response)) > maxEmbeddingBodyBytes {
		return nil, invalid
	}
	obj, err := decodeJSONObject(response)
	if err != nil {
		return nil, invalid
	}
	tokens, ok := embeddingInteger(obj["inputTextTokenCount"])
	if !ok || tokens > 1<<53-1 {
		return nil, invalid
	}
	byType, ok := obj["embeddingsByType"].(map[string]any)
	if !ok || len(byType) != len(options.types) {
		return nil, invalid
	}
	for _, kind := range options.types {
		vector, ok := byType[kind].([]any)
		if !ok || len(vector) != options.dimensions {
			return nil, invalid
		}
		for _, raw := range vector {
			number, ok := raw.(json.Number)
			if !ok {
				return nil, invalid
			}
			value, err := number.Float64()
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, invalid
			}
			if kind == "binary" && number.String() != "0" && number.String() != "1" {
				return nil, invalid
			}
		}
	}
	if contains(options.types, "float") {
		vector, ok := obj["embedding"].([]any)
		values := byType["float"].([]any)
		if !ok || len(vector) != len(values) {
			return nil, invalid
		}
		for i, raw := range vector {
			n, ok := raw.(json.Number)
			if !ok {
				return nil, invalid
			}
			a, err := n.Float64()
			b, _ := values[i].(json.Number).Float64()
			if err != nil || a != b {
				return nil, invalid
			}
		}
	} else if _, exists := obj["embedding"]; exists {
		return nil, invalid
	}
	return map[string]any{"input_count": 1, "dimensions": options.dimensions, "normalize": options.normalize, "embedding_types": options.types}, nil
}

// Keep local validation/storage failures consumable by native AWS SDK clients.
func writeModelError(writer http.ResponseWriter, codec string, status int, code, message string) {
	if codec != CodecTitanEmbeddings {
		writeBridgeError(writer, status, code, message)
		return
	}
	errorType := "InternalServerException"
	if status == 400 || status == 413 {
		errorType = "ValidationException"
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Amzn-Errortype", errorType)
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"message": message, "code": code})
}
