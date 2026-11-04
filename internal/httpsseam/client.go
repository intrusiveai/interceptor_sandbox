package httpsseam

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
)

type Client struct {
	ConnectionFailure func(string, string)
	session           string
	maximum           int64
	timeout           time.Duration
	stripped          []string
	spool             *bridge.HTTPSpoolClient
	unix              *http.Client
	next              atomic.Uint64
	mu                sync.Mutex
	paused            bool
	active            int
	idle              chan struct{}
}

func NewClient(session, transport, path string, maximum int64, timeout time.Duration, stripped []string) (*Client, error) {
	c := &Client{session: session, maximum: maximum, timeout: timeout, stripped: stripped, idle: make(chan struct{})}
	close(c.idle)
	if transport == "spool" {
		var err error
		c.spool, err = bridge.NewHTTPSpoolClient(path, FrameLimit(maximum), timeout)
		if err != nil {
			return nil, err
		}
	} else if transport == "unix" {
		c.unix = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}, DisableKeepAlives: true}, Timeout: timeout}
	} else {
		return nil, errors.New("unknown HTTPS seam transport")
	}
	return c, nil
}

// Pause stops admission before waiting; no turn context is needed for background
// SDK traffic. Resume only after the coordinated snapshot boundary is released.
func (c *Client) Pause(ctx context.Context) error {
	c.mu.Lock()
	c.paused = true
	idle := c.idle
	c.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *Client) Resume() { c.mu.Lock(); c.paused = false; c.mu.Unlock() }
func (c *Client) Handler(id string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || !validExchange(r.Method, r.RequestURI, r.Header) {
			if c.ConnectionFailure != nil {
				c.ConnectionFailure(id, "invalid_or_plaintext_request")
			}
			http.Error(w, "HTTPS seam requires bounded HTTPS request/response", 400)
			return
		}
		c.mu.Lock()
		if c.paused {
			c.mu.Unlock()
			http.Error(w, "HTTPS seam is quiescing", 503)
			return
		}
		if c.active == 0 {
			c.idle = make(chan struct{})
		}
		c.active++
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			c.active--
			if c.active == 0 {
				close(c.idle)
			}
			c.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(r.Context(), c.timeout)
		defer cancel()
		// Bound slow request uploads as well as upstream work.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(c.timeout))
		defer http.NewResponseController(w).SetReadDeadline(time.Time{})
		body, err := readBounded(r.Body, c.maximum)
		if err != nil {
			if c.ConnectionFailure != nil {
				c.ConnectionFailure(id, "request_body_failed")
			}
			http.Error(w, "HTTPS seam body incomplete or exceeds limit", 413)
			return
		}
		expires, _ := ctx.Deadline()
		frame := Request{Session: c.session, Sequence: c.next.Add(1), Seam: id, Method: r.Method, Target: r.RequestURI, Headers: cleanHeaders(r.Header, c.stripped), Body: body, Expires: expires}
		result, err := c.send(ctx, frame)
		if err != nil {
			if c.ConnectionFailure != nil {
				c.ConnectionFailure(id, "bridge_or_upstream_failed")
			}
			http.Error(w, "HTTPS seam forwarding failed; inspect host connection diagnostics", 502)
			return
		}
		for name, values := range result.Headers {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
		w.WriteHeader(result.Status)
		_, _ = w.Write(result.Body)
	})
}
func (c *Client) send(ctx context.Context, frame Request) (Response, error) {
	encoded, _ := json.Marshal(frame)
	var data []byte
	var err error
	var status int
	if c.spool != nil {
		var response bridge.HTTPResponseFrame
		response, err = c.spool.Do(ctx, bridge.SeamChannel, "POST", "/v1/https-seam", http.Header{"Content-Type": []string{"application/json"}}, encoded)
		data, status = response.Body, response.Status
	} else {
		request, _ := http.NewRequestWithContext(ctx, "POST", "http://unix/v1/https-seam", bytes.NewReader(encoded))
		response, callErr := c.unix.Do(request)
		if callErr != nil {
			return Response{}, callErr
		}
		defer response.Body.Close()
		status = response.StatusCode
		data, err = readBounded(response.Body, FrameLimit(c.maximum))
	}
	if err != nil {
		return Response{}, err
	}
	if status != 200 {
		return Response{}, errors.New("HTTPS seam connector rejected request")
	}
	var result Response
	if decode(data, &result) != nil || result.Status < 200 || result.Status > 599 || int64(len(result.Body)) > c.maximum {
		return Response{}, errors.New("invalid HTTPS seam result")
	}
	return result, nil
}
