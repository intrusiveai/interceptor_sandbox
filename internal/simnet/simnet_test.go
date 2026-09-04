package simnet

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDNSAnswersOnlyDeclaredARecords(t *testing.T) {
	server := &DNSServer{records: map[string]struct{}{"api.sandbox.local": {}}, policy: "nxdomain", limit: 3}
	declared, err := server.answer(dnsQuery("api.sandbox.local", 1))
	if err != nil {
		t.Fatal(err)
	}
	if answers := binary.BigEndian.Uint16(declared[6:8]); answers != 1 || declared[3]&0x0f != 0 {
		t.Fatalf("declared DNS response was not an A answer: %x", declared)
	}
	unknown, err := server.answer(dnsQuery("unknown.example", 1))
	if err != nil {
		t.Fatal(err)
	}
	if unknown[3]&0x0f != 3 || binary.BigEndian.Uint16(unknown[6:8]) != 0 {
		t.Fatalf("unknown DNS response did not fail NXDOMAIN: %x", unknown)
	}
	aaaa, err := server.answer(dnsQuery("api.sandbox.local", 28))
	if err != nil {
		t.Fatal(err)
	}
	if aaaa[3]&0x0f != 0 || binary.BigEndian.Uint16(aaaa[6:8]) != 0 {
		t.Fatalf("declared unsupported type was not NODATA: %x", aaaa)
	}
	limited, err := server.answer(dnsQuery("api.sandbox.local", 1))
	if err != nil {
		t.Fatal(err)
	}
	if limited[3]&0x0f != 5 || binary.BigEndian.Uint16(limited[6:8]) != 0 {
		t.Fatalf("DNS query limit did not fail closed: %x", limited)
	}
}

func TestHostRouterRequiresExactHostAndMatchingSNI(t *testing.T) {
	router := NewHostRouter(map[string]http.Handler{"api.sandbox.local": http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) })}, nil)
	request := httptest.NewRequest(http.MethodGet, "https://api.sandbox.local/test", nil)
	request.TLS.ServerName = "api.sandbox.local"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("declared host returned %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "https://api.sandbox.local"+TLSProbePath, nil)
	request.TLS.ServerName = "api.sandbox.local"
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("TLS probe returned %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "https://api.sandbox.local/test", nil)
	request.TLS.ServerName = "other.sandbox.local"
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("mismatched SNI returned %d", response.Code)
	}
}

func dnsQuery(name string, qtype uint16) []byte {
	message := make([]byte, 12)
	binary.BigEndian.PutUint16(message[0:2], 7)
	binary.BigEndian.PutUint16(message[2:4], 0x0100)
	binary.BigEndian.PutUint16(message[4:6], 1)
	start := 0
	for index := 0; index <= len(name); index++ {
		if index != len(name) && name[index] != '.' {
			continue
		}
		message = append(message, byte(index-start))
		message = append(message, name[start:index]...)
		start = index + 1
	}
	message = append(message, 0, byte(qtype>>8), byte(qtype), 0, 1)
	return message
}
