package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

func (f *fakeRuntime) ListSnapshots(ctx context.Context, id string) ([]snapshot.Checkpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.snapshotError {
		return nil, errors.New("snapshot storage unavailable")
	}
	return append([]snapshot.Checkpoint{}, f.snapshots[id]...), nil
}
func (f *fakeRuntime) InspectSnapshot(ctx context.Context, id, checkpoint string) (snapshot.Checkpoint, error) {
	records, err := f.ListSnapshots(ctx, id)
	if err != nil {
		return snapshot.Checkpoint{}, err
	}
	for _, item := range records {
		if item.ID == checkpoint {
			return item, nil
		}
	}
	return snapshot.Checkpoint{}, os.ErrNotExist
}
func testCheckpoint(t *testing.T, source string, second int64) snapshot.Checkpoint {
	t.Helper()
	hash := digest.Bytes([]byte("test"))
	cp, err := snapshot.NewCheckpoint(source, "", hash, hash, 1, hash, hash, 123, 1, "baseline", "test", time.Unix(second, 0), snapshot.Metadata{CampaignID: "campaign", Description: "Before testing the next mutation"})
	if err != nil {
		t.Fatal(err)
	}
	return cp
}
func listCheckpoints(t *testing.T, s *Service, input SnapshotListRequest) SnapshotListResponse {
	t.Helper()
	got := post(t, s, "/v1/snapshots/list", input)
	var result SnapshotListResponse
	if got.Status != 200 || json.Unmarshal(got.Body, &result) != nil {
		t.Fatalf("list: %d %s", got.Status, got.Body)
	}
	return result
}
func TestSnapshotInventoryAcrossRestoreAndStop(t *testing.T) {
	s, f := fixture(t, true)
	first := testCheckpoint(t, "session-1", 1)
	f.snapshots = map[string][]snapshot.Checkpoint{"session-1": {first}}
	req := SnapshotListRequest{APIVersion: Version, CampaignID: "campaign", Limit: 1}
	page := listCheckpoints(t, s, req)
	if page.Total != 1 || page.Checkpoints[0].Description != first.Description || page.Checkpoints[0].CampaignID != "campaign" {
		t.Fatal(page)
	}
	restore := LifecycleRequest{APIVersion: Version, CampaignID: "campaign", WorkerInstanceID: "worker", RunRevision: 1, SessionID: "session-1", OperationID: "restore", Operation: "snapshot.restore", CheckpointID: first.ID}
	if got := post(t, s, "/v1/lifecycle", restore); got.Status != 200 {
		t.Fatal(got)
	}
	second := testCheckpoint(t, "session-2", 2)
	f.snapshots["session-2"] = []snapshot.Checkpoint{second}
	page = listCheckpoints(t, s, req)
	if page.Total != 2 || len(page.Checkpoints) != 1 || page.Checkpoints[0].ID != first.ID || page.NextOffset != 1 {
		t.Fatal(page)
	}
	req.Offset = page.NextOffset
	page = listCheckpoints(t, s, req)
	if len(page.Checkpoints) != 1 || page.Checkpoints[0].ID != second.ID || page.NextOffset != 0 {
		t.Fatal(page)
	}
	inspect := SnapshotInspectRequest{APIVersion: Version, CampaignID: "campaign", SourceSessionID: "session-1", CheckpointID: first.ID}
	got := post(t, s, "/v1/snapshots/inspect", inspect)
	var cp snapshot.Checkpoint
	if got.Status != 200 || json.Unmarshal(got.Body, &cp) != nil || cp != first {
		t.Fatalf("inspect: %s", got.Body)
	}
	// The same original baseline remains selectable after a prior restore.
	restore.SessionID = "session-2"
	restore.SourceSessionID = "session-1"
	restore.OperationID = "restore-baseline-again"
	restore.RunRevision = 2
	if got = post(t, s, "/v1/lifecycle", restore); got.Status != 200 || f.restores != 2 {
		t.Fatal(got)
	}
	stop := LifecycleRequest{APIVersion: Version, CampaignID: "campaign", WorkerInstanceID: "worker", RunRevision: 3, SessionID: "session-3", OperationID: "stop", Operation: "session.stop"}
	if got = post(t, s, "/v1/lifecycle", stop); got.Status != 200 {
		t.Fatal(got)
	}
	req.Offset = 0
	req.SourceSessionID = "session-1"
	page = listCheckpoints(t, s, req)
	if page.Total != 1 || page.Checkpoints[0].ID != first.ID || f.active != "" {
		t.Fatal("stopped inventory unavailable or target restarted")
	}
}
func TestSnapshotInventoryScopeValidationAndErrors(t *testing.T) {
	s, f := fixture(t, true)
	cp := testCheckpoint(t, "session-1", 1)
	f.snapshots = map[string][]snapshot.Checkpoint{"session-1": {cp}}
	for _, input := range []SnapshotListRequest{
		{APIVersion: Version, CampaignID: "other"},
		{APIVersion: Version, CampaignID: "campaign", SourceSessionID: "foreign-session"},
	} {
		if got := post(t, s, "/v1/snapshots/list", input); got.Status != 403 {
			t.Fatal(got)
		}
	}
	for _, input := range []SnapshotListRequest{
		{APIVersion: Version, CampaignID: "campaign", Offset: -1},
		{APIVersion: Version, CampaignID: "campaign", Limit: MaxSnapshotPage + 1},
	} {
		if got := post(t, s, "/v1/snapshots/list", input); got.Status != 400 {
			t.Fatal(got)
		}
	}
	inspect := SnapshotInspectRequest{APIVersion: Version, CampaignID: "other", SourceSessionID: "session-1", CheckpointID: cp.ID}
	if got := post(t, s, "/v1/snapshots/inspect", inspect); got.Status != 403 {
		t.Fatal(got)
	}
	inspect.CampaignID = "campaign"
	inspect.CheckpointID = "missing"
	if got := post(t, s, "/v1/snapshots/inspect", inspect); got.Status != 404 {
		t.Fatal(got)
	}
	req := SnapshotListRequest{APIVersion: Version, CampaignID: "campaign"}
	f.snapshotError = true
	if got := post(t, s, "/v1/snapshots/list", req); got.Status != 503 {
		t.Fatal(got)
	}
	f.snapshotError = false
	f.snapshots["session-1"][0].CampaignID = "other"
	if got := post(t, s, "/v1/snapshots/list", req); got.Status != 503 {
		t.Fatal("foreign/corrupt metadata disclosed")
	}
	f.snapshots["session-1"] = make([]snapshot.Checkpoint, MaxSnapshotInventory+1)
	if got := post(t, s, "/v1/snapshots/list", req); got.Status != 413 {
		t.Fatal(got)
	}
	f.snapshots = map[string][]snapshot.Checkpoint{}
	page := listCheckpoints(t, s, req)
	if page.Total != 0 || page.Checkpoints == nil {
		t.Fatal("empty inventory must be []")
	}
	if s.state.Closed {
		t.Fatal("read error closed healthy execution")
	}
}
func TestNativeSnapshotInventoryUsesRetainedFiles(t *testing.T) {
	root := t.TempDir()
	source := "sess-1-000000000000000000000000"
	metadata := session.Metadata{SchemaVersion: 1, ID: source, Phase: session.Stopped}
	paths, events, journal, err := session.Initialize(root, metadata, digest.Bytes([]byte("baseline")))
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	defer journal.Close()
	cp := testCheckpoint(t, source, 1)
	if err = snapshot.WriteCheckpoint(paths.Checkpoints, cp); err != nil {
		t.Fatal(err)
	}
	n := &NativeRuntime{root: root}
	got, err := n.ListSnapshots(t.Context(), source)
	if err != nil || len(got) != 1 || got[0] != cp {
		t.Fatalf("list: %+v %v", got, err)
	}
	one, err := n.InspectSnapshot(t.Context(), source, cp.ID)
	if err != nil || one != cp {
		t.Fatal(one, err)
	}
	if err = os.WriteFile(filepath.Join(paths.Checkpoints, cp.ID+".json"), []byte(`{"bad":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = n.ListSnapshots(t.Context(), source); err == nil {
		t.Fatal("corrupt checkpoint silently omitted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = n.ListSnapshots(ctx, source); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
