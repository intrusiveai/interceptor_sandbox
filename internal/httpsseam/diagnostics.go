package httpsseam

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type diagnostics struct {
	mu            sync.Mutex
	session       string
	connectionLog *os.File
	captures      map[string]*os.File
	used          map[string]int64
	exhausted     map[string]bool
}

func newDiagnostics(directory, session string, p *Prepared) (*diagnostics, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, err
	}
	open := func(name string) (*os.File, error) {
		// New per-session files, not arbitrary paths or existing symlinks.
		return os.OpenFile(filepath.Join(directory, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	}
	f, err := open("https-seam-connections.jsonl")
	if err != nil {
		return nil, err
	}
	d := &diagnostics{session: session, connectionLog: f, captures: map[string]*os.File{}, used: map[string]int64{}, exhausted: map[string]bool{}}
	for id, e := range p.endpoints {
		if e.binding.Debug.Enabled {
			f, err := open("https-seam-" + id + "-debug.jsonl")
			if err != nil {
				d.Close()
				return nil, err
			}
			d.captures[id] = f
		}
	}
	return d, nil
}
func (d *diagnostics) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := d.connectionLog.Close()
	for _, f := range d.captures {
		err = errors.Join(err, f.Close())
	}
	return err
}
func (d *diagnostics) connection(id, event, category string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.connectionLocked(id, event, category)
}
func (d *diagnostics) connectionLocked(id, event, category string) {
	_ = json.NewEncoder(d.connectionLog).Encode(map[string]any{"time": time.Now().UTC(), "session": d.session, "seam": id, "event": event, "category": category})
}
func variants(secret string) []string {
	encoded, _ := json.Marshal(secret)
	return []string{secret, url.QueryEscape(secret), url.PathEscape(secret), base64.StdEncoding.EncodeToString([]byte(secret)), string(encoded[1 : len(encoded)-1])}
}
func decodedBody(body []byte, encoding string, maximum int64) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body, nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		return readBounded(reader, maximum)
	default:
		return nil, errors.New("unsupported diagnostic content encoding")
	}
}
func containsSecret(body []byte, headers http.Header, secrets []string, maximum int64) bool {
	if len(secrets) == 0 {
		return false
	}
	decoded, err := decodedBody(body, headers.Get("Content-Encoding"), maximum)
	if err != nil {
		return true
	}
	for _, secret := range secrets {
		for _, value := range variants(secret) {
			if bytes.Contains(body, []byte(value)) || bytes.Contains(decoded, []byte(value)) {
				return true
			}
			for _, values := range headers {
				for _, header := range values {
					if strings.Contains(header, value) {
						return true
					}
				}
			}
		}
	}
	return false
}
func redact(data []byte, secrets []string) []byte {
	result := append([]byte(nil), data...)
	for _, secret := range secrets {
		for _, value := range variants(secret) {
			if value != "" {
				result = bytes.ReplaceAll(result, []byte(value), []byte("[REDACTED]"))
			}
		}
	}
	return result
}
func (d *diagnostics) exchange(e *endpoint, request Request, response Response, category, address string, elapsed time.Duration, maximum int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	event := "failed"
	if category == "closed" {
		event = "closed"
	}
	d.connectionLocked(request.Seam, event, category)
	file := d.captures[request.Seam]
	if file == nil || d.exhausted[request.Seam] {
		return
	}
	req, err1 := decodedBody(request.Body, request.Headers.Get("Content-Encoding"), maximum)
	res, err2 := decodedBody(response.Body, response.Headers.Get("Content-Encoding"), maximum)
	if err1 != nil || err2 != nil {
		d.connectionLocked(request.Seam, "debug_unavailable", "unsupported_or_oversized_encoding")
		return
	}
	// Redact before truncation so a boundary cannot retain a partial secret.
	secrets := append([]string(nil), e.secrets...)
	if u, err := targetURL(request.Target); err == nil {
		for _, values := range u.Query() {
			secrets = append(secrets, values...)
		}
	}
	req = redact(req, secrets)
	res = redact(res, secrets)
	budget := e.binding.Debug.MaxExchangeBytes
	truncated := false
	if int64(len(req)) > budget {
		req = req[:budget]
		truncated = true
	}
	budget -= int64(len(req))
	if int64(len(res)) > budget {
		res = res[:budget]
		truncated = true
	}
	path := strings.SplitN(request.Target, "?", 2)[0]
	entry := map[string]any{"time": time.Now().UTC(), "session": d.session, "seam": request.Seam, "sequence": request.Sequence, "method": request.Method, "path": string(redact([]byte(path), secrets)), "address": address, "elapsed_ms": elapsed.Milliseconds(), "outcome": category, "status": response.Status, "request_content_type": request.Headers.Get("Content-Type"), "response_content_type": response.Headers.Get("Content-Type"), "request_content_encoding": request.Headers.Get("Content-Encoding"), "response_content_encoding": response.Headers.Get("Content-Encoding"), "body_encoding": "base64 of decoded content", "request_body": req, "response_body": res, "truncated": truncated}
	encoded, _ := json.Marshal(entry)
	// Also cover diagnostic metadata, without redacting base64 body fragments.
	if containsSecret(nil, http.Header{"Diagnostic": []string{string(encoded)}}, e.secrets, maximum) {
		d.connectionLocked(request.Seam, "debug_unavailable", "unsafe_metadata")
		return
	}
	encoded = append(encoded, '\n')
	if int64(len(encoded)) > e.binding.Debug.MaxTotalBytes-d.used[request.Seam] {
		d.exhausted[request.Seam] = true
		d.connectionLocked(request.Seam, "debug_truncated", "storage_limit")
		return
	}
	n, err := file.Write(encoded)
	d.used[request.Seam] += int64(n)
	if err != nil {
		d.exhausted[request.Seam] = true
		d.connectionLocked(request.Seam, "debug_unavailable", "storage_failure")
	}
}
