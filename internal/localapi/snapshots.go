package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"

	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/snapshot"
)

const MaxSnapshotPage = 1000
const MaxSnapshotInventory = 10000
const MaxSnapshotResponse = 4 << 20

type SnapshotListRequest struct {
	APIVersion      string `json:"api_version"`
	CampaignID      string `json:"campaign_id"`
	SourceSessionID string `json:"source_session_id,omitempty"`
	Offset          int    `json:"offset,omitempty"`
	Limit           int    `json:"limit,omitempty"`
}
type SnapshotListResponse struct {
	CampaignID  string                `json:"campaign_id"`
	Checkpoints []snapshot.Checkpoint `json:"checkpoints"`
	Total       int                   `json:"total"`
	NextOffset  int                   `json:"next_offset,omitempty"`
}
type SnapshotInspectRequest struct {
	APIVersion      string `json:"api_version"`
	CampaignID      string `json:"campaign_id"`
	SourceSessionID string `json:"source_session_id"`
	CheckpointID    string `json:"checkpoint_id"`
}

// Snapshot metadata is read from retained host storage, never by restarting a
// target or by asking a caller for a filesystem path. Capture campaign membership
// under the ledger lock; slow filesystem reads must not block terminal closure.
func (s *Service) snapshotSessions(campaign, source string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.attachedLocked(campaign) {
		return nil, false
	}
	if source != "" {
		if _, ok := s.state.Sessions[source]; !ok {
			return nil, false
		}
		return []string{source}, true
	}
	ids := make([]string, 0, len(s.state.Sessions))
	for id := range s.state.Sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, true
}
func writeSnapshotResponse(w http.ResponseWriter, body any) {
	result := response(200, body)
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > MaxSnapshotResponse {
		reject(w, 413, "snapshot_response_limit_exceeded")
		return
	}
	write(w, result)
}
func (s *Service) listSnapshots(w http.ResponseWriter, r *http.Request) {
	var input SnapshotListRequest
	if !read(w, r, &input, 4096) {
		return
	}
	if input.APIVersion != Version || input.Offset < 0 || input.Offset > MaxSnapshotInventory || input.Limit < 0 || input.Limit > MaxSnapshotPage {
		reject(w, 400, "invalid_snapshot_list")
		return
	}
	sessions, ok := s.snapshotSessions(input.CampaignID, input.SourceSessionID)
	if !ok {
		reject(w, 403, "session_not_in_campaign")
		return
	}
	items := make([]snapshot.Checkpoint, 0)
	for _, id := range sessions {
		records, err := s.runtime.ListSnapshots(r.Context(), id)
		if err != nil {
			reject(w, 503, "snapshot_inventory_unavailable")
			return
		}
		if len(records) > MaxSnapshotInventory-len(items) {
			reject(w, 413, "snapshot_inventory_limit_exceeded")
			return
		}
		for _, item := range records {
			if item.SourceSessionID != id || item.CampaignID != "" && item.CampaignID != input.CampaignID || snapshot.VerifyCheckpoint(item) != nil {
				reject(w, 503, "snapshot_inventory_invalid")
				return
			}
		}
		items = append(items, records...)
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		if items[i].SourceSessionID != items[j].SourceSessionID {
			return items[i].SourceSessionID < items[j].SourceSessionID
		}
		return items[i].ID < items[j].ID
	})
	if input.Limit == 0 {
		input.Limit = 100
	}
	start := min(input.Offset, len(items))
	end := min(start+input.Limit, len(items))
	out := SnapshotListResponse{CampaignID: input.CampaignID, Checkpoints: items[start:end], Total: len(items)}
	if end < len(items) {
		out.NextOffset = end
	}
	writeSnapshotResponse(w, out)
}
func (s *Service) inspectSnapshot(w http.ResponseWriter, r *http.Request) {
	var input SnapshotInspectRequest
	if !read(w, r, &input, 4096) {
		return
	}
	if input.APIVersion != Version || input.SourceSessionID == "" || !identifier.MatchString(input.CheckpointID) {
		reject(w, 400, "invalid_snapshot_inspect")
		return
	}
	if _, ok := s.snapshotSessions(input.CampaignID, input.SourceSessionID); !ok {
		reject(w, 403, "session_not_in_campaign")
		return
	}
	item, err := s.runtime.InspectSnapshot(r.Context(), input.SourceSessionID, input.CheckpointID)
	if errors.Is(err, os.ErrNotExist) {
		reject(w, 404, "checkpoint_not_found")
		return
	}
	if err != nil {
		reject(w, 503, "snapshot_metadata_unavailable")
		return
	}
	if item.SourceSessionID != input.SourceSessionID || item.ID != input.CheckpointID || item.CampaignID != "" && item.CampaignID != input.CampaignID || snapshot.VerifyCheckpoint(item) != nil {
		reject(w, 503, "snapshot_metadata_invalid")
		return
	}
	writeSnapshotResponse(w, item)
}

func (n *NativeRuntime) ListSnapshots(ctx context.Context, source string) ([]snapshot.Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, paths, err := session.Load(n.root, source)
	if err != nil {
		return nil, err
	}
	records, err := snapshot.ListCheckpoints(paths.Checkpoints)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return records, nil
}
func (n *NativeRuntime) InspectSnapshot(ctx context.Context, source, id string) (snapshot.Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return snapshot.Checkpoint{}, err
	}
	_, paths, err := session.Load(n.root, source)
	if err != nil {
		return snapshot.Checkpoint{}, err
	}
	return snapshot.LoadCheckpoint(paths.Checkpoints, id)
}
