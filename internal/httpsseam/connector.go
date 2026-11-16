package httpsseam

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"golang.org/x/net/http/httpguts"
)

type Request struct {
	Session  string      `json:"session"`
	Sequence uint64      `json:"sequence"`
	Seam     string      `json:"seam"`
	Method   string      `json:"method"`
	Target   string      `json:"target"`
	Headers  http.Header `json:"headers"`
	Body     []byte      `json:"body"`
	Expires  time.Time   `json:"expires"`
}
type Response struct {
	Status  int         `json:"status"`
	Headers http.Header `json:"headers"`
	Body    []byte      `json:"body"`
}
type Connector struct {
	slots       chan struct{}
	prepared    *Prepared
	session     string
	maximum     int64
	timeout     time.Duration
	clients     map[string]*http.Client
	diagnostics *diagnostics
	mu          sync.Mutex
	high        uint64
	seen        map[uint64]bool
}

func NewConnector(p *Prepared, session, logs string, maximum int64, timeout time.Duration) (*Connector, error) {
	if p == nil || session == "" || maximum <= 0 || timeout <= 0 {
		return nil, errors.New("invalid HTTPS seam connector configuration")
	}
	d, err := newDiagnostics(logs, session, p)
	if err != nil {
		return nil, err
	}
	c := &Connector{slots: make(chan struct{}, bridge.DefaultHTTPConcurrency), prepared: p, session: session, maximum: maximum, timeout: timeout, diagnostics: d, clients: map[string]*http.Client{}, seen: map[uint64]bool{}}
	for id, e := range p.endpoints {
		// No pooled-connection retry: even an idempotent-looking request can have
		// external effects. A new connection also makes connection logs unambiguous.
		transport := &http.Transport{Proxy: nil, TLSClientConfig: e.tls.Clone(), DisableCompression: true, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: timeout}).DialContext, TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout, MaxResponseHeaderBytes: 1 << 20}
		c.clients[id] = &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return c, nil
}
func (c *Connector) Close() error {
	for _, client := range c.clients {
		client.CloseIdleConnections()
	}
	return c.diagnostics.Close()
}
func FrameLimit(maximum int64) int64 { return 4*((maximum+2)/3) + (1 << 20) }
func readBounded(r io.Reader, maximum int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maximum {
		return nil, errors.New("HTTPS seam message exceeds limit")
	}
	return b, nil
}
func decode(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing frame data")
	}
	return nil
}
func (c *Connector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		http.Error(w, "HTTPS seam is busy", 503)
		return
	}
	if r.Method != "POST" || r.URL.RequestURI() != "/v1/https-seam" {
		http.Error(w, "invalid seam bridge route", 400)
		return
	}
	data, err := readBounded(r.Body, FrameLimit(c.maximum))
	var frame Request
	if err != nil || decode(data, &frame) != nil {
		http.Error(w, "invalid seam frame", 400)
		return
	}
	result, status, err := c.forward(r.Context(), frame)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
func (c *Connector) accept(sequence uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	const window = 4096 // Bounded replay protection; late requests fail rather than resend.
	if sequence == 0 || c.seen[sequence] || (c.high >= window && sequence <= c.high-window) {
		return false
	}
	if sequence > c.high {
		c.high = sequence
		for n := range c.seen {
			if c.high >= window && n <= c.high-window {
				delete(c.seen, n)
			}
		}
	}
	c.seen[sequence] = true
	return true
}
func targetURL(target string) (*url.URL, error) {
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || strings.ContainsAny(target, "\x00\r\n\\") {
		return nil, errors.New("invalid request target")
	}
	u, err := url.ParseRequestURI(target)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid request target")
	}
	return u, nil
}
func validExchange(method, target string, headers http.Header) bool {
	if !httpguts.ValidHeaderFieldName(method) || method == "CONNECT" {
		return false
	}
	if _, err := targetURL(target); err != nil {
		return false
	}
	for name, values := range headers {
		if !httpguts.ValidHeaderFieldName(name) {
			return false
		}
		for _, v := range values {
			if !httpguts.ValidHeaderFieldValue(v) {
				return false
			}
		}
	}
	canonical := http.Header{}
	for name, values := range headers {
		for _, value := range values {
			canonical.Add(name, value)
		}
	}
	headers = canonical
	return headers.Get("Upgrade") == "" && !strings.Contains(strings.ToLower(headers.Get("Content-Type")), "application/grpc") && !strings.Contains(strings.ToLower(headers.Get("Accept")), "text/event-stream")
}
func cleanHeaders(headers http.Header, stripped []string) http.Header {
	out := http.Header{}
	for name, values := range headers {
		for _, v := range values {
			out.Add(name, v)
		}
	}
	for _, line := range out.Values("Connection") {
		for _, name := range strings.Split(line, ",") {
			out.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Host", "Connection", "Proxy-Connection", "Keep-Alive", "Transfer-Encoding", "Trailer", "TE", "Upgrade", "Content-Length", "Proxy-Authenticate", "Proxy-Authorization", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		out.Del(name)
	}
	for _, name := range stripped {
		out.Del(name)
	}
	return out
}
func (c *Connector) forward(parent context.Context, f Request) (Response, int, error) {
	fail := func(code int, category string) (Response, int, error) { return Response{}, code, errors.New(category) }
	e := c.prepared.endpoints[f.Seam]
	if e == nil || f.Session != c.session || !validExchange(f.Method, f.Target, f.Headers) || int64(len(f.Body)) > c.maximum {
		return fail(400, "invalid HTTPS seam request")
	}
	now := time.Now()
	if !f.Expires.After(now) || f.Expires.After(now.Add(c.timeout)) || !c.accept(f.Sequence) {
		return fail(409, "expired or duplicate HTTPS seam request")
	}
	ctx, cancel := context.WithDeadline(parent, f.Expires)
	defer cancel()
	u, _ := targetURL(f.Target)
	u.Scheme = e.origin.Scheme
	u.Host = e.origin.Host
	request, err := http.NewRequestWithContext(ctx, f.Method, u.String(), bytes.NewReader(f.Body))
	if err != nil {
		return fail(400, "invalid HTTPS seam request")
	}
	request.Header = cleanHeaders(f.Headers, c.prepared.StrippedHeaders())
	for name, values := range e.headers {
		request.Header[name] = append([]string(nil), values...)
	}
	started := time.Now()
	category := "connection_failed"
	address := ""
	var result Response
	defer func() { c.diagnostics.exchange(e, f, result, category, address, time.Since(started), c.maximum) }()
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		address = info.Conn.RemoteAddr().String()
		c.diagnostics.connection(f.Seam, "established", "")
	}}))
	response, err := c.clients[f.Seam].Do(request)
	if err != nil {
		category = connectionError(err)
		return fail(502, "HTTPS seam upstream "+category)
	}
	defer response.Body.Close()
	if response.StatusCode == 101 || strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") || strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "application/grpc") {
		category = "unsupported_response"
		return fail(502, "HTTPS seam streaming responses are unsupported")
	}
	body, err := readBounded(response.Body, c.maximum)
	if err != nil {
		category = "response_read_failed"
		return fail(502, "HTTPS seam response incomplete or exceeds limit")
	}
	headers := cleanHeaders(response.Header, []string{"Set-Cookie", "Set-Cookie2", "Authorization"})
	// A credential-echoing upstream must never send host secrets into the guest.
	if containsSecret(body, headers, e.secrets, c.maximum) {
		category = "credential_in_response"
		return fail(502, "HTTPS seam response contains protected credentials or cannot be checked safely")
	}
	result = Response{Status: response.StatusCode, Headers: headers, Body: body}
	category = "closed"
	return result, 200, nil
}
func connectionError(err error) string {
	var dns *net.DNSError
	var timeout net.Error
	if errors.As(err, &dns) {
		return "dns_failed"
	}
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	// Do not expose URL, query, TLS peer strings or raw upstream error text.
	if strings.Contains(err.Error(), "certificate") || strings.Contains(err.Error(), "tls:") {
		return "tls_failed"
	}
	return "connection_failed"
}
