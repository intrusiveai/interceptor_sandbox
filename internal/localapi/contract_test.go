package localapi

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/integrationcontract"
)

type noContractRuntime struct{ Runtime }

func (noContractRuntime) CheckHost() error { panic("contract discovery called runtime") }

func TestContractBeforeAttachmentWithoutRuntimeOrState(t *testing.T) {
	// No ledger, target or campaign is needed; an attempted runtime call panics.
	s := &Service{runtime: noContractRuntime{}}
	for _, method := range []string{"POST", "GET"} {
		for _, input := range []string{`{}`, `null`, `[]`, ``, `{"campaign_id":"x"}`, `{} {}`, `{"x":1,"x":2}`, strings.Repeat(" ", 4097) + `{}`} {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(method, "/v1/contract-version", strings.NewReader(input)))
			if method != "POST" {
				if w.Code != 405 || w.Header().Get("Allow") != "POST" {
					t.Fatal(w)
				}
				continue
			}
			if input != `{}` {
				if w.Code != 400 && w.Code != 413 {
					t.Fatalf("accepted %q: %d", input, w.Code)
				}
				continue
			}
			var result adaptive.GatewayResponse
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			var body map[string]string
			if err := json.Unmarshal(result.Body, &body); err != nil {
				t.Fatal(err)
			}
			wantHash := fmt.Sprintf("%x", sha256.Sum256(integrationcontract.JSON()))
			if w.Code != 200 || result.Status != 200 || len(body) != 2 || body["interceptor_contract_version"] != integrationcontract.Version() || body["interceptor_contract_sha256"] != wantHash || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(w)
			}
		}
	}
	if s.state.Attached || s.state.CampaignID != "" || s.failed {
		t.Fatal("discovery mutated state")
	}
}
