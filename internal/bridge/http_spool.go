package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
)

const (
	ApplicationChannel = "application"
	ControlChannel     = "control"
	defaultHTTPMax     = int64(32 << 20)
	defaultHTTPTimeout = 10 * time.Minute
	orphanGrace        = time.Minute
)

type HTTPRequestFrame struct {
	ID        string              `json:"id"`
	Channel   string              `json:"channel"`
	Method    string              `json:"method"`
	Path      string              `json:"path"`
	Headers   map[string][]string `json:"headers,omitempty"`
	Body      []byte              `json:"body,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
	ExpiresAt time.Time           `json:"expires_at"`
}

type HTTPResponseFrame struct {
	ID        string              `json:"id"`
	Status    int                 `json:"status"`
	Headers   map[string][]string `json:"headers,omitempty"`
	Body      []byte              `json:"body,omitempty"`
	Error     string              `json:"error,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
	ExpiresAt time.Time           `json:"expires_at"`
}

type HTTPSpoolClient struct {
	root       string
	maxBytes   int64
	timeout    time.Duration
	prefix     string
	next       atomic.Uint64
	pollPeriod time.Duration
}

func NewHTTPSpoolClient(root string, maxBytes int64, timeout time.Duration) (*HTTPSpoolClient, error) {
	if root == "" {
		return nil, errors.New("HTTP spool directory is required")
	}
	if maxBytes <= 0 {
		maxBytes = defaultHTTPMax
	}
	if timeout <= 0 || timeout > defaultHTTPTimeout {
		timeout = defaultHTTPTimeout
	}
	if err := prepareHTTPSpool(root); err != nil {
		return nil, err
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	return &HTTPSpoolClient{root: root, maxBytes: maxBytes, timeout: timeout, prefix: hex.EncodeToString(random), pollPeriod: 10 * time.Millisecond}, nil
}

func (c *HTTPSpoolClient) Do(ctx context.Context, channel, method, path string, headers http.Header, body []byte) (HTTPResponseFrame, error) {
	if channel != ApplicationChannel && channel != ControlChannel {
		return HTTPResponseFrame{}, fmt.Errorf("unsupported HTTP spool channel %q", channel)
	}
	if method == "" || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\x00\r\n") {
		return HTTPResponseFrame{}, errors.New("HTTP spool request requires a method and absolute path")
	}
	if int64(len(body)) > c.maxBytes {
		return HTTPResponseFrame{}, fmt.Errorf("HTTP spool request exceeds %d-byte limit", c.maxBytes)
	}
	now := time.Now().UTC()
	expires := now.Add(c.timeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expires) {
		expires = deadline.UTC()
	}
	id := fmt.Sprintf("%020d-%020d-%s", now.UnixNano(), c.next.Add(1), c.prefix)
	frame := HTTPRequestFrame{ID: id, Channel: channel, Method: method, Path: path, Headers: cloneHeader(headers), Body: body, CreatedAt: now, ExpiresAt: expires}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return HTTPResponseFrame{}, err
	}
	requestPath := httpSpoolPath(c.root, "requests", id)
	responsePath := httpSpoolPath(c.root, "responses", id)
	cancelPath := httpSpoolPath(c.root, "cancellations", id)
	if err := fsutil.WriteFile(requestPath, encoded, 0o600); err != nil {
		return HTTPResponseFrame{}, err
	}
	defer os.Remove(requestPath)
	defer os.Remove(responsePath)
	ticker := time.NewTicker(c.pollPeriod)
	defer ticker.Stop()
	for {
		data, readErr := readRegularFile(responsePath, jsonFrameLimit(c.maxBytes))
		if readErr == nil {
			var response HTTPResponseFrame
			if err := decodeStrictJSON(data, &response); err != nil {
				return HTTPResponseFrame{}, fmt.Errorf("decode HTTP spool response: %w", err)
			}
			if response.ID != id || response.Status < 100 || response.Status > 599 || int64(len(response.Body)) > c.maxBytes {
				return HTTPResponseFrame{}, errors.New("HTTP spool returned an invalid response")
			}
			if response.Error != "" {
				return HTTPResponseFrame{}, errors.New(response.Error)
			}
			_ = os.Remove(cancelPath)
			return response, nil
		}
		if !os.IsNotExist(readErr) {
			return HTTPResponseFrame{}, readErr
		}
		select {
		case <-ctx.Done():
			_ = fsutil.WriteFile(cancelPath, []byte(ctx.Err().Error()), 0o600)
			return HTTPResponseFrame{}, ctx.Err()
		case <-ticker.C:
			if time.Now().After(expires) {
				_ = fsutil.WriteFile(cancelPath, []byte("deadline exceeded"), 0o600)
				return HTTPResponseFrame{}, context.DeadlineExceeded
			}
		}
	}
}

type HTTPSpoolServerConfig struct {
	// ControlClass opts self-serializing handlers into bounded concurrent dispatch.
	// Return "operation" for ordinary work, "priority" for closure/status, or ""
	// for the legacy serial control path. Classification does not grant authority.
	ControlClass             func(HTTPRequestFrame) string
	Root                     string
	Application              http.Handler
	Control                  http.Handler
	MaxBytes                 int64
	MaxConcurrentApplication int
	PollPeriod               time.Duration
	CleanupPeriod            time.Duration
}

// ServeHTTPSpool dispatches application requests concurrently up to the
// configured bound. Legacy control requests retain stable serial ordering;
// an explicitly configured handler may own its own mutation serialization.
func ServeHTTPSpool(ctx context.Context, config HTTPSpoolServerConfig) error {
	if config.Root == "" || config.Application == nil || config.Control == nil {
		return errors.New("HTTP spool requires a root and both handlers")
	}
	if config.MaxBytes <= 0 {
		config.MaxBytes = defaultHTTPMax
	}
	if config.MaxConcurrentApplication <= 0 {
		config.MaxConcurrentApplication = 8
	}
	if config.PollPeriod <= 0 {
		config.PollPeriod = 10 * time.Millisecond
	}
	if config.CleanupPeriod <= 0 {
		config.CleanupPeriod = 30 * time.Second
	}
	if err := prepareHTTPSpool(config.Root); err != nil {
		return err
	}
	if err := cleanupHTTPSpool(config.Root, time.Now().UTC()); err != nil {
		return err
	}
	semaphore := make(chan struct{}, config.MaxConcurrentApplication)
	controlSlots := make(chan struct{}, 32)
	prioritySlots := make(chan struct{}, 4)
	var workers sync.WaitGroup
	defer workers.Wait()
	poll := time.NewTicker(config.PollPeriod)
	cleanup := time.NewTicker(config.CleanupPeriod)
	defer poll.Stop()
	defer cleanup.Stop()
	for {
		if err := serviceHTTPSpoolBatch(ctx, config, semaphore, controlSlots, prioritySlots, &workers); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-cleanup.C:
			if err := cleanupHTTPSpool(config.Root, time.Now().UTC()); err != nil {
				return err
			}
		case <-poll.C:
		}
	}
}

func serviceHTTPSpoolBatch(ctx context.Context, config HTTPSpoolServerConfig, semaphore, controlSlots, prioritySlots chan struct{}, workers *sync.WaitGroup) error {
	entries, err := os.ReadDir(filepath.Join(config.Root, "requests"))
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		requestPath := filepath.Join(config.Root, "requests", name)
		processingPath := filepath.Join(config.Root, "processing", name)
		if err := os.Rename(requestPath, processingPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		data, err := readRegularFile(processingPath, jsonFrameLimit(config.MaxBytes))
		if err != nil {
			_ = os.Remove(processingPath)
			continue
		}
		var frame HTTPRequestFrame
		if err := decodeStrictJSON(data, &frame); err != nil || !validHTTPRequestFrame(frame, name, config.MaxBytes) {
			_ = os.Remove(processingPath)
			continue
		}
		slots := semaphore
		if frame.Channel == ControlChannel {
			class := ""
			if config.ControlClass != nil {
				class = config.ControlClass(frame)
			}
			switch class {
			case "operation":
				slots = controlSlots
			case "priority":
				slots = prioritySlots
			default:
				processHTTPSpoolRequest(ctx, config, frame, processingPath)
				continue
			}
		}
		select {
		case slots <- struct{}{}:
		default:
			// A saturated lane must not prevent scanning for closure/status requests.
			if err := os.Rename(processingPath, requestPath); err != nil {
				return err
			}
			continue
		}
		workers.Add(1)
		go func(frame HTTPRequestFrame, path string, slots chan struct{}) {
			defer workers.Done()
			defer func() { <-slots }()
			processHTTPSpoolRequest(ctx, config, frame, path)
		}(frame, processingPath, slots)
	}
	return nil
}

func jsonFrameLimit(maximumBodyBytes int64) int64 {
	if maximumBodyBytes <= 0 {
		return 0
	}
	return 4*((maximumBodyBytes+2)/3) + (1 << 20)
}

func processHTTPSpoolRequest(parent context.Context, config HTTPSpoolServerConfig, frame HTTPRequestFrame, processingPath string) {
	defer os.Remove(processingPath)
	cancelPath := httpSpoolPath(config.Root, "cancellations", frame.ID)
	defer os.Remove(cancelPath)
	if time.Now().After(frame.ExpiresAt) {
		publishHTTPSpoolError(config.Root, frame, http.StatusGatewayTimeout, "request expired before execution")
		return
	}
	requestContext, cancel := context.WithDeadline(parent, frame.ExpiresAt)
	defer cancel()
	watchDone := make(chan struct{})
	go watchHTTPCancellation(requestContext, cancelPath, cancel, watchDone)
	handler := config.Application
	if frame.Channel == ControlChannel {
		handler = config.Control
	}
	request, err := http.NewRequestWithContext(requestContext, frame.Method, "http://spool"+frame.Path, bytes.NewReader(frame.Body))
	if err != nil {
		close(watchDone)
		publishHTTPSpoolError(config.Root, frame, http.StatusBadRequest, err.Error())
		return
	}
	request.Header = cloneHeader(frame.Headers)
	response := newBoundedResponseWriter(config.MaxBytes)
	handler.ServeHTTP(response, request)
	close(watchDone)
	if requestContext.Err() != nil {
		return
	}
	if response.overflow {
		publishHTTPSpoolError(config.Root, frame, http.StatusBadGateway, "handler response exceeds spool limit")
		return
	}
	now := time.Now().UTC()
	result := HTTPResponseFrame{ID: frame.ID, Status: response.statusCode(), Headers: cloneHeader(response.header), Body: response.body.Bytes(), CreatedAt: now, ExpiresAt: now.Add(orphanGrace)}
	encoded, _ := json.Marshal(result)
	_ = fsutil.WriteFile(httpSpoolPath(config.Root, "responses", frame.ID), encoded, 0o600)
}

func watchHTTPCancellation(ctx context.Context, path string, cancel context.CancelFunc, done <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Lstat(path); err == nil {
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
		}
	}
}

func publishHTTPSpoolError(root string, frame HTTPRequestFrame, status int, message string) {
	now := time.Now().UTC()
	response := HTTPResponseFrame{ID: frame.ID, Status: status, Error: message, CreatedAt: now, ExpiresAt: now.Add(orphanGrace)}
	encoded, _ := json.Marshal(response)
	_ = fsutil.WriteFile(httpSpoolPath(root, "responses", frame.ID), encoded, 0o600)
}

func prepareHTTPSpool(root string) error {
	for _, name := range []string{"requests", "processing", "responses", "cancellations"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func cleanupHTTPSpool(root string, now time.Time) error {
	for _, name := range []string{"requests", "processing", "responses", "cancellations"} {
		directory := filepath.Join(root, name)
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			expires := info.ModTime().Add(defaultHTTPTimeout + orphanGrace)
			if name == "requests" || name == "processing" || name == "responses" {
				data, readErr := readRegularFile(path, defaultHTTPMax+(1<<20))
				if readErr == nil {
					var envelope struct {
						ExpiresAt time.Time `json:"expires_at"`
					}
					if json.Unmarshal(data, &envelope) == nil && !envelope.ExpiresAt.IsZero() {
						expires = envelope.ExpiresAt.Add(orphanGrace)
					}
				}
			}
			if now.After(expires) {
				_ = os.Remove(path)
			}
		}
	}
	return nil
}

func validHTTPRequestFrame(frame HTTPRequestFrame, filename string, maxBytes int64) bool {
	return filename == frame.ID+".json" && (frame.Channel == ApplicationChannel || frame.Channel == ControlChannel) && frame.Method != "" && strings.HasPrefix(frame.Path, "/") && !strings.ContainsAny(frame.Path, "\x00\r\n") && !frame.CreatedAt.IsZero() && frame.ExpiresAt.After(frame.CreatedAt) && int64(len(frame.Body)) <= maxBytes
}

func httpSpoolPath(root, directory, id string) string {
	return filepath.Join(root, directory, id+".json")
}

func readRegularFile(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxBytes {
		return nil, errors.New("spool frame is not a bounded regular file")
	}
	return os.ReadFile(path)
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("frame contains trailing JSON")
	}
	return nil
}

func cloneHeader(header http.Header) http.Header {
	result := make(http.Header, len(header))
	for name, values := range header {
		result[name] = append([]string(nil), values...)
	}
	return result
}

type boundedResponseWriter struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	maxBytes int64
	overflow bool
}

func newBoundedResponseWriter(maxBytes int64) *boundedResponseWriter {
	return &boundedResponseWriter{header: http.Header{}, maxBytes: maxBytes}
}

func (w *boundedResponseWriter) Header() http.Header { return w.header }
func (w *boundedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *boundedResponseWriter) Write(content []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if int64(w.body.Len()+len(content)) > w.maxBytes {
		w.overflow = true
		return 0, errors.New("response exceeds spool limit")
	}
	return w.body.Write(content)
}
func (w *boundedResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
