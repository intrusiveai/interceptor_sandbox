package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

type ModelProxy struct {
	sessionID   string
	connector   Connector
	injections  *injection.Manager
	blobs       *blobstore.Store
	recorder    *recording.Writer
	maxBytes    int64
	timeout     time.Duration
	nextID      atomic.Uint64
	connectorMu sync.Mutex
	onTokens    func(int64)
}

func (p *ModelProxy) SetTokenObserver(observer func(int64)) { p.onTokens = observer }

func NewModelProxy(sessionID string, connector Connector, injections *injection.Manager, blobs *blobstore.Store, recorder *recording.Writer, maxBytes int64, timeout time.Duration) (*ModelProxy, error) {
	if sessionID == "" || connector == nil || maxBytes <= 0 || timeout <= 0 {
		return nil, fmt.Errorf("model proxy requires session, connector, limits, and timeout")
	}
	return &ModelProxy{sessionID: sessionID, connector: connector, injections: injections, blobs: blobs, recorder: recorder, maxBytes: maxBytes, timeout: timeout}, nil
}

func (p *ModelProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		writeBridgeError(writer, http.StatusBadRequest, "unsupported_model_route", "model routes may not contain caller-selected query parameters")
		return
	}
	codec, ok := modelCodec(request.Method, request.URL.Path)
	if !ok {
		writeBridgeError(writer, http.StatusNotImplemented, "unsupported_model_route", "unsupported model route")
		return
	}
	bodyLimit := p.maxBytes
	if codec == CodecOpenAIEmbeddings {
		bodyLimit = min(bodyLimit, maxEmbeddingBodyBytes)
	}
	body, err := readLimit(request.Body, bodyLimit)
	if err != nil {
		writeBridgeError(writer, http.StatusRequestEntityTooLarge, "model_body_limit", err.Error())
		return
	}
	turnID := request.Header.Get("X-Interceptor-Turn-ID")
	rewritten := body
	results := []injection.Result{}
	if err := validateNonStreaming(codec, body); err != nil {
		writeBridgeError(writer, http.StatusBadRequest, "unsupported_streaming", err.Error())
		return
	}
	if err := validateNativeShape(codec, body, request.Header); err != nil {
		writeBridgeError(writer, http.StatusBadRequest, "unsupported_model_feature", err.Error())
		return
	}
	if p.injections != nil && codec != CodecOpenAIEmbeddings {
		switch codec {
		case CodecOpenAIChat:
			rewritten, results, err = p.injections.RewriteOpenAI(body, turnID)
		case CodecOpenAIResponses:
			rewritten, results, err = p.injections.RewriteOpenAIResponses(body, turnID)
		case CodecBedrockConverse:
			rewritten, results, err = p.injections.RewriteBedrockConverse(body, turnID)
		case CodecAnthropicMessages:
			rewritten, results, err = p.injections.RewriteAnthropicMessages(body, turnID)
		case CodecGoogleGenerateContent:
			rewritten, results, err = p.injections.RewriteGoogleGenerateContent(body, turnID)
		}
		if err != nil {
			writeBridgeError(writer, http.StatusBadRequest, "injection_failed", err.Error())
			return
		}
	}
	// Freeze already-applied response candidates before forwarding. A response
	// rewritten while this model request is in flight cannot qualify this request.
	responseCandidates := []string{}
	if p.injections != nil && codec != CodecOpenAIEmbeddings {
		responseCandidates = p.injections.ObservableResponseIDs()
	}
	requestRef := p.store(request.Context(), rewritten)
	if codec == CodecOpenAIEmbeddings && p.blobs != nil && requestRef == "" {
		p.record("model_error", turnID, "", map[string]any{"codec": codec, "error": "embedding request evidence storage failed"})
		writeBridgeError(writer, http.StatusInsufficientStorage, "evidence_storage_error", "embedding request exceeds evidence storage limits or storage is unavailable")
		return
	}
	started := time.Now()
	p.connectorMu.Lock()
	requestID := p.nextID.Add(1)
	result, err := p.connector.Forward(request.Context(), ModelFrame{
		SessionID: p.sessionID, RequestID: requestID, Codec: codec, Path: request.URL.EscapedPath(),
		Headers: map[string]string{
			"OpenAI-Organization": request.Header.Get("OpenAI-Organization"),
			"OpenAI-Project":      request.Header.Get("OpenAI-Project"),
		}, Body: rewritten, TimeoutMS: p.timeout.Milliseconds(), CreatedAt: time.Now().UTC(),
	})
	p.connectorMu.Unlock()
	if err != nil {
		p.record("model_error", turnID, requestRef, map[string]any{"codec": codec, "error": err.Error(), "elapsed_ms": time.Since(started).Milliseconds()})
		writeBridgeError(writer, http.StatusBadGateway, "model_connector_error", err.Error())
		return
	}
	ids := []string{}
	for _, item := range results {
		if item.Applied {
			ids = append(ids, item.InjectionID)
		}
	}
	if p.injections != nil && codec != CodecOpenAIEmbeddings {
		candidates := append(append([]string(nil), ids...), p.injections.ObservableFidelityIDs(turnID)...)
		candidates = append(candidates, responseCandidates...)
		confirmed, confirmErr := p.injections.ConfirmDelivery(result.FinalRequest, candidates)
		if confirmErr != nil {
			p.record("injection_state_error", turnID, requestRef, map[string]any{"error": confirmErr.Error()})
			writeBridgeError(writer, http.StatusInternalServerError, "injection_state_error", confirmErr.Error())
			return
		}
		if err := p.injections.CompleteFidelityTurn(turnID, confirmed); err != nil {
			p.record("injection_state_error", turnID, requestRef, map[string]any{"error": err.Error()})
			writeBridgeError(writer, http.StatusInternalServerError, "injection_state_error", err.Error())
			return
		}
		for _, id := range confirmed {
			if !containsString(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	model, usage := modelSemantics(codec, request.URL.EscapedPath(), rewritten, result.Body)
	// Account for a completed provider call even if retaining its evidence fails.
	if p.onTokens != nil && (codec != CodecOpenAIEmbeddings || result.Status >= 200 && result.Status < 300) {
		if total := totalTokens(usage); total > 0 {
			p.onTokens(total)
		}
	}
	responseRef := p.store(request.Context(), result.Body)
	providerRequestRef := p.store(request.Context(), result.ProviderRequest)
	providerResponseRef := p.store(request.Context(), result.ProviderResponse)
	if codec == CodecOpenAIEmbeddings && p.blobs != nil && (len(result.Body) > 0 && responseRef == "" || len(result.ProviderRequest) > 0 && providerRequestRef == "" || len(result.ProviderResponse) > 0 && providerResponseRef == "") {
		p.record("model_error", turnID, requestRef, map[string]any{"codec": codec, "model": model, "status": result.Status, "usage": usage, "error": "embedding response evidence storage failed"})
		writeBridgeError(writer, http.StatusInsufficientStorage, "evidence_storage_error", "embedding response exceeds evidence storage limits or storage is unavailable")
		return
	}
	finish, safety := modelOutcome(codec, result.Body)
	attemptID, payloadDigest := "", ""
	if p.injections != nil && codec != CodecOpenAIEmbeddings {
		attemptID, payloadDigest = p.injections.Attribution(ids)
	}
	exchange := map[string]any{
		"codec": codec, "provider": result.Provider, "model": model, "status": result.Status,
		"request_ref": requestRef, "response_ref": responseRef, "provider_request_ref": providerRequestRef,
		"provider_response_ref": providerResponseRef, "final_proxy_request_digest": result.FinalRequestDigest,
		"provider_request_digest": result.ProviderDigest, "provider_response_digest": result.ProviderResponseDigest,
		"elapsed_ms": time.Since(started).Milliseconds(), "usage": usage, "injection_ids": ids,
		"provider_metadata": result.ProviderMetadata, "finish_status": finish, "safety_status": safety,
		"attempt_id": attemptID, "payload_digest": payloadDigest,
	}
	if codec == CodecOpenAIEmbeddings {
		delete(exchange, "finish_status")
		delete(exchange, "safety_status")
		exchange["operation"] = "embeddings"
		count, _ := validateEmbeddingRequest(rewritten)
		exchange["input_count"] = count
		if result.Status >= 200 && result.Status < 300 {
			metadata, validationErr := embeddingResponseMetadata(rewritten, result.Body)
			if validationErr != nil {
				writeBridgeError(writer, http.StatusBadGateway, "invalid_embeddings_response", validationErr.Error())
				return
			}
			for key, value := range metadata {
				exchange[key] = value
			}
		}
	}
	if err := p.record("model_exchange", turnID, requestRef, exchange); err != nil && codec == CodecOpenAIEmbeddings {
		writeBridgeError(writer, http.StatusInsufficientStorage, "evidence_storage_error", "embedding exchange could not be recorded")
		return
	}
	for name, value := range result.Headers {
		writer.Header().Set(name, value)
	}
	writer.WriteHeader(result.Status)
	_, _ = writer.Write(result.Body)
}

func validateNativeShape(codec string, body []byte, headers http.Header) error {
	object, err := decodeJSONObject(body)
	if err != nil {
		return err
	}
	if value, ok := object["background"].(bool); ok && value {
		return errors.New("background model operations are not supported")
	}
	switch codec {
	case CodecOpenAIEmbeddings:
		_, err := validateEmbeddingRequest(body)
		return err
	case CodecAnthropicMessages:
		if err := rejectUnsupportedKeys(object, []string{"model", "max_tokens", "messages", "system", "temperature", "top_p", "top_k", "stop_sequences", "tools", "tool_choice", "metadata", "stream"}, "Anthropic Messages parameter"); err != nil {
			return err
		}
		if headers.Get("Anthropic-Beta") != "" {
			return errors.New("undeclared Anthropic beta features are not supported")
		}
		if tools, ok := object["tools"].([]any); ok {
			for _, raw := range tools {
				tool, _ := raw.(map[string]any)
				if tool["type"] != nil {
					return errors.New("Anthropic server tools are not supported")
				}
			}
		}
		if err := validateContentBlocks(object["messages"], "content", map[string]bool{"text": true, "tool_use": true, "tool_result": true}); err != nil {
			return err
		}
	case CodecGoogleGenerateContent:
		if err := rejectUnsupportedKeys(object, []string{"contents", "systemInstruction", "generationConfig", "safetySettings", "tools", "toolConfig", "labels"}, "Gemini generateContent parameter"); err != nil {
			return err
		}
		if raw, exists := object["generationConfig"]; exists {
			config, ok := raw.(map[string]any)
			if !ok {
				return errors.New("Gemini generationConfig must be an object")
			}
			if err := rejectUnsupportedKeys(config, []string{"stopSequences", "responseMimeType", "responseSchema", "responseJsonSchema", "candidateCount", "maxOutputTokens", "temperature", "topP", "topK", "seed", "presencePenalty", "frequencyPenalty", "responseLogprobs", "logprobs", "thinkingConfig"}, "Gemini generationConfig parameter"); err != nil {
				return err
			}
		}
		if tools, ok := object["tools"].([]any); ok {
			for _, raw := range tools {
				tool, _ := raw.(map[string]any)
				for key := range tool {
					if key != "functionDeclarations" {
						return errors.New("Gemini provider-hosted tools are not supported")
					}
				}
			}
		}
		if err := validateGoogleParts(object["contents"]); err != nil {
			return err
		}
		if instruction := object["systemInstruction"]; instruction != nil {
			if err := validateGoogleParts([]any{instruction}); err != nil {
				return err
			}
		}
	case CodecOpenAIChat:
		if err := rejectUnsupportedKeys(object, []string{"model", "messages", "tools", "tool_choice", "max_tokens", "max_completion_tokens", "temperature", "top_p", "stop", "response_format", "seed", "frequency_penalty", "presence_penalty", "n", "logprobs", "top_logprobs", "user", "stream", "stream_options", "parallel_tool_calls", "reasoning_effort", "service_tier"}, "Chat Completions parameter"); err != nil {
			return err
		}
		if tools, ok := object["tools"].([]any); ok {
			for _, raw := range tools {
				tool, _ := raw.(map[string]any)
				if tool["type"] != "function" {
					return errors.New("provider-hosted tools are not supported")
				}
			}
		}
		if err := validateOpenAIContent(object["messages"]); err != nil {
			return err
		}
	case CodecOpenAIResponses:
		if err := rejectUnsupportedKeys(object, []string{"model", "input", "instructions", "tools", "tool_choice", "max_output_tokens", "temperature", "top_p", "text", "reasoning", "parallel_tool_calls", "store", "metadata", "include", "stream", "background", "service_tier"}, "Responses parameter"); err != nil {
			return err
		}
		if stored, ok := object["store"].(bool); ok && stored {
			return errors.New("provider-stored conversation state is not supported")
		}
		if tools, ok := object["tools"].([]any); ok {
			for _, raw := range tools {
				tool, _ := raw.(map[string]any)
				if tool["type"] != "function" {
					return errors.New("provider-hosted tools are not supported")
				}
			}
		}
		if input, ok := object["input"].([]any); ok {
			if err := validateResponsesInput(input); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateResponsesInput(items []any) error {
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		kind, _ := item["type"].(string)
		if strings.Contains(kind, "image") || strings.Contains(kind, "file") || strings.Contains(kind, "computer") {
			return errors.New("multimodal, file, and computer-use inputs are not supported")
		}
		blocks, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]any)
			blockKind, _ := block["type"].(string)
			if blockKind != "input_text" && blockKind != "output_text" && blockKind != "refusal" {
				return errors.New("Responses multimodal and file content is not supported")
			}
		}
	}
	return nil
}
func validateContentBlocks(messages any, field string, allowed map[string]bool) error {
	items, ok := messages.([]any)
	if !ok {
		return nil
	}
	for _, raw := range items {
		message, _ := raw.(map[string]any)
		blocks, ok := message[field].([]any)
		if !ok {
			continue
		}
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]any)
			kind, _ := block["type"].(string)
			if kind != "" && !allowed[kind] {
				return errors.New("multimodal or provider-hosted content blocks are not supported")
			}
		}
	}
	return nil
}
func validateGoogleParts(contents any) error {
	items, ok := contents.([]any)
	if !ok {
		return nil
	}
	for _, raw := range items {
		content, _ := raw.(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			for key := range part {
				if key != "text" && key != "functionCall" && key != "functionResponse" {
					return errors.New("multimodal, file, and executable Gemini parts are not supported")
				}
			}
		}
	}
	return nil
}
func validateOpenAIContent(messages any) error {
	items, ok := messages.([]any)
	if !ok {
		return nil
	}
	for _, raw := range items {
		message, _ := raw.(map[string]any)
		if blocks, ok := message["content"].([]any); ok {
			for _, rawBlock := range blocks {
				block, _ := rawBlock.(map[string]any)
				kind, _ := block["type"].(string)
				if kind != "text" && kind != "input_text" {
					return errors.New("multimodal and file content is not supported")
				}
			}
		}
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func totalTokens(usage any) int64 {
	object, ok := usage.(map[string]any)
	if !ok {
		return 0
	}
	value := object["total_tokens"]
	if value == nil {
		value = object["totalTokens"]
	}
	switch value := value.(type) {
	case float64:
		return int64(value)
	case json.Number:
		result, _ := value.Int64()
		return result
	default:
		return 0
	}
}

func (p *ModelProxy) store(ctx context.Context, content []byte) string {
	if p.blobs == nil || len(content) == 0 {
		return ""
	}
	object, err := p.blobs.Put(ctx, bytes.NewReader(content))
	if err != nil {
		return ""
	}
	return object.Digest
}

func (p *ModelProxy) record(kind, turnID, rawRef string, semantic any) error {
	if p.recorder == nil {
		return nil
	}
	draft := recording.Draft{Channel: recording.Evaluation, Layer: "inference_proxy", Kind: kind, Provenance: recording.Application, TurnID: turnID, RawRef: rawRef, Semantic: semantic}
	if value, ok := semantic.(map[string]any); ok {
		draft.AttemptID, _ = value["attempt_id"].(string)
		draft.PayloadDigest, _ = value["payload_digest"].(string)
	}
	_, err := p.recorder.Append(draft)
	return err
}

func modelSemantics(codec, path string, request, response []byte) (string, any) {
	model, _ := modelIdentifier(codec, path, request)
	var value map[string]any
	_ = json.Unmarshal(response, &value)
	usage := value["usage"]
	if codec == CodecGoogleGenerateContent {
		if raw, ok := value["usageMetadata"].(map[string]any); ok {
			usage = map[string]any{"input_tokens": raw["promptTokenCount"], "output_tokens": raw["candidatesTokenCount"], "total_tokens": raw["totalTokenCount"]}
		}
	}
	if codec == CodecAnthropicMessages {
		if raw, ok := usage.(map[string]any); ok {
			raw["total_tokens"] = numberInt64(raw["input_tokens"]) + numberInt64(raw["output_tokens"])
			usage = raw
		}
	}
	return model, usage
}

func numberInt64(value any) int64 {
	switch value := value.(type) {
	case float64:
		return int64(value)
	case json.Number:
		number, _ := value.Int64()
		return number
	default:
		return 0
	}
}
func modelOutcome(codec string, response []byte) (any, any) {
	var value map[string]any
	if json.Unmarshal(response, &value) != nil {
		return nil, nil
	}
	switch codec {
	case CodecAnthropicMessages:
		return value["stop_reason"], nil
	case CodecGoogleGenerateContent:
		var finish, safety any
		if candidates, ok := value["candidates"].([]any); ok && len(candidates) > 0 {
			if candidate, ok := candidates[0].(map[string]any); ok {
				finish = candidate["finishReason"]
				safety = candidate["safetyRatings"]
			}
		}
		return finish, safety
	case CodecOpenAIChat:
		safety := chatSafetyAnnotations(value)
		if choices, ok := value["choices"].([]any); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				return choice["finish_reason"], safety
			}
		}
		return nil, safety
	case CodecOpenAIResponses:
		if filters, present := value["content_filters"]; present {
			safety := map[string]any{"content_filters": filters}
			if details := value["incomplete_details"]; details != nil {
				safety["incomplete_details"] = details
			}
			return value["status"], safety
		}
		return value["status"], value["incomplete_details"]
	case CodecBedrockConverse:
		return value["stopReason"], nil
	}
	return nil, nil
}

// chatSafetyAnnotations projects provider evidence only. It does not interpret
// flags or decide whether the target application may execute a returned tool.
func chatSafetyAnnotations(value map[string]any) any {
	safety := map[string]any{}
	if prompts, present := value["prompt_filter_results"]; present {
		safety["prompt_filter_results"] = prompts
	}
	annotations := []any{}
	choices, _ := value["choices"].([]any)
	for _, item := range choices {
		choice, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if filters, present := choice["content_filter_results"]; present {
			annotation := map[string]any{"content_filter_results": filters}
			if index, present := choice["index"]; present {
				annotation["index"] = index
			}
			annotations = append(annotations, annotation)
		}
	}
	if len(annotations) > 0 {
		safety["choices"] = annotations
	}
	if len(safety) == 0 {
		return nil
	}
	return safety
}

func modelCodec(method, requestPath string) (string, bool) {
	if method != http.MethodPost {
		return "", false
	}
	switch {
	case requestPath == "/v1/embeddings":
		return CodecOpenAIEmbeddings, true
	case strings.HasSuffix(requestPath, "/chat/completions"):
		return CodecOpenAIChat, true
	case strings.HasSuffix(requestPath, "/responses"):
		return CodecOpenAIResponses, true
	case strings.Contains(requestPath, "/model/") && strings.HasSuffix(requestPath, "/converse"):
		return CodecBedrockConverse, true
	case strings.HasSuffix(requestPath, "/v1/messages") || requestPath == "/v1/messages":
		return CodecAnthropicMessages, true
	case strings.Contains(requestPath, "/models/") && strings.HasSuffix(requestPath, ":generateContent"):
		return CodecGoogleGenerateContent, true
	default:
		return "", false
	}
}

func validateNonStreaming(codec string, body []byte) error {
	if codec == CodecBedrockConverse || codec == CodecGoogleGenerateContent {
		return nil
	}
	var request struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return fmt.Errorf("model request must be valid JSON: %w", err)
	}
	if request.Stream {
		return errors.New("streaming model responses are not supported in this release")
	}
	return nil
}
