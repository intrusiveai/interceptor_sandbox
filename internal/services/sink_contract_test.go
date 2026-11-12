package services

import (
	"github.com/intrusive-ai/interceptor-sandbox/internal/delivery"
	"testing"
)

func TestSinkDiscoveryAllowlists(t *testing.T) {
	for _, options := range []map[string]any{nil, {"methods": []string{"POST"}}, {"paths": []string{"/collect"}}, {"methods": []any{"get", "POST"}, "paths": []any{"/collect", "/instructions"}}} {
		methods, paths, err := SinkAllowlists(options)
		if err != nil {
			t.Fatal(err)
		}
		ep := SinkEndpoint(methods, paths, SinkResponse{Body: "hidden"}, 0)
		if ep.Kind != "http-route-set" || ep.Method != "" || ep.Path != "" {
			t.Fatal("invented concrete endpoint")
		}
		if err := delivery.Validate(ep.Delivery); err != nil {
			t.Fatal(err)
		}
	}
	for _, options := range []map[string]any{{"methods": "POST"}, {"methods": []any{7}}, {"paths": []any{false}}, {"paths": []string{"relative"}}, {"methods": []string{"GET POST"}}, {"methods": []string{"GET\x00"}}, {"methods": []string{"GÉT"}}, {"paths": []string{"/a?x=y"}}} {
		if _, _, err := SinkAllowlists(options); err == nil {
			t.Fatal("invalid allowlist widened scope", options)
		}
	}
}

func TestHeadOnlySinkDoesNotAdvertiseResponseInjection(t *testing.T) {
	if ep := SinkEndpoint([]string{"HEAD"}, nil, SinkResponse{}, 1024); ep != nil {
		t.Fatal("HEAD-only sink advertised body injection")
	}
	ep := SinkEndpoint([]string{"GET", "HEAD"}, nil, SinkResponse{}, 1024)
	if len(ep.Methods) != 1 || ep.Methods[0] != "GET" {
		t.Fatal(ep)
	}
}
