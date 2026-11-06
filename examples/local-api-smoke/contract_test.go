package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/integrationcontract"
	"github.com/intrusive-ai/interceptor-sandbox/internal/localapi"
)

func TestConsumerAgainstInterceptorContractAPI(t *testing.T) {
	// Use the real endpoint before campaign attachment, with no target or runtime.
	// The consumer's independent pin must agree with the embedded contract.
	s := httptest.NewServer(&localapi.Service{})
	defer s.Close()
	if err := checkContract(s.Client(), s.URL, supportedContractVersion, supportedContractSHA256); err != nil {
		t.Fatal(err)
	}
	if err := checkContract(s.Client(), s.URL, "0.3.0", supportedContractSHA256); err == nil {
		t.Fatal("accepted an unsupported contract version")
	}
}

func TestConsumerContractCheck(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		pass       bool
	}{
		{"match", `{"status":200,"body":{"interceptor_contract_version":"0.2.0","interceptor_contract_sha256":"8a073e2d88fcad4152eb53abfa512a20e8733d2baaacff3b77daec5e10280f30"}}`, 200, true},
		{"different product release", `{"status":200,"body":{"version":"42.0.0","interceptor_contract_version":"0.2.0","interceptor_contract_sha256":"8a073e2d88fcad4152eb53abfa512a20e8733d2baaacff3b77daec5e10280f30"}}`, 200, true},
		{"mismatch", `{"status":200,"body":{"interceptor_contract_version":"0.3.0","interceptor_contract_sha256":"8a073e2d88fcad4152eb53abfa512a20e8733d2baaacff3b77daec5e10280f30"}}`, 200, false},
		{"hash mismatch", `{"status":200,"body":{"interceptor_contract_version":"0.2.0","interceptor_contract_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`, 200, false},
		{"missing hash", `{"status":200,"body":{"interceptor_contract_version":"0.2.0"}}`, 200, false},
		{"malformed hash", `{"status":200,"body":{"interceptor_contract_version":"0.2.0","interceptor_contract_sha256":"sha256:abcd"}}`, 200, false},
		{"wrong hash type", `{"status":200,"body":{"interceptor_contract_version":"0.2.0","interceptor_contract_sha256":42}}`, 200, false},
		{"missing", `{"status":200,"body":{}}`, 200, false},
		{"malformed", `{"status":200,"body":{"interceptor_contract_version":"v0.2.0"}}`, 200, false},
		{"wrong type", `{"status":200,"body":{"interceptor_contract_version":1}}`, 200, false},
		{"unreadable", `{`, 200, false},
		{"trailing", `{"status":200,"body":{"interceptor_contract_version":"0.2.0","interceptor_contract_sha256":"8a073e2d88fcad4152eb53abfa512a20e8733d2baaacff3b77daec5e10280f30"}} {}`, 200, false},
		{"unavailable", `{}`, 404, false},
		{"failed", `{}`, 503, false},
		{"native error", `{"status":503,"body":{"interceptor_contract_version":"0.2.0","interceptor_contract_sha256":"8a073e2d88fcad4152eb53abfa512a20e8733d2baaacff3b77daec5e10280f30"}}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/contract-version" || r.Method != "POST" {
					t.Error("unexpected campaign operation", r.URL)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			err := checkContract(s.Client(), s.URL, supportedContractVersion, supportedContractSHA256)
			if (err == nil) != tc.pass || calls != 1 {
				t.Fatal(err, calls)
			}
			if tc.name == "hash mismatch" && (!strings.Contains(err.Error(), supportedContractSHA256) || !strings.Contains(err.Error(), strings.Repeat("a", 64))) {
				t.Fatal(err)
			}
			if tc.name == "mismatch" && (!strings.Contains(err.Error(), "expected 0.2.0") || !strings.Contains(err.Error(), "actual 0.3.0")) {
				t.Fatal(err)
			}
		})
	}
	s := httptest.NewServer(http.NotFoundHandler())
	s.Close()
	if checkContract(s.Client(), s.URL, supportedContractVersion, supportedContractSHA256) == nil {
		t.Fatal("accepted connection failure")
	}
}

// The API still reports the same version, but even a formatting-only edit must
// fail the independently pinned consumer before any attach can be attempted.
func TestConsumerRejectsChangedContractBytesBeforeAttach(t *testing.T) {
	raw := integrationcontract.JSON()
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	for name, altered := range map[string][]byte{
		"formatting": compact.Bytes(),
		"content":    bytes.Replace(raw, []byte("Discover the supported Operator API agreement"), []byte("Changed API agreement"), 1),
	} {
		t.Run(name, func(t *testing.T) {
			hash := fmt.Sprintf("%x", sha256.Sum256(altered))
			if hash == supportedContractSHA256 {
				t.Fatal("test did not change document bytes")
			}
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/contract-version" {
					t.Error("operation sent after incompatible identity", r.URL)
				}
				fmt.Fprintf(w, `{"status":200,"body":{"interceptor_contract_version":%q,"interceptor_contract_sha256":%q}}`, supportedContractVersion, hash)
			}))
			defer s.Close()
			if err := checkContract(s.Client(), s.URL, supportedContractVersion, supportedContractSHA256); err == nil {
				resp, err := s.Client().Post(s.URL+"/v1/attach", "application/json", strings.NewReader(`{}`))
				if err == nil {
					resp.Body.Close()
				}
				t.Fatal("accepted changed document bytes")
			}
			if calls != 1 {
				t.Fatal("unexpected calls", calls)
			}
		})
	}
}
