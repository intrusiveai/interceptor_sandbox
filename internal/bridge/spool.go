package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
)

type spoolResponse struct {
	Result *ModelResult `json:"result,omitempty"`
	Error  string       `json:"error,omitempty"`
}

// SpoolConnector transports model requests using atomic regular files. Regular
// files work across Docker Desktop bind mounts, unlike Unix sockets.
type SpoolConnector struct {
	root string
}

func NewSpoolConnector(root string) (*SpoolConnector, error) {
	if root == "" {
		return nil, errors.New("connector spool directory is required")
	}
	for _, directory := range modelSpoolDirectories(root) {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, err
		}
	}
	return &SpoolConnector{root: root}, nil
}

func (c *SpoolConnector) Forward(ctx context.Context, frame ModelFrame) (ModelResult, error) {
	if frame.CreatedAt.IsZero() {
		frame.CreatedAt = time.Now().UTC()
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return ModelResult{}, err
	}
	name := fmt.Sprintf("%020d.json", frame.RequestID)
	requestPath := filepath.Join(requestsDirectory(c.root), name)
	responsePath := filepath.Join(responsesDirectory(c.root), name)
	cancelPath := filepath.Join(cancellationsDirectory(c.root), name)
	if err := fsutil.WriteFile(requestPath, encoded, 0o600); err != nil {
		return ModelResult{}, err
	}
	defer os.Remove(requestPath)
	defer os.Remove(responsePath)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, readErr := os.ReadFile(responsePath)
		if readErr == nil {
			var response spoolResponse
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&response); err != nil {
				return ModelResult{}, fmt.Errorf("decode connector response: %w", err)
			}
			if response.Error != "" {
				return ModelResult{}, errors.New(response.Error)
			}
			if response.Result == nil || response.Result.Status < 100 || response.Result.Status > 599 || !digest.Valid(response.Result.FinalRequestDigest) || digest.Bytes(response.Result.FinalRequest) != response.Result.FinalRequestDigest {
				return ModelResult{}, errors.New("host connector returned invalid result")
			}
			return *response.Result, nil
		}
		if !os.IsNotExist(readErr) {
			return ModelResult{}, readErr
		}
		select {
		case <-ctx.Done():
			_ = fsutil.WriteFile(cancelPath, []byte(ctx.Err().Error()), 0o600)
			return ModelResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// ServeSpool services requests until the context is canceled. A request and
// response remain on disk until the in-container client consumes the result.
func ServeSpool(ctx context.Context, root string, connector Connector) error {
	if connector == nil {
		return errors.New("host connector is required")
	}
	for _, directory := range modelSpoolDirectories(root) {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return err
		}
	}
	if err := cleanupModelSpool(root, time.Now().UTC()); err != nil {
		return err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	cleanup := time.NewTicker(30 * time.Second)
	defer cleanup.Stop()
	for {
		if err := serviceSpoolBatch(ctx, root, connector); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-cleanup.C:
			if err := cleanupModelSpool(root, time.Now().UTC()); err != nil {
				return err
			}
		case <-ticker.C:
		}
	}
}

func serviceSpoolBatch(ctx context.Context, root string, connector Connector) error {
	entries, err := os.ReadDir(requestsDirectory(root))
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
		requestPath := filepath.Join(requestsDirectory(root), name)
		processingPath := filepath.Join(processingDirectory(root), name)
		responsePath := filepath.Join(responsesDirectory(root), name)
		cancelPath := filepath.Join(cancellationsDirectory(root), name)
		if _, err := os.Stat(responsePath); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if _, err := os.Lstat(cancelPath); err == nil {
			_ = os.Remove(requestPath)
			_ = os.Remove(cancelPath)
			continue
		}
		if err := os.Rename(requestPath, processingPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		data, err := readRegularFile(processingPath, maxConnectorFrameBytes)
		if err != nil {
			_ = os.Remove(processingPath)
			continue
		}
		var frame ModelFrame
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&frame)
		if decodeErr == nil {
			if trailingErr := decoder.Decode(&struct{}{}); trailingErr != io.EOF {
				decodeErr = errors.New("connector request contains trailing JSON")
			}
		}
		response := spoolResponse{}
		if decodeErr != nil {
			response.Error = "invalid connector request: " + decodeErr.Error()
		} else {
			timeout := time.Duration(frame.TimeoutMS) * time.Millisecond
			if timeout <= 0 || timeout > defaultHTTPTimeout {
				timeout = defaultHTTPTimeout
			}
			requestContext, cancel := context.WithTimeout(ctx, timeout)
			watchDone := make(chan struct{})
			go watchHTTPCancellation(requestContext, cancelPath, cancel, watchDone)
			result, forwardErr := connector.Forward(requestContext, frame)
			close(watchDone)
			if requestContext.Err() != nil {
				cancel()
				_ = os.Remove(processingPath)
				_ = os.Remove(cancelPath)
				continue
			}
			cancel()
			if forwardErr != nil {
				response.Error = forwardErr.Error()
			} else {
				response.Result = &result
			}
		}
		encoded, _ := json.Marshal(response)
		if err := fsutil.WriteFile(responsePath, encoded, 0o600); err != nil {
			return err
		}
		_ = os.Remove(processingPath)
		_ = os.Remove(cancelPath)
	}
	return nil
}

func requestsDirectory(root string) string      { return filepath.Join(root, "requests") }
func responsesDirectory(root string) string     { return filepath.Join(root, "responses") }
func processingDirectory(root string) string    { return filepath.Join(root, "processing") }
func cancellationsDirectory(root string) string { return filepath.Join(root, "cancellations") }

func modelSpoolDirectories(root string) []string {
	return []string{requestsDirectory(root), processingDirectory(root), responsesDirectory(root), cancellationsDirectory(root)}
}

func cleanupModelSpool(root string, now time.Time) error {
	for _, directory := range modelSpoolDirectories(root) {
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
			if directory == responsesDirectory(root) {
				expires = info.ModTime().Add(orphanGrace)
			} else if data, readErr := readRegularFile(path, maxConnectorFrameBytes); readErr == nil {
				var frame ModelFrame
				if json.Unmarshal(data, &frame) == nil && !frame.CreatedAt.IsZero() && frame.TimeoutMS > 0 {
					expires = frame.CreatedAt.Add(time.Duration(frame.TimeoutMS)*time.Millisecond + orphanGrace)
				}
			}
			if now.After(expires) {
				_ = os.Remove(path)
			}
		}
	}
	return nil
}
