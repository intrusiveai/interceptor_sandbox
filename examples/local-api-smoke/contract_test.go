package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConsumerContractCheck(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		pass       bool
	}{
		{"match", `{"status":200,"body":{"interceptor_contract_version":"0.2.0"}}`, 200, true},
		{"different product release", `{"status":200,"body":{"version":"42.0.0","interceptor_contract_version":"0.2.0"}}`, 200, true},
		{"mismatch", `{"status":200,"body":{"interceptor_contract_version":"0.3.0"}}`, 200, false},
		{"missing", `{"status":200,"body":{}}`, 200, false},
		{"malformed", `{"status":200,"body":{"interceptor_contract_version":"v0.2.0"}}`, 200, false},
		{"wrong type", `{"status":200,"body":{"interceptor_contract_version":1}}`, 200, false},
		{"unreadable", `{`, 200, false},
		{"trailing", `{"status":200,"body":{"interceptor_contract_version":"0.2.0"}} {}`, 200, false},
		{"unavailable", `{}`, 404, false},
		{"failed", `{}`, 503, false},
		{"native error", `{"status":503,"body":{"interceptor_contract_version":"0.2.0"}}`, 200, false},
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
			err := checkContract(s.Client(), s.URL, supportedContractVersion)
			if (err == nil) != tc.pass || calls != 1 {
				t.Fatal(err, calls)
			}
			if tc.name == "mismatch" && (!strings.Contains(err.Error(), "expected 0.2.0") || !strings.Contains(err.Error(), "actual 0.3.0")) {
				t.Fatal(err)
			}
		})
	}
	s := httptest.NewServer(http.NotFoundHandler())
	s.Close()
	if checkContract(s.Client(), s.URL, supportedContractVersion) == nil {
		t.Fatal("accepted connection failure")
	}
}
