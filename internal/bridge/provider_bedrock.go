package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/intrusive-ai/interceptor-sandbox/internal/credentials"
)

type BedrockProviderConfig struct {
	Authentication string
	Region         string
	Profile        string
}

type BedrockConverseAPI interface {
	Converse(context.Context, *bedrockruntime.ConverseInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
}

type bedrockConverseProvider struct{ client BedrockConverseAPI }

// LoadBedrockConverseProvider delegates authentication to the AWS SDK default
// configuration chain. No AWS credential is copied into the sandbox.
func LoadBedrockConverseProvider(ctx context.Context, config BedrockProviderConfig) (ModelProvider, error) {
	loaded, err := credentials.AWSIdentityConfig(ctx, config.Region, config.Profile, config.Authentication)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	if loaded.Region == "" {
		return nil, errors.New("AWS region is required; set --aws-region, AWS_REGION, or a region in the selected AWS profile")
	}
	return NewBedrockConverseProvider(bedrockruntime.NewFromConfig(loaded)), nil
}

func NewBedrockConverseProvider(client BedrockConverseAPI) ModelProvider {
	return &bedrockConverseProvider{client: client}
}

func (p *bedrockConverseProvider) Name() string { return ProviderBedrockConverse }

func (p *bedrockConverseProvider) SupportsCodec(codec string) bool {
	return codec == CodecBedrockConverse
}

func (p *bedrockConverseProvider) Complete(ctx context.Context, request ProviderRequest) (ProviderResponse, error) {
	if p.client == nil {
		return ProviderResponse{}, errors.New("Bedrock Converse client is required")
	}
	if request.Codec != CodecBedrockConverse {
		return ProviderResponse{}, fmt.Errorf("Bedrock provider does not accept codec %q", request.Codec)
	}
	input, evidence, err := nativeBedrockConverse(request.Model, request.Body)
	if err != nil {
		return ProviderResponse{}, err
	}
	if int64(len(evidence)) > request.MaxBodyBytes {
		return ProviderResponse{}, errors.New("Bedrock request exceeds connector body limit")
	}
	output, err := p.client.Converse(ctx, input)
	if err != nil {
		return ProviderResponse{}, err
	}
	body, err := bedrockResponseEvidence(output)
	if err != nil {
		return ProviderResponse{}, err
	}
	if int64(len(body)) > request.MaxBodyBytes {
		return ProviderResponse{}, errors.New("Bedrock response exceeds connector body limit")
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if requestID, ok := awsmiddleware.GetRequestIDMetadata(output.ResultMetadata); ok && requestID != "" {
		headers["X-Request-ID"] = requestID
	}
	return ProviderResponse{Status: http.StatusOK, Headers: headers, Body: body, RequestBody: evidence, RawBody: append([]byte(nil), body...)}, nil
}

func bedrockResponseEvidence(output *bedrockruntime.ConverseOutput) ([]byte, error) {
	if output == nil {
		return nil, errors.New("Bedrock returned an empty response")
	}
	result := map[string]any{"stopReason": output.StopReason}
	if output.Usage != nil {
		result["usage"] = map[string]any{
			"inputTokens": aws.ToInt32(output.Usage.InputTokens), "outputTokens": aws.ToInt32(output.Usage.OutputTokens), "totalTokens": aws.ToInt32(output.Usage.TotalTokens),
		}
	}
	if output.Metrics != nil {
		result["metrics"] = map[string]any{"latencyMs": aws.ToInt64(output.Metrics.LatencyMs)}
	}
	if output.AdditionalModelResponseFields != nil {
		value, err := smithyDocumentValue(output.AdditionalModelResponseFields)
		if err != nil {
			return nil, fmt.Errorf("decode Bedrock additional response fields: %w", err)
		}
		result["additionalModelResponseFields"] = value
	}
	messageOutput, ok := output.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return nil, errors.New("Bedrock returned an unsupported output type")
	}
	content := make([]any, 0, len(messageOutput.Value.Content))
	for _, block := range messageOutput.Value.Content {
		switch typed := block.(type) {
		case *types.ContentBlockMemberText:
			content = append(content, map[string]any{"text": typed.Value})
		case *types.ContentBlockMemberToolUse:
			var input any
			if typed.Value.Input != nil {
				var err error
				input, err = smithyDocumentValue(typed.Value.Input)
				if err != nil {
					return nil, err
				}
			}
			content = append(content, map[string]any{"toolUse": map[string]any{
				"toolUseId": aws.ToString(typed.Value.ToolUseId), "name": aws.ToString(typed.Value.Name), "input": input,
			}})
		default:
			return nil, fmt.Errorf("Bedrock returned unsupported content block %T", block)
		}
	}
	result["output"] = map[string]any{"message": map[string]any{"role": messageOutput.Value.Role, "content": content}}
	return json.Marshal(result)
}

type smithyDocument interface {
	MarshalSmithyDocument() ([]byte, error)
}

func smithyDocumentValue(value smithyDocument) (any, error) {
	documentBody, err := value.MarshalSmithyDocument()
	if err != nil {
		return nil, err
	}
	var result any
	decoder := json.NewDecoder(strings.NewReader(string(documentBody)))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}
