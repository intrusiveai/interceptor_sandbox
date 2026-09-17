package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

func TestSnapshotCLIMetadataRoundTrip(t *testing.T) {
	// Exercise the CLI's real private transport and persisted inventory without Docker.
	root, err := os.MkdirTemp("/tmp", "ix-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("INTERCEPTOR_DATA_DIR", root)
	source := "sess-1-000000000000000000000000"
	paths, events, journal, err := session.Initialize(root, session.Metadata{SchemaVersion: 1, ID: source, Phase: session.Running, ContainerID: "test", BridgeTransport: "unix", Limits: session.Limits{MaxBlobBytes: 1 << 20}}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = events.Close(); _ = journal.Close() })
	listener, err := net.Listen("unix", filepath.Join(paths.Sockets, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/snapshots" {
			http.Error(w, "wrong route", 400)
			return
		}
		var input struct {
			CampaignID  string `json:"campaign_id"`
			Description string `json:"description"`
			Label       string `json:"label"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		h := digest.Bytes([]byte("test"))
		cp, err := snapshot.NewCheckpoint(source, "", h, h, 1, h, h, 0, 1, input.Label, "test", time.Now(), snapshot.Metadata{CampaignID: input.CampaignID, Description: input.Description})
		if err == nil {
			err = snapshot.WriteCheckpoint(paths.Checkpoints, cp)
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(cp)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	var out bytes.Buffer
	if err := snapshotCommand([]string{"create", source, "--name", "baseline", "--campaign-id", "campaign-1", "--description", "Before branch B — baseline", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var created snapshot.Checkpoint
	if err := json.Unmarshal(out.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.CampaignID != "campaign-1" || created.Description != "Before branch B — baseline" || created.Label != "baseline" {
		t.Fatal(created)
	}
	for _, args := range [][]string{{"list", source, "--json"}, {"inspect", source, created.ID}} {
		out.Reset()
		if err := snapshotCommand(args, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), `"campaign_id": "campaign-1"`) || !strings.Contains(out.String(), created.Description) {
			t.Fatalf("missing metadata: %s", &out)
		}
	}
	out.Reset()
	if err := snapshotCommand([]string{"list", source}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "campaign-1") || !strings.Contains(out.String(), created.Description) {
		t.Fatal(out.String())
	}
	if err := snapshotCommand([]string{"create", source, "--description", strings.Repeat("x", 4097)}, &out); err == nil {
		t.Fatal("accepted oversized description")
	}
}
