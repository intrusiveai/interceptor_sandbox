package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evaluation"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEvidenceDownloadVerifiesBeforeCommit(t *testing.T) {
	var source bytes.Buffer
	archive := tar.NewWriter(&source)
	metadata := session.Metadata{SchemaVersion: 1, ID: "session-test", CampaignID: campaign, Phase: session.Stopped, FeedbackProfile: "black-box", CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC()}
	report := evaluation.Report{}
	bundle, err := adaptive.BuildEvidenceBundle(metadata, nil, nil, nil, nil, report, true)
	if err != nil {
		t.Fatal(err)
	}
	execution := adaptive.ExecutionState{APIVersion: adaptive.ExecutionStateVersion, SessionID: metadata.ID, Owner: &adaptive.ExecutionOwner{CampaignID: campaign, Principal: "operator", WorkerInstanceID: "test-worker", RunRevision: 1, BoundAt: metadata.CreatedAt, ClosedAt: &metadata.UpdatedAt, ClosureReason: "session_stopped"}, Records: map[string]*adaptive.OperationRecord{}}
	bundle, err = adaptive.BindExecutionEvidence(bundle, execution)
	if err != nil {
		t.Fatal(err)
	}
	members := map[string]any{
		"execution.json":     execution,
		"session.json":       metadata,
		"manifest.json":      map[string]any{"schema_version": 1, "session_id": metadata.ID, "environment_digest": digest.Bytes(nil), "application_digest": digest.Bytes(nil), "interceptor_version": "test"},
		"file-manifest.json": snapshot.EmptyManifest(), "final-evaluation.json": report, "evidence-bundle.json": bundle,
		"state.jsonl": nil, "events.jsonl": nil,
	}
	for name, value := range members {
		var data []byte
		if value != nil {
			data, _ = json.Marshal(value)
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
