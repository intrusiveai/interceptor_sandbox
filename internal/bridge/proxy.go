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
	codec, ok := modelCodec(request.Method, request.URL.Path)
	if !ok {
		writeBridgeError(writer, http.StatusNotImplemented, "unsupported_model_route", "supported model routes are Chat Completions, OpenAI Responses, and Bedrock Converse")
		return
	}
	body, err := readLimit(request.Body, p.maxBytes)
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
	if p.injections != nil {
		switch codec {
		case CodecOpenAIChat:
			rewritten, results, err = p.injections.RewriteOpenAI(body, turnID)
		case CodecOpenAIResponses:
			rewritten, results, err = p.injections.RewriteOpenAIResponses(body, turnID)
		case CodecBedrockConverse:
			rewritten, results, err = p.injections.RewriteBedrockConverse(body, turnID)
		}
		if err != nil {
			writeBridgeError(writer, http.StatusBadRequest, "injection_failed", err.Error())
			return
		}
	}
	requestRef := p.store(request.Context(), rewritten)
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
		p.record("model_error", turnID, requestRef, map[string]any{"error": err.Error(), "elapsed_ms": time.Since(started).Milliseconds()})
		writeBridgeError(writer, http.StatusBadGateway, "model_connector_error", err.Error())
		return
	}
	ids := []string{}
	for _, item := range results {
		if item.Applied {
			ids = append(ids, item.InjectionID)
		}
	}
	if p.injections != nil {
		candidates := append(append([]string(nil), ids...), p.injections.ObservableFidelityIDs(turnID)...)
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
	responseRef := p.store(request.Context(), result.Body)
	providerRequestRef := p.store(request.Context(), result.ProviderRequest)
	providerResponseRef := p.store(request.Context(), result.ProviderResponse)
	model, usage := modelSemantics(codec, request.URL.EscapedPath(), rewritten, result.Body)
	if p.onTokens != nil {
		if total := totalTokens(usage); total > 0 {
			p.onTokens(total)
		}
	}
	p.record("model_exchange", turnID, requestRef, map[string]any{
		"codec": codec, "provider": result.Provider, "model": model, "status": result.Status,
		"request_ref": requestRef, "response_ref": responseRef, "provider_request_ref": providerRequestRef,
		"provider_response_ref": providerResponseRef, "final_proxy_request_digest": result.FinalRequestDigest,
		"provider_request_digest": result.ProviderDigest, "provider_response_digest": result.ProviderResponseDigest,
		"elapsed_ms": time.Since(started).Milliseconds(), "usage": usage, "injection_ids": ids,
	})
	for name, value := range result.Headers {
		writer.Header().Set(name, value)
	}
	writer.WriteHeader(result.Status)
	_, _ = writer.Write(result.Body)
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

func (p *ModelProxy) record(kind, turnID, rawRef string, semantic any) {
	if p.recorder == nil {
		return
	}
	_, _ = p.recorder.Append(recording.Draft{Channel: recording.Evaluation, Layer: "inference_proxy", Kind: kind, Provenance: recording.Application, TurnID: turnID, RawRef: rawRef, Semantic: semantic})
}

func modelSemantics(codec, path string, request, response []byte) (string, any) {
	model, _ := modelIdentifier(codec, path, request)
	var responseValue struct {
		Usage any `json:"usage"`
	}
	_ = json.Unmarshal(response, &responseValue)
	return model, responseValue.Usage
}

func modelCodec(method, requestPath string) (string, bool) {
	if method != http.MethodPost {
		return "", false
	}
	switch {
	case strings.HasSuffix(requestPath, "/chat/completions"):
		return CodecOpenAIChat, true
	case strings.HasSuffix(requestPath, "/responses"):
		return CodecOpenAIResponses, true
	case strings.Contains(requestPath, "/model/") && strings.HasSuffix(requestPath, "/converse"):
		return CodecBedrockConverse, true
	default:
		return "", false
	}
}

func validateNonStreaming(codec string, body []byte) error {
	if codec == CodecBedrockConverse {
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
