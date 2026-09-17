package adaptive

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
)

func TestFeedbackSelectionValidation(t *testing.T) {
	for _, s := range []*ObservationSelection{nil, {Mode: "all-permitted"}, {Mode: "selected", Kinds: []string{"target_output", "oracle_outcome"}}} {
		if err := ValidateObservationSelection(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []*ObservationSelection{{Mode: "all"}, {Mode: "selected"}, {Mode: "all-permitted", Kinds: []string{"target_output"}}, {Mode: "selected", Kinds: []string{"protected"}}, {Mode: "selected", Kinds: []string{"target_output", "target_output"}}} {
		if ValidateObservationSelection(s) == nil {
			t.Fatal("invalid selection accepted", s)
		}
	}
}
func TestFeedbackReceiptScopeIntegrityAndEmptyOutput(t *testing.T) {
	root := t.TempDir()
	blobs, err := blobstore.Open(filepath.Join(root, "blobs"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := blobs.Put(t.Context(), bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	v := ObservationView{APIVersion: ObservationViewVersion, SessionID: "session", CampaignID: "campaign", AttemptID: "attempt", AttemptContextDigest: digest.Bytes([]byte("attempt")), TurnID: "turn", FeedbackProfile: "black-box", CollectionState: "complete", Entries: []FeedbackEntry{{ID: "entry-1", Kind: "target_output", Visibility: TargetVisible, Availability: "available", Artifact: &ArtifactDescriptor{Digest: empty.Digest, MediaType: "text/plain", Canonicalization: "raw"}}}}
	v.ReceiptID = FeedbackReceiptID(v.SessionID, v.TurnID)
	if err = SaveFeedback(root, v); err != nil {
		t.Fatal(err)
	}
	if SaveFeedback(root, v) == nil {
		t.Fatal("receipt overwrote previous view")
	}
	r := OperationRequest{SessionID: v.SessionID, CampaignID: v.CampaignID, AttemptID: v.AttemptID, AttemptContextDigest: v.AttemptContextDigest, WorkerInstanceID: "new-worker", RunRevision: 10}
	input := FeedbackReadRequest{ReceiptID: v.ReceiptID, EntryID: "entry-1", MaxBytes: 10}
	got := ReadFeedback(root, blobs, r, input)
	var chunk FeedbackChunk
	_ = json.Unmarshal(got.Body, &chunk)
	if got.Status != 200 || chunk.Entry.Availability != "available" || !chunk.EOF || chunk.RawLength != 0 {
		t.Fatal(got)
	}
	for _, alter := range []func(*OperationRequest){func(q *OperationRequest) { q.CampaignID = "foreign" }, func(q *OperationRequest) { q.SessionID = "replacement" }, func(q *OperationRequest) { q.AttemptID = "different" }, func(q *OperationRequest) { q.AttemptContextDigest = digest.Bytes([]byte("different")) }} {
		q := r
		alter(&q)
		if got := ReadFeedback(root, blobs, q, input); got.Status != 403 {
			t.Fatal("unrelated binding read content", got)
		}
	}
	input.EntryID = "guessed"
	if got := ReadFeedback(root, blobs, r, input); got.Status != 404 {
		t.Fatal(got)
	}
	input.EntryID = "entry-1"
	path := filepath.Join(root, "feedback", v.ReceiptID+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"black-box"`), []byte(`"oracle-assisted"`), 1)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if got := ReadFeedback(root, blobs, r, input); got.Status != 503 {
		t.Fatal("corrupt receipt read", got)
	}
	if _, err := LoadFeedback(root, "../../execution"); err == nil {
		t.Fatal("path receipt accepted")
	}
	if !strings.HasPrefix(v.ReceiptID, "feedback-") {
		t.Fatal(v.ReceiptID)
	}
}
