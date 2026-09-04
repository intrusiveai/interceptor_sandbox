package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

func TestOpenAIResponsesProviderPreservesNativeRequestAndResponse(t *testing.T) {
	var path string
	var upstream map[string]any
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		path = request.URL.Path
		if err := json.NewDecoder(request.Body).Decode(&upstream); err != nil {
			t.Fatal(err)
		}
		body := `{"id":"resp_1","object":"response","created_at":1700000000,"model":"gpt-test","status":"completed","output":[{"type":"reasoning","summary":[]},{"type":"function_call","call_id":"call_2","name":"lookup","arguments":"{\"q\":\"next\"}"}],"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	provider, err := NewOpenAIResponsesProvider(HTTPProviderConfig{BaseURL: "https://api.openai.example/v1", APIKey: "secret", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	requestBody := []byte(`{"model":"gpt-test","input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"old\"}"},{"type":"function_call_output","call_id":"call_1","output":"old result"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"store":false}`)
	result, err := provider.Complete(context.Background(), ProviderRequest{Codec: CodecOpenAIResponses, Model: "gpt-test", Body: requestBody, MaxBodyBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/responses" || upstream["store"] != false {
		t.Fatalf("unexpected Responses request: path=%q body=%#v", path, upstream)
	}
	if string(result.RequestBody) != string(requestBody) || string(result.RawBody) != string(result.Body) || !strings.Contains(string(result.Body), `"object":"response"`) {
		t.Fatalf("native Responses exchange was not preserved: request=%s raw=%s body=%s", result.RequestBody, result.RawBody, result.Body)
	}
	items, _ := upstream["input"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["type"] != "function_call" || items[1].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("native Responses items changed: %#v", items)
	}
}

type fakeBedrockConverse struct {
	input *bedrockruntime.ConverseInput
}

func (f *fakeBedrockConverse) Converse(_ context.Context, input *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	f.input = input
	return &bedrockruntime.ConverseOutput{
		Output: &types.ConverseOutputMemberMessage{Value: types.Message{
			Role: types.ConversationRoleAssistant,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberText{Value: "checking"},
				&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{ToolUseId: aws.String("tool-2"), Name: aws.String("lookup"), Input: document.NewLazyDocument(map[string]any{"q": "next"})}},
			},
		}},
		StopReason: types.StopReasonToolUse,
		Usage:      &types.TokenUsage{InputTokens: aws.Int32(8), OutputTokens: aws.Int32(3), TotalTokens: aws.Int32(11)},
		Metrics:    &types.ConverseMetrics{},
	}, nil
}

func TestBedrockConverseProviderUsesNativeSDKAndHTTPShapes(t *testing.T) {
	client := &fakeBedrockConverse{}
	provider := NewBedrockConverseProvider(client)
	body := []byte(`{"system":[{"text":"Be useful"}],"messages":[{"role":"assistant","content":[{"toolUse":{"toolUseId":"tool-1","name":"lookup","input":{"q":"old"}}}]},{"role":"user","content":[{"toolResult":{"toolUseId":"tool-1","status":"success","content":[{"text":"old result"}]}}]}],"toolConfig":{"tools":[{"toolSpec":{"name":"lookup","description":"Lookup","inputSchema":{"json":{"type":"object","properties":{"q":{"type":"string"}}}}}}],"toolChoice":{"any":{}}},"inferenceConfig":{"maxTokens":200,"temperature":0.2}}`)
	result, err := provider.Complete(context.Background(), ProviderRequest{Codec: CodecBedrockConverse, Model: "anthropic.test", Body: body, MaxBodyBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if client.input == nil || aws.ToString(client.input.ModelId) != "anthropic.test" || len(client.input.System) != 1 || len(client.input.Messages) != 2 {
		t.Fatalf("unexpected Converse input: %#v", client.input)
	}
	if client.input.ToolConfig == nil || len(client.input.ToolConfig.Tools) != 1 {
		t.Fatalf("tool configuration was not converted: %#v", client.input.ToolConfig)
	}
	toolResult, ok := client.input.Messages[1].Content[0].(*types.ContentBlockMemberToolResult)
	if !ok {
		t.Fatalf("tool result was not converted into a Bedrock user message: %#v", client.input.Messages[1])
	}
	toolText, ok := toolResult.Value.Content[0].(*types.ToolResultContentBlockMemberText)
	if !ok || toolText.Value != "old result" {
		t.Fatalf("injected tool-result text was not retained by Bedrock translation: %#v", toolResult.Value.Content)
	}
	if _, ok := client.input.ToolConfig.ToolChoice.(*types.ToolChoiceMemberAny); !ok {
		t.Fatalf("required tool choice was not converted: %#v", client.input.ToolConfig.ToolChoice)
	}
	if client.input.InferenceConfig == nil || aws.ToInt32(client.input.InferenceConfig.MaxTokens) != 200 {
		t.Fatalf("inference configuration was not converted: %#v", client.input.InferenceConfig)
	}
	var native map[string]any
	if err := json.Unmarshal(result.Body, &native); err != nil {
		t.Fatal(err)
	}
	if native["stopReason"] != "tool_use" || native["usage"].(map[string]any)["totalTokens"] != float64(11) || native["output"] == nil {
		t.Fatalf("unexpected native Bedrock response: %#v", native)
	}
	if !json.Valid(result.RequestBody) || !strings.Contains(string(result.RequestBody), `"toolResult"`) || !strings.Contains(string(result.RequestBody), `"modelId":"anthropic.test"`) {
		t.Fatalf("provider evidence did not capture the native Converse request: %s", result.RequestBody)
	}
	if !json.Valid(result.RawBody) || !strings.Contains(string(result.RawBody), `"stopReason":"tool_use"`) {
		t.Fatalf("provider evidence did not capture the native Converse response: %s", result.RawBody)
	}
}

func TestLiteLLMProviderUsesFixedChatEndpointAndOptionalGatewayKey(t *testing.T) {
	var receivedPath, authorization, receivedBody string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		receivedPath = request.URL.Path
		authorization = request.Header.Get("Authorization")
		body, _ := io.ReadAll(request.Body)
		receivedBody = string(body)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[]}`)), Request: request}, nil
	})}
	provider, err := NewLiteLLMProvider(HTTPProviderConfig{BaseURL: "http://127.0.0.1:4000/v1", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"gateway-alias","messages":[]}`)
	result, err := provider.Complete(context.Background(), ProviderRequest{Codec: CodecOpenAIChat, Model: "gateway-alias", Body: body, MaxBodyBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if receivedPath != "/v1/chat/completions" || authorization != "" || receivedBody != string(body) || string(result.Body) != `{"choices":[]}` {
		t.Fatalf("unexpected LiteLLM pass-through: path=%q auth=%q request=%s response=%s", receivedPath, authorization, receivedBody, result.Body)
	}
	responsesBody := []byte(`{"model":"gateway-alias","input":"hello"}`)
	if _, err := provider.Complete(context.Background(), ProviderRequest{Codec: CodecOpenAIResponses, Model: "gateway-alias", Body: responsesBody, MaxBodyBytes: 4096}); err != nil {
		t.Fatal(err)
	}
	if receivedPath != "/v1/responses" || receivedBody != string(responsesBody) {
		t.Fatalf("unexpected LiteLLM Responses pass-through: path=%q request=%s", receivedPath, receivedBody)
	}
}

func TestProvidersAdvertiseOnlyNativeApplicationCodecs(t *testing.T) {
	responses, _ := NewOpenAIResponsesProvider(HTTPProviderConfig{BaseURL: "https://api.openai.example/v1"})
	if !responses.SupportsCodec(CodecOpenAIResponses) || responses.SupportsCodec(CodecOpenAIChat) {
		t.Fatal("OpenAI Responses provider codec contract is incorrect")
	}
	bedrock := NewBedrockConverseProvider(&fakeBedrockConverse{})
	if !bedrock.SupportsCodec(CodecBedrockConverse) || bedrock.SupportsCodec(CodecOpenAIChat) {
		t.Fatal("Bedrock provider codec contract is incorrect")
	}
	litellm, _ := NewLiteLLMProvider(HTTPProviderConfig{BaseURL: "http://127.0.0.1:4000/v1"})
	if !litellm.SupportsCodec(CodecOpenAIChat) || !litellm.SupportsCodec(CodecOpenAIResponses) {
		t.Fatal("LiteLLM should accept both OpenAI-compatible routes")
	}
}

func TestExtendedHTTPProvidersUseNativeRoutesAndHostCredentials(t *testing.T) {
	tests := []struct {
		name, wantPath, wantHeader, wantValue, codec, model string
		newProvider                                         func(HTTPProviderConfig) (ModelProvider, error)
	}{
		{"anthropic", "/v1/messages", "X-Api-Key", "secret", CodecAnthropicMessages, "claude-test", NewAnthropicMessagesProvider},
		{"gemini", "/v1beta/models/gemini-test:generateContent", "X-Goog-Api-Key", "secret", CodecGoogleGenerateContent, "gemini-test", NewGeminiAPIProvider},
		{"vertex", "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-test:generateContent", "Authorization", "Bearer secret", CodecGoogleGenerateContent, "gemini-test", NewVertexGeminiProvider},
		{"azure", "/openai/v1/chat/completions", "Api-Key", "secret", CodecOpenAIChat, "deployment-test", NewAzureOpenAIProvider},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var path, header string
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				path, header = request.URL.Path, request.Header.Get(test.wantHeader)
				if test.name == "anthropic" && request.Header.Get("Anthropic-Version") != "2023-06-01" {
					t.Fatalf("Anthropic version was not pinned: %q", request.Header.Get("Anthropic-Version"))
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: request}, nil
			})}
			base := map[string]string{
				"anthropic": "https://api.anthropic.example", "gemini": "https://generativelanguage.example/v1beta",
				"vertex": "https://us-central1-aiplatform.example/v1/projects/p/locations/us-central1",
				"azure":  "https://resource.openai.azure.example/openai/v1",
			}[test.name]
			provider, err := test.newProvider(HTTPProviderConfig{BaseURL: base, APIKey: "secret", Client: client})
			if err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"model":"` + test.model + `"}`)
			if test.codec == CodecGoogleGenerateContent {
				body = []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
			}
			_, err = provider.Complete(context.Background(), ProviderRequest{Codec: test.codec, Model: test.model, Body: body, MaxBodyBytes: 4096})
			if err != nil {
				t.Fatal(err)
			}
			if path != test.wantPath || header != test.wantValue {
				t.Fatalf("unexpected request path=%q header=%q", path, header)
			}
		})
	}
}
