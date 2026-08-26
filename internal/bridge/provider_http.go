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
	BaseURL string
	APIKey  string
	Client  *http.Client
}

type httpModelProvider struct {
	name      string
	endpoints map[string]string
	baseURL   *url.URL
	apiKey    string
	client    *http.Client
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
	return &httpModelProvider{name: name, endpoints: endpoints, baseURL: baseURL, apiKey: config.APIKey, client: &copyClient}, nil
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
	for _, name := range []string{"OpenAI-Organization", "OpenAI-Project"} {
		if value := input.Headers[name]; value != "" {
			request.Header.Set(name, value)
		}
	}
	if p.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+p.apiKey)
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
	for _, header := range []string{"Content-Type", "X-Request-ID", "OpenAI-Request-ID"} {
		if value := response.Header.Get(header); value != "" {
			headers[header] = value
		}
	}
	if headers["Content-Type"] == "" && response.StatusCode >= 200 && response.StatusCode < 300 {
		headers["Content-Type"] = "application/json"
	}
	return ProviderResponse{Status: response.StatusCode, Headers: headers, Body: responseBody, RequestBody: body, RawBody: append([]byte(nil), responseBody...)}, nil
}
