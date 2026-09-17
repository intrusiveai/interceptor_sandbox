package sandboxd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/delivery"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
)

func deliveryServer() *Server {
	c := &delivery.Contract{Description: "Task", Input: delivery.Body{MediaType: "application/json", Encoding: "json", MaxBytes: 1024, Basis: "source-derived",
		Schema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []any{"query"}}},
		Output: delivery.Body{MediaType: "application/json", Encoding: "json", MaxBytes: 1024, Basis: "unknown"}}
	return &Server{blueprint: &environment.Blueprint{Limits: environment.Limits{MaxBlobBytes: 1024}, Application: environment.Application{Interface: environment.ApplicationInterface{Operations: []environment.ApplicationOperation{{ID: "invoke", Delivery: c}}}}}}
}

func TestInvalidDeliveryInputDoesNotInvokeApplication(t *testing.T) {
	s := deliveryServer()
	called := false
	h := s.adapterHandler(func(context.Context, applicationInvocation) (applicationResult, error) {
		called = true
		return applicationResult{}, nil
	})
	for _, body := range []string{"prose", `{"query":42}`, `{"query":"x","query":"y"}`} {
		request := httptest.NewRequest("POST", "/operations/invoke", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, request)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_delivery_input") || called {
			t.Fatalf("invalid input reached application: %d %s", w.Code, w.Body)
		}
	}
	if err := s.validateDeliveryInput("invoke", "text/plain", []byte(`{"query":"ok"}`)); err == nil {
		t.Fatal("wrong media type accepted")
	}
	if err := s.validateDeliveryInput("invoke", "application/json; charset=utf-8", []byte(`{"query":"ok"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestOutputContractStatusIsAdvisory(t *testing.T) {
	s := deliveryServer()
	if got := s.outputContractStatus("invoke", "application/json", []byte(`{"unpredicted":"value"}`)); got != "unknown" {
		t.Fatal(got)
	}
	c := s.blueprint.Application.Interface.Operations[0].Delivery
	c.Output.Basis = "observed"
	c.Output.Schema = map[string]any{"type": "object"}
	body := []byte(`"an unexpected response"`)
	original := string(body)
	if got := s.outputContractStatus("invoke", "application/json", body); got != "mismatch" {
		t.Fatal(got)
	}
	if string(body) != original {
		t.Fatal("output was rewritten")
	}
	if got := s.outputContractStatus("invoke", "application/json", []byte(`{}`)); got != "matched" {
		t.Fatal(got)
	}
}
