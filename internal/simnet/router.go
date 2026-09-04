package simnet

import (
	"net"
	"net/http"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

const TLSProbePath = "/.well-known/interceptor/tls-probe"

// HostRouter is the shared HTTP/TLS front door for declared simulated origins.
// It routes only exact Blueprint hostnames and rejects ambiguous TLS Host/SNI
// combinations before a service handler observes the request.
type HostRouter struct {
	routes   map[string]http.Handler
	recorder *recording.Writer
}

func NewHostRouter(routes map[string]http.Handler, recorder *recording.Writer) *HostRouter {
	copy := make(map[string]http.Handler, len(routes))
	for hostname, handler := range routes {
		copy[normalizeHostname(hostname)] = handler
	}
	return &HostRouter{routes: copy, recorder: recorder}
}

func (r *HostRouter) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	host := normalizeHostname(request.Host)
	if request.TLS != nil {
		if request.TLS.ServerName == "" {
			r.gap(host, request, "TLS SNI is required")
			http.Error(writer, "misdirected simulated-service request", http.StatusMisdirectedRequest)
			return
		}
		if normalizeHostname(request.TLS.ServerName) != host {
			r.gap(host, request, "TLS SNI and HTTP Host differ")
			http.Error(writer, "misdirected simulated-service request", http.StatusMisdirectedRequest)
			return
		}
	}
	handler := r.routes[host]
	if handler == nil {
		r.gap(host, request, "hostname is not declared")
		http.Error(writer, "unknown simulated-service hostname", http.StatusMisdirectedRequest)
		return
	}
	if request.TLS != nil && request.Method == http.MethodGet && request.URL.Path == TLSProbePath {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	handler.ServeHTTP(writer, request)
}

func (r *HostRouter) gap(host string, request *http.Request, reason string) {
	if r.recorder == nil {
		return
	}
	_, _ = r.recorder.Append(recording.Draft{Channel: recording.Evaluation, Layer: "network_frontdoor", Kind: "coverage_gap", Provenance: recording.EnvironmentSealed, Semantic: map[string]any{"hostname": host, "method": request.Method, "path": request.URL.Path, "reason": reason}})
}

func normalizeHostname(value string) string {
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
}
