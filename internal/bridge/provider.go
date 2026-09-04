package bridge

import "context"

const (
	CodecOpenAIChat            = "openai_chat_completions"
	CodecOpenAIResponses       = "openai_responses"
	CodecBedrockConverse       = "aws_bedrock_converse"
	CodecAnthropicMessages     = "anthropic_messages"
	CodecGoogleGenerateContent = "google_generate_content"

	ProviderOpenAIChat        = "openai-chat"
	ProviderOpenAIResponses   = "openai-responses"
	ProviderBedrockConverse   = "bedrock-converse"
	ProviderLiteLLM           = "litellm"
	ProviderAnthropicMessages = "anthropic-messages"
	ProviderGeminiAPI         = "gemini-api"
	ProviderVertexGemini      = "vertex-gemini"
	ProviderAzureOpenAI       = "azure-openai"
)

type ProviderRequest struct {
	Codec        string
	Model        string
	Path         string
	Headers      map[string]string
	Body         []byte
	MaxBodyBytes int64
}

type ProviderResponse struct {
	Status      int
	Headers     map[string]string
	Body        []byte
	RequestBody []byte
	RawBody     []byte
	Metadata    map[string]string
}

// ModelProvider runs on the host. Implementations may hold credentials, SDK
// clients, and provider endpoints; none of those values cross into the sandbox.
type ModelProvider interface {
	Name() string
	SupportsCodec(string) bool
	Complete(context.Context, ProviderRequest) (ProviderResponse, error)
}
