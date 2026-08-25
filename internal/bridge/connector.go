package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
)

const maxConnectorFrameBytes = 32 << 20

type ModelFrame struct {
	SessionID string            `json:"session_id"`
	RequestID uint64            `json:"request_id"`
	Codec     string            `json:"codec"`
	Path      string            `json:"path"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      []byte            `json:"body"`
	TimeoutMS int64             `json:"timeout_ms"`
	CreatedAt time.Time         `json:"created_at,omitempty"`
}

var bridgeErrorSequence atomic.Uint64

type ModelResult struct {
	Status             int               `json:"status"`
	Headers            map[string]string `json:"headers,omitempty"`
	Body               []byte            `json:"body"`
	FinalRequest       []byte            `json:"final_request"`
	FinalRequestDigest string            `json:"final_request_digest"`
}

type Connector interface {
	Forward(context.Context, ModelFrame) (ModelResult, error)
}

type HostConnectorConfig struct {
	SessionID    string
	ProviderURL  string
	APIKey       string
	AllowedCodec string
	AllowedModel []string
	MaxBodyBytes int64
	HTTPClient   *http.Client
}

type HostConnector struct {
	config  HostConnectorConfig
	baseURL *url.URL
	client  *http.Client
	lastID  atomic.Uint64
}

func NewHostConnector(config HostConnectorConfig) (*HostConnector, error) {
	if config.SessionID == "" {
		return nil, errors.New("connector session ID is required")
	}
	baseURL, err := url.Parse(config.ProviderURL)
	if err != nil || baseURL.Scheme != "http" && baseURL.Scheme != "https" || baseURL.Host == "" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("provider URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if config.AllowedCodec == "" {
		config.AllowedCodec = "openai_chat_completions"
	}
	if config.MaxBodyBytes <= 0 {
		config.MaxBodyBytes = 16 << 20
	}
	client := config.HTTPClient
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
	return &HostConnector{config: config, baseURL: baseURL, client: &copyClient}, nil
}

func (c *HostConnector) Forward(ctx context.Context, frame ModelFrame) (ModelResult, error) {
	if frame.SessionID != c.config.SessionID || frame.RequestID == 0 {
		return ModelResult{}, errors.New("connector rejected session identity or non-monotonic request ID")
	}
	for {
		last := c.lastID.Load()
		if frame.RequestID <= last || !c.lastID.CompareAndSwap(last, frame.RequestID) {
			if frame.RequestID <= last {
				return ModelResult{}, errors.New("connector rejected session identity or non-monotonic request ID")
			}
			continue
		}
		break
	}
	if frame.Codec != c.config.AllowedCodec {
		return ModelResult{}, fmt.Errorf("connector rejected codec %q", frame.Codec)
	}
	if int64(len(frame.Body)) > c.config.MaxBodyBytes {
		return ModelResult{}, errors.New("model request exceeds connector body limit")
	}
	if !strings.HasPrefix(frame.Path, "/") || strings.Contains(frame.Path, "..") || strings.ContainsAny(frame.Path, "?#") {
		return ModelResult{}, errors.New("connector request path is invalid")
	}
	var requestEnvelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(frame.Body, &requestEnvelope); err != nil || requestEnvelope.Model == "" {
		return ModelResult{}, errors.New("model request must contain a model identifier")
	}
	if len(c.config.AllowedModel) > 0 && !contains(c.config.AllowedModel, requestEnvelope.Model) {
		return ModelResult{}, fmt.Errorf("model %q is not allowed", requestEnvelope.Model)
	}
	timeout := time.Duration(frame.TimeoutMS) * time.Millisecond
	if timeout <= 0 || timeout > 10*time.Minute {
		return ModelResult{}, errors.New("connector timeout is out of bounds")
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	destination := *c.baseURL
	framePath := strings.TrimPrefix(frame.Path, "/")
	if strings.HasSuffix(strings.TrimSuffix(c.baseURL.Path, "/"), "/v1") {
		framePath = strings.TrimPrefix(framePath, "v1/")
	}
	destination.Path = path.Join(strings.TrimSuffix(c.baseURL.Path, "/"), framePath)
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, destination.String(), bytes.NewReader(frame.Body))
	if err != nil {
		return ModelResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	if value := frame.Headers["OpenAI-Organization"]; value != "" {
		request.Header.Set("OpenAI-Organization", value)
	}
	if c.config.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return ModelResult{}, fmt.Errorf("provider request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := readLimit(response.Body, c.config.MaxBodyBytes)
	if err != nil {
		return ModelResult{}, fmt.Errorf("provider response: %w", err)
	}
	result := ModelResult{Status: response.StatusCode, Body: body, FinalRequest: append([]byte(nil), frame.Body...), FinalRequestDigest: digest.Bytes(frame.Body), Headers: map[string]string{}}
	for _, header := range []string{"Content-Type", "X-Request-ID", "OpenAI-Request-ID"} {
		if value := response.Header.Get(header); value != "" {
			result.Headers[header] = value
		}
	}
	return result, nil
}

func (c *HostConnector) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/v1/model" {
		http.NotFound(writer, request)
		return
	}
	body, err := readLimit(request.Body, maxConnectorFrameBytes)
	if err != nil {
		writeBridgeError(writer, http.StatusRequestEntityTooLarge, "frame_too_large", err.Error())
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var frame ModelFrame
	if err := decoder.Decode(&frame); err != nil {
		writeBridgeError(writer, http.StatusBadRequest, "invalid_frame", err.Error())
		return
	}
	result, err := c.Forward(request.Context(), frame)
	if err != nil {
		writeBridgeError(writer, http.StatusBadGateway, "connector_rejected", err.Error())
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(result)
}

type UnixConnectorClient struct {
	client *http.Client
}

func NewUnixConnectorClient(socketPath string) *UnixConnectorClient {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}
	return &UnixConnectorClient{client: &http.Client{Transport: transport}}
}

func (c *UnixConnectorClient) Forward(ctx context.Context, frame ModelFrame) (ModelResult, error) {
	encoded, err := json.Marshal(frame)
	if err != nil {
		return ModelResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/v1/model", bytes.NewReader(encoded))
	if err != nil {
		return ModelResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return ModelResult{}, err
	}
	defer response.Body.Close()
	body, err := readLimit(response.Body, maxConnectorFrameBytes)
	if err != nil {
		return ModelResult{}, err
	}
	if response.StatusCode != http.StatusOK {
		return ModelResult{}, fmt.Errorf("host connector returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var result ModelResult
	if err := decoder.Decode(&result); err != nil {
		return ModelResult{}, err
	}
	if result.Status < 100 || result.Status > 599 || !digest.Valid(result.FinalRequestDigest) || digest.Bytes(result.FinalRequest) != result.FinalRequestDigest {
		return ModelResult{}, errors.New("host connector returned invalid result")
	}
	return result, nil
}

func ListenUnix(socketPath string, handler http.Handler) (*http.Server, net.Listener, error) {
	if err := os.MkdirAll(pathDir(socketPath), 0o750); err != nil {
		return nil, nil, err
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, nil, errors.New("refusing to replace non-socket connector path")
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, nil, err
	}
	if err := makeSocketPrivate(socketPath); err != nil {
		listener.Close()
		return nil, nil, err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	return server, listener, nil
}

func makeSocketPrivate(socketPath string) error {
	if err := os.Chmod(socketPath, 0o600); err == nil {
		return nil
	} else {
		// Docker Desktop bind mounts can reject chmod on Unix sockets. A
		// socket created under the normal 0022 (or stricter) umask is still
		// private for connection purposes because only its owner may write.
		info, statErr := os.Lstat(socketPath)
		if statErr == nil && info.Mode()&os.ModeSocket != 0 && info.Mode().Perm()&0o022 == 0 {
			return nil
		}
		return err
	}
}

func readLimit(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("body exceeds %d-byte limit", limit)
	}
	return body, nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func pathDir(value string) string {
	index := strings.LastIndex(value, string(os.PathSeparator))
	if index <= 0 {
		return "."
	}
	return value[:index]
}

func writeBridgeError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]any{"code": code, "message": message, "request_id": fmt.Sprintf("req-%d", bridgeErrorSequence.Add(1))}})
}
