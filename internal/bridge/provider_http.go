package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
)

type HTTPProviderConfig struct {
	BaseURL          string
	APIKey           string
	CredentialHeader string
	CredentialPrefix string
	Headers          map[string]string
	Metadata         map[string]string
	Client           *http.Client
}

type httpModelProvider struct {
	name             string
	endpoints        map[string]string
	baseURL          *url.URL
	apiKey           string
	credentialHeader string
	credentialPrefix string
	headers          map[string]string
	metadata         map[string]string
	client           *http.Client
}

func NewOpenAIEmbeddingsProvider(config HTTPProviderConfig) (ModelProvider, error) {
	return newHTTPModelProvider(ProviderOpenAICompatible, map[string]string{CodecOpenAIEmbeddings: "embeddings"}, config)
}

func NewOpenAIChatProvider(config HTTPProviderConfig) (ModelProvider, error) {
	return newHTTPModelProvider(ProviderOpenAIChat, map[string]string{CodecOpenAIChat: "chat/completions"}, config)
}

func NewLiteLLMProvider(config HTTPProviderConfig) (ModelProvider, error) {
	return newHTTPModelProvider(ProviderLiteLLM, map[string]string{
		CodecOpenAIChat: "chat/completions", CodecOpenAIResponses: "responses",
	}, config)
}

func NewOpenAIResponsesProvider(config HTTPProviderConfig) (ModelProvider, error) {
	return newHTTPModelProvider(ProviderOpenAIResponses, map[string]string{CodecOpenAIResponses: "responses"}, config)
}

func NewAnthropicMessagesProvider(config HTTPProviderConfig) (ModelProvider, error) {
	if config.CredentialHeader == "" {
		config.CredentialHeader = "X-Api-Key"
	}
	if config.Headers == nil {
		config.Headers = map[string]string{}
	}
	if config.Headers["Anthropic-Version"] == "" {
		config.Headers["Anthropic-Version"] = "2023-06-01"
	}
	return newHTTPModelProvider(ProviderAnthropicMessages, map[string]string{CodecAnthropicMessages: "v1/messages"}, config)
}

func NewGeminiAPIProvider(config HTTPProviderConfig) (ModelProvider, error) {
	if config.CredentialHeader == "" {
		config.CredentialHeader = "X-Goog-Api-Key"
	}
	return newHTTPModelProvider(ProviderGeminiAPI, map[string]string{CodecGoogleGenerateContent: "models/{model}:generateContent"}, config)
}

func NewVertexGeminiProvider(config HTTPProviderConfig) (ModelProvider, error) {
	return newHTTPModelProvider(ProviderVertexGemini, map[string]string{CodecGoogleGenerateContent: "publishers/google/models/{model}:generateContent"}, config)
}

func NewAzureOpenAIProvider(config HTTPProviderConfig) (ModelProvider, error) {
	if config.CredentialHeader == "" && config.APIKey != "" {
		config.CredentialHeader = "Api-Key"
	}
	return newHTTPModelProvider(ProviderAzureOpenAI, map[string]string{
		CodecOpenAIChat: "chat/completions", CodecOpenAIResponses: "responses",
	}, config)
}

func newHTTPModelProvider(name string, endpoints map[string]string, config HTTPProviderConfig) (ModelProvider, error) {
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || baseURL.Scheme != "http" && baseURL.Scheme != "https" || baseURL.Host == "" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("provider URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	previousRedirect := copyClient.CheckRedirect
	copyClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if request.URL.Scheme != baseURL.Scheme || request.URL.Host != baseURL.Host {
			return errors.New("provider redirected to a different origin")
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		if len(via) >= 10 {
			return errors.New("too many provider redirects")
		}
		return nil
	}
	credentialHeader := config.CredentialHeader
	credentialPrefix := config.CredentialPrefix
	if credentialHeader == "" {
		credentialHeader, credentialPrefix = "Authorization", "Bearer "
	}
	headers := make(map[string]string, len(config.Headers))
	for name, value := range config.Headers {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return nil, errors.New("provider headers must not contain empty names or line breaks")
		}
		headers[http.CanonicalHeaderKey(name)] = value
	}
	metadata := make(map[string]string, len(config.Metadata))
	for key, value := range config.Metadata {
		metadata[key] = value
	}
	return &httpModelProvider{name: name, endpoints: endpoints, baseURL: baseURL, apiKey: config.APIKey, credentialHeader: http.CanonicalHeaderKey(credentialHeader), credentialPrefix: credentialPrefix, headers: headers, metadata: metadata, client: &copyClient}, nil
}

func (p *httpModelProvider) Name() string { return p.name }

func (p *httpModelProvider) SupportsCodec(codec string) bool {
	_, ok := p.endpoints[codec]
	return ok
}

func (p *httpModelProvider) Complete(ctx context.Context, input ProviderRequest) (ProviderResponse, error) {
	endpoint, ok := p.endpoints[input.Codec]
	if !ok {
		return ProviderResponse{}, fmt.Errorf("provider %s does not accept codec %q", p.name, input.Codec)
	}
	body := append([]byte(nil), input.Body...)
	if int64(len(body)) > input.MaxBodyBytes {
		return ProviderResponse{}, errors.New("provider request exceeds connector body limit")
	}
	if strings.Contains(endpoint, "{model}") {
		if input.Model == "" || strings.ContainsAny(input.Model, "/?#") {
			return ProviderResponse{}, errors.New("provider model identifier is invalid for a constructed route")
		}
		endpoint = strings.ReplaceAll(endpoint, "{model}", url.PathEscape(input.Model))
	}
	destination := *p.baseURL
	if strings.HasSuffix(strings.TrimSuffix(destination.Path, "/"), "/v1") {
		endpoint = strings.TrimPrefix(endpoint, "v1/")
	}
	destination.Path = path.Join(strings.TrimSuffix(destination.Path, "/"), endpoint)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, destination.String(), bytes.NewReader(body))
	if err != nil {
		return ProviderResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range p.headers {
		request.Header.Set(name, value)
	}
	for _, name := range []string{"OpenAI-Organization", "OpenAI-Project"} {
		if value := input.Headers[name]; value != "" {
			request.Header.Set(name, value)
		}
	}
	if p.apiKey != "" {
		request.Header.Set(p.credentialHeader, p.credentialPrefix+p.apiKey)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return ProviderResponse{}, err
	}
	defer response.Body.Close()
	responseBody, err := readLimit(response.Body, input.MaxBodyBytes)
	if err != nil {
		return ProviderResponse{}, fmt.Errorf("read response: %w", err)
	}
	headers := map[string]string{}
	for _, header := range []string{"Content-Type", "X-Request-ID", "OpenAI-Request-ID", "Retry-After", "Retry-After-Ms"} {
		if value := response.Header.Get(header); value != "" {
			headers[header] = value
		}
	}
	if headers["Content-Type"] == "" && response.StatusCode >= 200 && response.StatusCode < 300 {
		headers["Content-Type"] = "application/json"
	}
	return ProviderResponse{Status: response.StatusCode, Headers: headers, Body: responseBody, RequestBody: body, RawBody: append([]byte(nil), responseBody...), Metadata: p.metadata}, nil
}
