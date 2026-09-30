package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEvidenceDownloadVerifiesBeforeCommit(t *testing.T) {
	var source bytes.Buffer
	archive := tar.NewWriter(&source)
	bundle, _ := json.Marshal(adaptive.EvidenceBundle{SessionID: "session-test", CampaignID: campaign, Completeness: adaptive.EvidenceCompleteness{State: "complete"}})
	for _, name := range []string{"execution.json", "state.jsonl", "events.jsonl", "manifest.json", "evidence-bundle.json"} {
		data := []byte("{}")
		if name == "evidence-bundle.json" {
			data = bundle
		}
		if err := archive.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0600}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	original := client
	defer func() { client = original }()
	for _, name := range []string{"valid", "bad-hash", "truncated", "extra-byte", "over-limit", "missing-length"} {
		t.Run(name, func(t *testing.T) {
			data := source.Bytes()
			length := int64(len(data))
			maximum := length
			hash := digest.Bytes(data)
			switch name {
			case "bad-hash":
				hash = digest.Bytes([]byte("wrong"))
			case "truncated":
				data = data[:len(data)-1]
			case "extra-byte":
				length--
			case "over-limit":
				maximum--
			case "missing-length":
				length = -1
			}
			client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, ContentLength: length, Header: http.Header{"X-Content-Sha256": []string{hash}, "X-Evidence-Max-Bytes": []string{strconv.Itoa(source.Len())}}, Body: io.NopCloser(bytes.NewReader(data))}, nil
			})}
			output := t.TempDir()
			var failure any
			func() { defer func() { failure = recover() }(); evidence(output, "session-test", maximum) }()
			if (failure == nil) != (name == "valid") {
				t.Fatalf("unexpected result: %v", failure)
			}
			entries, err := os.ReadDir(output)
			if err != nil {
				t.Fatal(err)
			}
			if name != "valid" && len(entries) != 0 {
				t.Fatal("failed download retained output", entries)
			}
			if name == "valid" {
				raw, err := os.ReadFile(filepath.Join(output, "session-test.tar"))
				if err != nil || !bytes.Equal(raw, source.Bytes()) {
					t.Fatal("committed archive changed", err)
				}
			}
		})
	}
}
