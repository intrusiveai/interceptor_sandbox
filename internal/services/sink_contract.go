package services

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/delivery"
)

// SinkAllowlists is shared by validation, discovery and runtime setup. Invalid
// configuration must never silently turn a restricted sink into an open one.
func SinkAllowlists(options map[string]any) (methods, paths []string, err error) {
	read := func(key string) ([]string, error) {
		value := options[key]
		if value == nil {
			return nil, nil
		}
		var values []string
		switch v := value.(type) {
		case []string:
			values = append(values, v...)
		case []any:
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("sink %s must contain strings", key)
				}
				values = append(values, s)
			}
		default:
			return nil, fmt.Errorf("sink %s must be a list", key)
		}
		seen := map[string]bool{}
		result := []string{}
		for _, s := range values {
			if key == "methods" {
				s = strings.ToUpper(strings.TrimSpace(s))
				_, methodErr := http.NewRequest(s, "http://sink.invalid/", nil)
				if s == "" || methodErr != nil {
					return nil, fmt.Errorf("invalid sink method %q", s)
				}
			} else if !strings.HasPrefix(s, "/") || strings.ContainsAny(s, "?#\r\n") {
				return nil, fmt.Errorf("invalid sink path %q", s)
			}
			if !seen[s] {
				seen[s] = true
				result = append(result, s)
			}
		}
		sort.Strings(result)
		return result, nil
	}
	if methods, err = read("methods"); err != nil {
		return
	}
	paths, err = read("paths")
	return
}

// SinkEndpoint describes routing and response encoding without exporting the
// configured response, captures, destination, or request examples.
func SinkEndpoint(methods, paths []string, response SinkResponse, maxBytes int64) *delivery.Endpoint {
	restricted := len(methods) > 0
	methods = slices.DeleteFunc(slices.Clone(methods), func(method string) bool { return method == "HEAD" })
	if restricted && len(methods) == 0 {
		return nil
	}
	if maxBytes == 0 {
		maxBytes = 1 << 20
	}
	if maxBytes > maxServiceRequestBody {
		maxBytes = maxServiceRequestBody
	}
	output := delivery.Body{MediaType: "text/plain", Encoding: "utf8", Basis: "source-derived", MaxBytes: maxServiceRequestBody}
	if response.JSON != nil {
		output.MediaType = "application/json"
		output.Encoding = "json"
		output.Schema = map[string]any{"type": "object"}
	}
	return &delivery.Endpoint{ID: "response", Kind: "http-route-set", Methods: methods, Paths: slices.Clone(paths), InjectionRoot: "response-body", MaximumRequestBytes: maxBytes,
		Delivery: &delivery.Contract{Description: "Captured attacker requests receive a body-only injectable response. Method/path lists are exact allowlists; empty lists are unrestricted except HEAD, which has no response body. No response values are disclosed.",
			Input: delivery.Body{MediaType: "application/octet-stream", Encoding: "bytes", Basis: "source-derived", MaxBytes: maxBytes}, Output: output}}
}
