package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evidence"
)

type evidenceRuntime struct {
	*fakeRuntime
	size int64
	fail bool
	seen int64
	path string
}

func (f *evidenceRuntime) Export(_ context.Context, _ string, maximum int64) (*os.File, error) {
	f.seen = maximum
	if f.fail {
		return nil, errors.Join(errors.New("generation refused"), evidence.ErrLimitExceeded)
	}
	file, err := os.CreateTemp(f.root, "evidence-capacity-")
	if err != nil {
		return nil, err
	}
	f.path = file.Name()
	if err = file.Truncate(f.size); err != nil {
		file.Close()
		os.Remove(f.path)
		return nil, err
	}
	return file, nil
}
func TestEvidenceLimitAdvertisedAndEnforced(t *testing.T) {
	s, f := fixture(t, true)
	if s.evidenceMaxBytes != evidence.DefaultMaxBytes {
		t.Fatal(s.evidenceMaxBytes)
	}
	status := post(t, s, "/v1/status", map[string]string{"api_version": Version, "campaign_id": "campaign"})
	var body struct {
		Maximum int64 `json:"evidence_max_bytes"`
	}
	if err := json.Unmarshal(status.Body, &body); err != nil || body.Maximum != 4<<30 {
		t.Fatal(string(status.Body), err)
	}
	attach := post(t, s, "/v1/attach", AttachRequest{APIVersion: Version, CampaignID: "campaign", WorkerInstanceID: "worker", AllowTargetStop: true})
	if err := json.Unmarshal(attach.Body, &body); err != nil || body.Maximum != 4<<30 {
		t.Fatal(string(attach.Body), err)
	}
	for _, generationFailure := range []bool{false, true} {
		runtime := &evidenceRuntime{fakeRuntime: f, size: 17, fail: generationFailure}
		s.runtime = runtime
		s.evidenceMaxBytes = 16
		result := post(t, s, "/v1/evidence", map[string]string{"api_version": Version, "campaign_id": "campaign", "session_id": "session-1"})
		var failure struct {
			Code     string `json:"code"`
			Maximum  int64  `json:"maximum_bytes"`
			Retained bool   `json:"evidence_retained"`
		}
		if err := json.Unmarshal(result.Body, &failure); err != nil {
			t.Fatal(err)
		}
		if result.Status != 413 || failure.Code != "evidence_limit_exceeded" || failure.Maximum != 16 || !failure.Retained || runtime.seen != 16 {
			t.Fatal(result, failure, runtime.seen)
		}
		if s.state.Closed || s.state.Phase != "ready" {
			t.Fatal("evidence failure changed execution state")
		}
		if runtime.path != "" {
			if _, err := os.Stat(runtime.path); !os.IsNotExist(err) {
				t.Fatal("temporary archive retained", err)
			}
		}
	}
}
func TestEvidenceBeyondFormerCeilingAndExactBoundary(t *testing.T) {
	s, f := fixture(t, true)
	size := int64(256<<20) + 1
	runtime := &evidenceRuntime{fakeRuntime: f, size: size}
	s.runtime = runtime
	s.evidenceMaxBytes = size // Exact configured boundary, greater than the old ceiling.
	raw, _ := json.Marshal(map[string]string{"api_version": Version, "campaign_id": "campaign", "session_id": "session-1"})
	request := httptest.NewRequest("POST", "/v1/evidence", bytes.NewReader(raw))
	// Existing HTTP range support keeps this sparse-file capacity test small.
	request.Header.Set("Range", "bytes=0-0")
	writer := httptest.NewRecorder()
	s.ServeHTTP(writer, request)
	if writer.Code != http.StatusPartialContent || writer.Body.Len() != 1 {
		t.Fatal(writer.Code, writer.Body.Len())
	}
	if writer.Header().Get("X-Evidence-Max-Bytes") != strconv.FormatInt(size, 10) || !digest.Valid(writer.Header().Get("X-Content-SHA256")) {
		t.Fatal(writer.Header())
	}
	if _, err := os.Stat(runtime.path); !os.IsNotExist(err) {
		t.Fatal("temporary archive retained", err)
	}
	// A normal full transfer advertises its exact length and full-object digest.
	runtime.size = 17
	writer = httptest.NewRecorder()
	s.ServeHTTP(writer, httptest.NewRequest("POST", "/v1/evidence", bytes.NewReader(raw)))
	if writer.Code != 200 || writer.Header().Get("Content-Length") != "17" || writer.Header().Get("X-Content-SHA256") != digest.Bytes(writer.Body.Bytes()) {
		t.Fatal(writer.Code, writer.Header())
	}
}
