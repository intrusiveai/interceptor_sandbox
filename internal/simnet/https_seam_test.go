package simnet

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

func TestSeamDNSAndFrontDoorDoNotRecordCampaignEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writer, err := recording.Open(path, "session-test")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	dns := &DNSServer{records: map[string]struct{}{"trace.example": {}}, unobserved: map[string]bool{"trace.example": true}, policy: "nxdomain", limit: 10, recorder: writer}
	if _, err := dns.answer(dnsQuery("trace.example", 1)); err != nil {
		t.Fatal(err)
	}
	router := NewHostRouter(map[string]http.Handler{"trace.example": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })}, writer, "trace.example")
	rejected := 0
	router.Rejected = func(host, reason string) {
		rejected++
		if host != "trace.example" {
			t.Error("untrusted hostname logged")
		}
	}
	for _, sni := range []string{"trace.example", "", "other.example"} {
		request := httptest.NewRequest("POST", "https://trace.example/private-sentinel", nil)
		request.TLS.ServerName = sni
		router.ServeHTTP(httptest.NewRecorder(), request)
	}
	if rejected != 2 {
		t.Fatalf("expected local rejections, got %d", rejected)
	}
	_ = writer.Flush()
	events, err := recording.VerifyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatal("seam traffic created campaign evidence")
	}
}
