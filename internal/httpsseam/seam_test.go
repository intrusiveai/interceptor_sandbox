package httpsseam

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

func configuration(t *testing.T, upstream *httptest.Server, auth Auth, debug bool) (*Prepared, Config) {
	t.Helper()
	root := t.TempDir()
	ca := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	config := Config{Seams: []Binding{{ID: "tracing", Origin: upstream.URL, CAFile: ca, Auth: auth, Debug: Debug{Enabled: debug, MaxExchangeBytes: 4096, MaxTotalBytes: 32768}}}}
	p, err := prepare(context.Background(), config, root, []environment.HTTPSSeam{{ID: "tracing", Hostname: "trace.example"}}, func(context.Context, string) (string, error) { return "host-secret-123456789", nil })
	if err != nil {
		t.Fatal(err)
	}
	return p, config
}
func frame(body string) Request {
	return Request{Session: "session-test", Sequence: 1, Seam: "tracing", Method: "POST", Target: "/api/public/otel/v1/traces?sample=private-query", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(body), Expires: time.Now().Add(time.Second)}
}
func TestForwardingBothTransports(t *testing.T) {
	for _, mode := range []string{"spool", "unix"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer host-secret-123456789" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Forwarded-Host") != "" {
					t.Error("credentials or routing headers leaked")
				}
				if r.URL.RequestURI() != "/api/public/otel/v1/traces?sample=private-query" {
					t.Errorf("target changed: %s", r.URL.RequestURI())
				}
				if strings.Join(r.Header.Values("X-Repeated"), ",") != "a,b" {
					t.Error("repeated header lost")
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != "request-sentinel" {
					t.Errorf("body %q", body)
				}
				w.Header().Add("X-Result", "one")
				w.Header().Add("X-Result", "two")
				w.WriteHeader(422)
				_, _ = w.Write([]byte("response-sentinel"))
			}))
			defer upstream.Close()
			p, _ := configuration(t, upstream, Auth{Kind: "header", Header: "Authorization", Credential: "test", Prefix: "Bearer "}, true)
			logs := t.TempDir()
			host, err := NewConnector(p, "session-test", logs, 1<<20, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(t.TempDir(), "spool")
			if mode == "unix" {
				path = filepath.Join(t.TempDir(), "s.sock")
				server, listener, err := bridge.ListenUnix(path, host)
				if err != nil {
					t.Fatal(err)
				}
				defer server.Close()
				go server.Serve(listener)
			} else {
				done := make(chan error, 1)
				go func() {
					done <- bridge.ServeHTTPSpool(ctx, bridge.HTTPSpoolServerConfig{Root: path, Seam: host, MaxBytes: FrameLimit(1 << 20)})
				}()
				defer func() {
					cancel()
					if err := <-done; err != nil {
						t.Error(err)
					}
				}()
			}
			client, err := NewClient("session-test", mode, path, 1<<20, 2*time.Second, p.StrippedHeaders())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "https://trace.example/api/public/otel/v1/traces?sample=private-query", strings.NewReader("request-sentinel"))
			request.RequestURI = "/api/public/otel/v1/traces?sample=private-query"
			request.Header.Set("Authorization", "synthetic-placeholder")
			request.Header.Set("Cookie", "session=untrusted")
			request.Header.Set("X-Forwarded-Host", "other.example")
			request.Header["X-Repeated"] = []string{"a", "b"}
			result := httptest.NewRecorder()
			client.Handler("tracing").ServeHTTP(result, request)
			if result.Code != 422 || result.Body.String() != "response-sentinel" || len(result.Header().Values("X-Result")) != 2 {
				t.Fatalf("response %d %s", result.Code, result.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatal("unexpected retries")
			}
			raw, err := os.ReadFile(filepath.Join(logs, "https-seam-tracing-debug.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			var record struct {
				RequestBody  []byte `json:"request_body"`
				ResponseBody []byte `json:"response_body"`
			}
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			if string(record.RequestBody) != "request-sentinel" || string(record.ResponseBody) != "response-sentinel" {
				t.Fatalf("missing diagnostic bodies: %s", raw)
			}
			for _, forbidden := range []string{"host-secret-123456789", "synthetic-placeholder", "private-query"} {
				if bytes.Contains(raw, []byte(forbidden)) {
					t.Fatalf("diagnostics leaked %s", forbidden)
				}
			}
			raw, _ = os.ReadFile(filepath.Join(logs, "https-seam-connections.jsonl"))
			if bytes.Contains(raw, []byte("sentinel")) {
				t.Fatal("ordinary logs captured payload")
			}
			pauseCtx, stop := context.WithTimeout(ctx, time.Second)
			defer stop()
			if err := client.Pause(pauseCtx); err != nil {
				t.Fatal(err)
			}
			rejected := httptest.NewRecorder()
			client.Handler("tracing").ServeHTTP(rejected, request)
			if rejected.Code != 503 {
				t.Fatal("paused seam admitted request")
			}
			client.Resume()
		})
	}
}
func TestIsolationAndFailures(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/redirect":
			w.Header().Set("Location", "https://undeclared.example/steal")
			w.WriteHeader(302)
		case "/echo":
			_, _ = w.Write([]byte(r.Header.Get("Authorization")))
		case "/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: test\n\n"))
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer upstream.Close()
	p, _ := configuration(t, upstream, Auth{Kind: "header", Header: "Authorization", Credential: "test", Prefix: "Bearer "}, false)
	logs := t.TempDir()
	host, err := NewConnector(p, "session-test", logs, 1024, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	f := frame("payload")
	f.Target = "/redirect"
	response, _, err := host.forward(context.Background(), f)
	if err != nil || response.Status != 302 || calls.Load() != 1 {
		t.Fatalf("redirect: %+v %v", response, err)
	}
	if _, _, err := host.forward(context.Background(), f); err == nil || calls.Load() != 1 {
		t.Fatal("replayed request dispatched")
	}
	for i, change := range []func(*Request){func(f *Request) { f.Target = "https://other.example/" }, func(f *Request) { f.Target = "//other.example/" }, func(f *Request) { f.Method = "CONNECT" }, func(f *Request) { f.Session = "wrong" }, func(f *Request) { f.Seam = "unknown" }, func(f *Request) { f.Expires = time.Now().Add(-time.Second) }, func(f *Request) { f.Headers["upgrade"] = []string{"websocket"} }, func(f *Request) { f.Headers.Set("Content-Type", "application/grpc") }, func(f *Request) { f.Body = make([]byte, 1025) }} {
		f = frame("payload")
		f.Sequence = uint64(i + 2)
		change(&f)
		if _, _, err := host.forward(context.Background(), f); err == nil {
			t.Errorf("invalid frame %d accepted", i)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("invalid input reached upstream")
	}
	f = frame("")
	f.Sequence = 20
	f.Target = "/echo"
	if _, _, err := host.forward(context.Background(), f); err == nil {
		t.Fatal("credential echo reached guest")
	}
	f.Sequence++
	f.Target = "/stream"
	if _, _, err := host.forward(context.Background(), f); err == nil {
		t.Fatal("stream accepted")
	}
	files, _ := os.ReadDir(logs)
	if len(files) != 1 {
		t.Fatal("debug file created with debug off")
	}
	raw, _ := os.ReadFile(filepath.Join(logs, "https-seam-connections.jsonl"))
	if strings.Contains(string(raw), "payload") || strings.Contains(string(raw), "host-secret") {
		t.Fatal("ordinary logs leaked content")
	}
}
func TestBinaryGzipAndDebugBounds(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, _ = gz.Write([]byte("request-sentinel host-secret-123456789"))
	_ = gz.Close()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, compressed.Bytes()) {
			t.Error("compressed payload transformed")
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write([]byte{0, 1, 2, 255})
	}))
	defer upstream.Close()
	p, _ := configuration(t, upstream, Auth{Kind: "basic", UsernameCredential: "user", PasswordCredential: "pass"}, true)
	p.endpoints["tracing"].binding.Debug.MaxExchangeBytes = 20
	logs := t.TempDir()
	host, err := NewConnector(p, "session-test", logs, 1<<20, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	f := frame("")
	f.Headers.Set("Content-Encoding", "gzip")
	f.Body = compressed.Bytes()
	response, _, err := host.forward(context.Background(), f)
	if err != nil || !bytes.Equal(response.Body, []byte{0, 1, 2, 255}) {
		t.Fatalf("binary response changed %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(logs, "https-seam-tracing-debug.jsonl"))
	var entry map[string]any
	_ = json.Unmarshal(raw, &entry)
	if entry["truncated"] != true {
		t.Fatalf("truncation missing %s", raw)
	}
	var payload struct {
		Request []byte `json:"request_body"`
	}
	_ = json.Unmarshal(raw, &payload)
	if bytes.Contains(payload.Request, []byte("host-secret")) {
		t.Fatal("secret prefix survived truncation")
	}
	p.endpoints["tracing"].binding.Debug.MaxTotalBytes = int64(len(raw))
	f.Sequence++
	_, _, _ = host.forward(context.Background(), f)
	after, _ := os.ReadFile(filepath.Join(logs, "https-seam-tracing-debug.jsonl"))
	if len(after) != len(raw) {
		t.Fatal("debug total limit not enforced")
	}
}
func TestCancellationAndTrust(t *testing.T) {
	canceled := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	p, _ := configuration(t, upstream, Auth{Kind: "none"}, false)
	host, err := NewConnector(p, "session-test", t.TempDir(), 1024, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	f := frame("body")
	f.Expires = time.Now().Add(60 * time.Millisecond)
	if _, _, err := host.forward(context.Background(), f); err == nil {
		t.Fatal("deadline ignored")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream not canceled")
	}
	p.endpoints["tracing"].tls = &tls.Config{MinVersion: tls.VersionTLS12}
	untrusted, err := NewConnector(p, "session-test", t.TempDir(), 1024, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer untrusted.Close()
	f = frame("")
	if _, _, err := untrusted.forward(context.Background(), f); err == nil || !strings.Contains(err.Error(), "tls_failed") {
		t.Fatalf("untrusted CA not rejected: %v", err)
	}
}
func TestBindingValidation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	_, original := configuration(t, server, Auth{Kind: "none"}, false)
	declarations := []environment.HTTPSSeam{{ID: "tracing"}}
	for _, origin := range []string{"http://example.com", "https://user:secret@example.com", "https://example.com/path", "https://example.com?query=x", "https://example.com#fragment", "https://example.com?"} {
		config := original
		config.Seams = append([]Binding(nil), original.Seams...)
		config.Seams[0].Origin = origin
		if _, err := prepare(context.Background(), config, "", declarations, nil); err == nil {
			t.Errorf("invalid origin accepted %s", origin)
		}
	}
	for _, name := range []string{"Host", "Cookie", "Connection", "Content-Length", "X-Forwarded-Host", "bad\r\nname"} {
		if allowedAuthHeader(name) {
			t.Errorf("unsafe auth header %s", name)
		}
	}
	if _, err := prepare(context.Background(), Config{}, "", declarations, nil); err == nil {
		t.Fatal("missing binding accepted")
	}
	if _, err := prepare(context.Background(), original, "", nil, nil); err == nil {
		t.Fatal("undeclared binding accepted")
	}
	p, err := prepare(context.Background(), original, "", declarations, nil)
	if err != nil {
		t.Fatal(err)
	}
	original.Seams[0].Debug.Enabled = true
	same, err := prepare(context.Background(), original, "", declarations, nil)
	if err != nil || same.Digest() != p.Digest() {
		t.Fatal("debug changes authority binding")
	}
	original.Seams[0].Origin = "https://different.example"
	changed, err := prepare(context.Background(), original, "", declarations, nil)
	if err != nil || changed.Digest() == p.Digest() {
		t.Fatal("origin change not bound")
	}
}

func TestDiagnosticFailureDoesNotChangeExchange(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(401)
		_, _ = w.Write([]byte("configuration rejected"))
	}))
	defer upstream.Close()
	p, _ := configuration(t, upstream, Auth{Kind: "none"}, true)
	host, err := NewConnector(p, "session-test", t.TempDir(), 1024, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	_ = host.diagnostics.captures["tracing"].Close()
	result, _, err := host.forward(context.Background(), frame("request"))
	if err != nil || result.Status != 401 || string(result.Body) != "configuration rejected" || calls.Load() != 1 {
		t.Fatalf("diagnostic failure changed upstream exchange: %+v %v", result, err)
	}
}
func TestSnapshotWaitsForAdmittedSeamRequest(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.WriteHeader(204) }))
	defer upstream.Close()
	p, _ := configuration(t, upstream, Auth{Kind: "none"}, false)
	host, err := NewConnector(p, "session-test", t.TempDir(), 1024, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	directory, err := os.MkdirTemp("", "seam-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "seam.sock")
	server, listener, err := bridge.ListenUnix(path, host)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	go server.Serve(listener)
	client, err := NewClient("session-test", "unix", path, 1024, time.Second, p.StrippedHeaders())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		r := httptest.NewRequest("POST", "https://trace.example/", nil)
		r.RequestURI = "/"
		w := httptest.NewRecorder()
		client.Handler("tracing").ServeHTTP(w, r)
		done <- w.Code
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := client.Pause(ctx); err == nil {
		t.Fatal("snapshot admitted before in-flight request drained")
	}
	close(release)
	if status := <-done; status != 204 {
		t.Fatalf("drain altered exchange: %d", status)
	}
	if err := client.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.Resume()
}

func TestConnectorBoundsConcurrentExchanges(t *testing.T) {
	entered := make(chan struct{}, bridge.DefaultHTTPConcurrency)
	release := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-release; w.WriteHeader(204) }))
	defer upstream.Close()
	p, _ := configuration(t, upstream, Auth{Kind: "none"}, false)
	host, err := NewConnector(p, "session-test", t.TempDir(), 1024, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	done := make(chan int, bridge.DefaultHTTPConcurrency)
	for n := 1; n <= bridge.DefaultHTTPConcurrency; n++ {
		go func(n int) {
			f := frame("")
			f.Sequence = uint64(n)
			f.Expires = time.Now().Add(4 * time.Second)
			raw, _ := json.Marshal(f)
			w := httptest.NewRecorder()
			host.ServeHTTP(w, httptest.NewRequest("POST", "http://bridge/v1/https-seam", bytes.NewReader(raw)))
			done <- w.Code
		}(n)
	}
	for n := 0; n < bridge.DefaultHTTPConcurrency; n++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("requests did not reach upstream")
		}
	}
	w := httptest.NewRecorder()
	host.ServeHTTP(w, httptest.NewRequest("POST", "http://bridge/v1/https-seam", nil))
	close(release)
	if w.Code != 503 {
		t.Fatalf("overload accepted: %d", w.Code)
	}
	for n := 0; n < bridge.DefaultHTTPConcurrency; n++ {
		if code := <-done; code != 200 {
			t.Errorf("admitted request failed: %d", code)
		}
	}
}
