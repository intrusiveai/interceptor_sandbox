package services

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

type responseCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *responseCapture) Header() http.Header { return c.header }
func (c *responseCapture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}
func (c *responseCapture) Write(body []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(body)
}

// WithResponseInjection is deliberately placed inside authorization middleware:
// rejected requests never produce an injectable candidate.
func WithResponseInjection(serviceID, kind string, next http.Handler, manager *injection.Manager, recorder *recording.Writer) http.Handler {
	if manager == nil {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestBody, _ := io.ReadAll(io.LimitReader(request.Body, maxServiceRequestBody+1))
		request.Body = io.NopCloser(bytes.NewReader(requestBody))
		var requestValue any = string(requestBody)
		requestDecoder := json.NewDecoder(bytes.NewReader(requestBody))
		requestDecoder.UseNumber()
		var decodedRequest any
		if requestDecoder.Decode(&decodedRequest) == nil && requestDecoder.Decode(&struct{}{}) == io.EOF {
			requestValue = decodedRequest
		}
		capture := &responseCapture{header: make(http.Header)}
		next.ServeHTTP(capture, request)
		if capture.status == 0 {
			capture.status = http.StatusOK
		}
		for name, values := range capture.header {
			for _, value := range values {
				writer.Header().Add(name, value)
			}
		}
		body := capture.body.Bytes()
		if capture.status >= 200 && capture.status < 300 {
			surface, tool, args, candidate := "service_response", "", any(nil), any(nil)
			var envelope map[string]any
			if kind == "mcp_http" && json.Unmarshal(requestBody, &envelope) == nil && envelope["method"] == "tools/call" {
				surface = "mcp_tool_result"
				if params, ok := envelope["params"].(map[string]any); ok {
					tool, _ = params["name"].(string)
					args = params["arguments"]
				}
			}
			var responseValue any
			if json.Unmarshal(body, &responseValue) == nil {
				candidate = responseValue
				if surface == "mcp_tool_result" {
					candidate = nil
					if object, ok := responseValue.(map[string]any); ok {
						candidate = object["result"]
						if result, ok := candidate.(map[string]any); ok {
							if failed, _ := result["isError"].(bool); failed {
								candidate = nil
							}
						}
					}
				}
			} else {
				candidate = string(body)
			}
			if candidate != nil && !(surface == "mcp_tool_result" && tool == "") {
				changed, results, err := manager.RewriteResponse(surface, serviceID, tool, injection.ResponseContext{
					Method: request.Method, Path: request.URL.Path, Query: cloneQuery(request.URL.Query()),
					Headers: cloneHeaders(request.Header), Body: requestValue, Arguments: args,
				}, candidate)
				if err != nil {
					writer.Header().Del("Content-Length")
					http.Error(writer, "response injection failed closed", http.StatusInternalServerError)
					return
				}
				if len(results) > 0 {
					if surface == "mcp_tool_result" {
						responseValue.(map[string]any)["result"] = changed
						body, _ = json.Marshal(responseValue)
					} else if text, ok := changed.(string); ok {
						body = []byte(text)
					} else {
						body, _ = json.Marshal(changed)
					}
					if recorder != nil {
						ids := make([]string, 0, len(results))
						for _, result := range results {
							if result.Applied {
								ids = append(ids, result.InjectionID)
							}
						}
						attemptID, payloadDigest := manager.Attribution(ids)
						_, _ = recorder.AppendContext(request.Context(), recording.Draft{Channel: recording.Evaluation, Layer: "service_ingress", Kind: "response_rewritten", Provenance: recording.HarnessInjected, AttemptID: attemptID, PayloadDigest: payloadDigest, Semantic: map[string]any{"service": serviceID, "surface": surface, "tool_name": tool, "method": request.Method, "path": request.URL.Path, "request_digest": digest.Bytes(requestBody), "original_digest": digest.Bytes(capture.body.Bytes()), "result_digest": digest.Bytes(body), "injections": results}})
					}
				}
			}
		}
		writer.Header().Del("Content-Length")
		writer.WriteHeader(capture.status)
		_, _ = writer.Write(body)
	})
}

func cloneQuery(values map[string][]string) map[string][]string {
	result := make(map[string][]string, len(values))
	for name, entries := range values {
		result[name] = append([]string(nil), entries...)
	}
	return result
}

func cloneHeaders(values map[string][]string) map[string][]string {
	result := make(map[string][]string, len(values))
	for name, entries := range values {
		result[http.CanonicalHeaderKey(name)] = append([]string(nil), entries...)
	}
	return result
}
