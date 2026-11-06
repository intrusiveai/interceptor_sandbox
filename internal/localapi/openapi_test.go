package localapi

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/contracttest"
)

func TestActualHTTPResponsesConformToOpenAPI(t *testing.T) {
	v, err := contracttest.New()
	if err != nil {
		t.Fatal(err)
	}
	s, _ := fixture(t, true)
	for _, tc := range []struct {
		path string
		body any
	}{
		{"/v1/contract-version", map[string]any{}},
		{"/v1/contract-version", map[string]any{"unexpected": true}},
		{"/v1/status", map[string]any{"api_version": Version, "campaign_id": "campaign"}},
		{"/v1/status", map[string]any{"api_version": Version, "campaign_id": "wrong"}},
		{"/v1/snapshots/list", SnapshotListRequest{APIVersion: Version, CampaignID: "campaign"}},
		{"/v1/snapshots/inspect", SnapshotInspectRequest{APIVersion: Version, CampaignID: "campaign", SourceSessionID: "session-1", CheckpointID: "missing"}},
		{"/v1/lifecycle", LifecycleRequest{APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", WorkerInstanceID: "w", RunRevision: 1, OperationID: "stop-1", Operation: "session.stop"}},
	} {
		raw, _ := json.Marshal(tc.body)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("POST", tc.path, bytes.NewReader(raw)))
		if err := v.Exchange(tc.path, raw, w.Code, w.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
}
