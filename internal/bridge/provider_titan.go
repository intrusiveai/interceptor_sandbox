package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/intrusive-ai/interceptor-sandbox/internal/credentials"
)

type BedrockInvokeAPI interface {
	InvokeModel(context.Context, *bedrockruntime.InvokeModelInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error)
}

type titanProvider struct{ client BedrockInvokeAPI }

func LoadTitanProvider(ctx context.Context, config BedrockProviderConfig) (ModelProvider, error) {
	loaded, err := credentials.AWSIdentityConfig(ctx, config.Region, config.Profile, config.Authentication)
	if err != nil {
		return nil, errors.New("Titan embeddings AWS identity unavailable; check --embeddings-auth and its selected credentials")
	}
	if loaded.Region == "" {
		return nil, errors.New("Titan embeddings require --embeddings-aws-region or a region in the selected AWS configuration")
	}
	if loaded.Credentials == nil {
		return nil, errors.New("Titan embeddings AWS credentials unavailable")
	}
	if _, err := loaded.Credentials.Retrieve(ctx); err != nil {
		return nil, errors.New("Titan embeddings AWS credentials unavailable; check the selected identity")
	}
	// This standard AWS endpoint override is host-only; it also permits local
	// deterministic qualification through the real SDK transport.
	if endpoint := os.Getenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME"); endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback())) {
			return nil, errors.New("invalid host Bedrock Runtime endpoint override")
		}
		loaded.BaseEndpoint = aws.String(endpoint)
	}
	// Prevent signed requests from following redirects to another origin.
	loaded.HTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return NewTitanProvider(bedrockruntime.NewFromConfig(loaded, func(o *bedrockruntime.Options) { o.Retryer = aws.NopRetryer{} })), nil
}

func NewTitanProvider(client BedrockInvokeAPI) ModelProvider { return &titanProvider{client: client} }
func (p *titanProvider) Name() string                        { return ProviderBedrockTitan }
func (p *titanProvider) SupportsCodec(codec string) bool     { return codec == CodecTitanEmbeddings }
func (p *titanProvider) Complete(ctx context.Context, request ProviderRequest) (ProviderResponse, error) {
	if p.client == nil || request.Codec != CodecTitanEmbeddings || request.Model != TitanEmbeddingModel {
		return ProviderResponse{}, errors.New("unsupported Titan embeddings request")
	}
	if _, err := titanModel(request.Path); err != nil {
		return ProviderResponse{}, err
	}
	if _, err := titanRequest(request.Body); err != nil {
		return ProviderResponse{}, err
	}
	if int64(len(request.Body)) > request.MaxBodyBytes {
		return ProviderResponse{}, errors.New("Titan request exceeds connector body limit")
	}
	output, err := p.client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{ModelId: aws.String(request.Model), Body: request.Body, ContentType: aws.String("application/json"), Accept: aws.String("application/json")}, func(o *bedrockruntime.Options) {
		o.Retryer = aws.NopRetryer{}
		o.HTTPClient = titanHTTPClient{client: o.HTTPClient, limit: min(request.MaxBodyBytes, maxEmbeddingBodyBytes)}
	})
	if err != nil {
		return titanProviderError(err), nil
	}
	if output == nil || aws.ToString(output.ContentType) != "application/json" || int64(len(output.Body)) > request.MaxBodyBytes {
		return ProviderResponse{}, errors.New("invalid or oversized Titan response")
	}
	if _, err := titanResponseMetadata(request.Body, output.Body); err != nil {
		return ProviderResponse{}, err
	}
	headers := map[string]string{"Content-Type": "application/json"}
	metadata := map[string]string{}
	if id, ok := awsmiddleware.GetRequestIDMetadata(output.ResultMetadata); ok && id != "" && len(id) <= 256 {
		headers["X-Amzn-Requestid"] = id
		metadata["request_id"] = id
	}
	return ProviderResponse{Status: http.StatusOK, Headers: headers, Body: output.Body, RawBody: output.Body, RequestBody: request.Body, Metadata: metadata}, nil
}

func titanProviderError(err error) ProviderResponse {
	code, status, message := "InternalServerException", 502, "Titan embeddings request failed"
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "ValidationException":
			code, status, message = "ValidationException", 400, "Titan rejected the input; check model input limits and parameters"
		case "AccessDeniedException":
			code, status, message = "AccessDeniedException", 403, "Titan access denied; check bedrock:InvokeModel permission and model access"
		case "ThrottlingException":
			code, status, message = "ThrottlingException", 429, "Titan request throttled"
		case "ServiceUnavailableException":
			code, status, message = "ServiceUnavailableException", 503, "Titan service unavailable"
		case "ModelTimeoutException":
			code, status, message = "ModelTimeoutException", 408, "Titan model request timed out"
		case "ModelNotReadyException":
			code, status, message = "ModelNotReadyException", 429, "Titan model is not ready"
		case "ResourceNotFoundException":
			code, status, message = "ResourceNotFoundException", 404, "Titan model unavailable in the configured region"
		case "ModelErrorException":
			code, status, message = "ModelErrorException", 424, "Titan model could not process the request"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		status, message = 504, "Titan request cancelled or deadline exceeded"
	}
	body, _ := json.Marshal(map[string]string{"message": message})
	headers := map[string]string{"Content-Type": "application/json", "X-Amzn-Errortype": code}
	metadata := map[string]string{}
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) && response.Response != nil && response.Response.Response != nil {
		if id := response.Response.Header.Get("X-Amzn-Requestid"); id != "" && len(id) <= 256 {
			headers["X-Amzn-Requestid"] = id
			metadata["request_id"] = id
		}
	}
	return ProviderResponse{Status: status, Headers: headers, Body: body, Metadata: metadata}
}

// Preserve Close while bounding SDK response reads, including error bodies.
type titanHTTPClient struct {
	client aws.HTTPClient
	limit  int64
}

func (c titanHTTPClient) Do(request *http.Request) (*http.Response, error) {
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	response.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(response.Body, c.limit+1), response.Body}
	return response, nil
}
