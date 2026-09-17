package adaptive

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/intrusive-ai/interceptor-sandbox/internal/blobstore"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/fsutil"
)

const MaxFeedbackChunk = 256 << 10
const MaxFeedbackArtifact = 16 << 20
const MaxFeedbackEntries = 64
const maxFeedbackManifest = 256 << 10

var FeedbackKinds = []string{"target_output", "operation_error", "injection_delivery", "oracle_outcome"}
var feedbackID = regexp.MustCompile(`^feedback-[0-9a-f]{64}$`)

type ObservationSelection struct {
	Mode  string   `json:"mode"`
	Kinds []string `json:"kinds,omitempty"`
}

func ValidateObservationSelection(s *ObservationSelection) error {
	if s == nil {
		return nil
	}
	if s.Mode == "all-permitted" && len(s.Kinds) == 0 {
		return nil
	}
	if s.Mode != "selected" || len(s.Kinds) == 0 || len(s.Kinds) > len(FeedbackKinds) {
		return errors.New("invalid observation selection")
	}
	seen := map[string]bool{}
	for _, kind := range s.Kinds {
		valid := false
		for _, known := range FeedbackKinds {
			valid = valid || kind == known
		}
		if !valid || seen[kind] {
			return errors.New("unknown or duplicate observation kind")
		}
		seen[kind] = true
	}
	return nil
}
func FeedbackSelected(s *ObservationSelection, kind string) bool {
	if s == nil || s.Mode == "all-permitted" {
		return true
	}
	for _, k := range s.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}
func FeedbackPermitted(profile, kind string) bool {
	switch profile {
	case "black-box":
		return kind == "target_output"
	case "diagnostic":
		return kind == "target_output" || kind == "operation_error" || kind == "injection_delivery"
	case "oracle-assisted":
		return kind == "target_output" || kind == "operation_error" || kind == "injection_delivery" || kind == "oracle_outcome"
	}
	return false
}

type FeedbackCategory struct {
	Kind   string `json:"kind"`
	State  string `json:"state"` // available, empty, withheld, not_requested, unavailable, partial
	Reason string `json:"reason,omitempty"`
}
type FeedbackEntry struct {
	ID                string              `json:"id"`
	Kind              string              `json:"kind"`
	Visibility        Visibility          `json:"visibility"`
	Source            string              `json:"source"`
	Assurance         string              `json:"assurance"`
	Availability      string              `json:"availability"`
	Reason            string              `json:"reason,omitempty"`
	Artifact          *ArtifactDescriptor `json:"artifact,omitempty"`
	Truncated         bool                `json:"truncated"`
	OriginalSizeBytes int64               `json:"original_size_bytes"`
}
type ObservationRequest struct {
	TurnID    string `json:"turn_id,omitempty"`
	ReceiptID string `json:"receipt_id,omitempty"`
}

func (r ObservationRequest) Validate() error {
	if r.TurnID != "" && (!boundedID(r.TurnID) || r.ReceiptID != "") || r.ReceiptID != "" && !feedbackID.MatchString(r.ReceiptID) {
		return errors.New("invalid feedback lookup")
	}
	return nil
}

type FeedbackReadRequest struct {
	ReceiptID string `json:"receipt_id"`
	EntryID   string `json:"entry_id"`
	Offset    int64  `json:"offset"`
	MaxBytes  int    `json:"max_bytes"`
}

func (r FeedbackReadRequest) Validate() error {
	if !feedbackID.MatchString(r.ReceiptID) || !boundedID(r.EntryID) || r.Offset < 0 || r.MaxBytes <= 0 || r.MaxBytes > MaxFeedbackChunk {
		return errors.New("invalid feedback range")
	}
	return nil
}

type FeedbackChunk struct {
	ReceiptID string        `json:"receipt_id"`
	Entry     FeedbackEntry `json:"entry"`
	Offset    int64         `json:"offset"`
	Content   []byte        `json:"content"`
	RawLength int           `json:"raw_length"`
	EOF       bool          `json:"eof"`
}

func FeedbackReceiptID(session, turn string) string {
	return "feedback-" + digest.Bytes([]byte(session + "\x00" + turn))[7:]
}
func feedbackHash(v ObservationView) string {
	v.Hash = ""
	raw, _ := json.Marshal(v)
	return digest.Bytes(raw)
}
func SaveFeedback(root string, v ObservationView) error {
	if !feedbackID.MatchString(v.ReceiptID) || v.ReceiptID != FeedbackReceiptID(v.SessionID, v.TurnID) {
		return errors.New("invalid feedback identity")
	}
	v.Hash = feedbackHash(v)
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > maxFeedbackManifest {
		return errors.New("feedback manifest limit")
	}
	dir := filepath.Join(root, "feedback")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Callers serialize publication; a receipt is never overwritten with new facts.
	if _, err = os.Stat(filepath.Join(dir, v.ReceiptID+".json")); err == nil {
		return errors.New("feedback already published")
	} else if !os.IsNotExist(err) {
		return err
	}
	return fsutil.WriteJSON(filepath.Join(dir, v.ReceiptID+".json"), v, 0600)
}
func LoadFeedback(root, id string) (ObservationView, error) {
	var v ObservationView
	if !feedbackID.MatchString(id) {
		return v, errors.New("invalid feedback receipt")
	}
	f, err := os.Open(filepath.Join(root, "feedback", id+".json"))
	if err != nil {
		return v, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxFeedbackManifest+1))
	if err != nil || len(raw) > maxFeedbackManifest || StrictJSON(raw, &v) != nil {
		return v, errors.New("invalid feedback manifest")
	}
	if v.APIVersion != ObservationViewVersion || v.ReceiptID != id || v.ReceiptID != FeedbackReceiptID(v.SessionID, v.TurnID) || v.Hash != feedbackHash(v) || len(v.Entries) > MaxFeedbackEntries {
		return v, errors.New("feedback integrity mismatch")
	}
	return v, nil
}
func scopedFeedback(root string, r OperationRequest, id string) (ObservationView, GatewayResponse) {
	v, err := LoadFeedback(root, id)
	if os.IsNotExist(err) {
		return v, OperationError(404, "feedback_unavailable")
	}
	if err != nil {
		return v, OperationError(503, "feedback_integrity_unavailable")
	}
	if v.SessionID != r.SessionID || v.CampaignID != r.CampaignID || v.AttemptID != r.AttemptID || v.AttemptContextDigest != r.AttemptContextDigest {
		return v, OperationError(403, "feedback_binding_mismatch")
	}
	return v, GatewayResponse{}
}
func RetainedFeedback(root string, r OperationRequest, id string) GatewayResponse {
	v, failure := scopedFeedback(root, r, id)
	if failure.Status != 0 {
		return failure
	}
	raw, _ := json.Marshal(v)
	return GatewayResponse{Status: 200, Body: raw, SessionRevision: v.SessionRevision}
}
func ReadFeedback(root string, blobs *blobstore.Store, r OperationRequest, input FeedbackReadRequest) GatewayResponse {
	if input.Validate() != nil {
		return OperationError(400, "invalid_feedback_range")
	}
	v, failure := scopedFeedback(root, r, input.ReceiptID)
	if failure.Status != 0 {
		return failure
	}
	for _, entry := range v.Entries {
		if entry.ID != input.EntryID {
			continue
		}
		if !FeedbackPermitted(v.FeedbackProfile, entry.Kind) || (entry.Visibility != HarnessVisible && entry.Visibility != TargetVisible) {
			return OperationError(403, "feedback_not_permitted")
		}
		result := FeedbackChunk{ReceiptID: v.ReceiptID, Entry: entry, Offset: input.Offset, Content: []byte{}}
		if entry.Availability == "available" && entry.Artifact != nil {
			if input.Offset > entry.Artifact.SizeBytes {
				return OperationError(416, "feedback_range_outside_content")
			}
			reader, object, err := blobs.Open(entry.Artifact.Digest)
			if err != nil {
				result.Entry.Availability = "unavailable"
				result.Entry.Reason = "content_missing_or_corrupt"
			} else {
				defer reader.Close()
				if object.Size != entry.Artifact.SizeBytes {
					return OperationError(503, "feedback_content_mismatch")
				}
				if _, err = io.CopyN(io.Discard, reader, input.Offset); err != nil {
					return OperationError(503, "feedback_read_failed")
				}
				result.Content, err = io.ReadAll(io.LimitReader(reader, int64(input.MaxBytes)))
				if err != nil {
					return OperationError(503, "feedback_read_failed")
				}
				result.RawLength = len(result.Content)
				result.EOF = input.Offset+int64(result.RawLength) == entry.Artifact.SizeBytes
			}
		}
		raw, _ := json.Marshal(result)
		return GatewayResponse{Status: 200, Body: raw, SessionRevision: v.SessionRevision}
	}
	return OperationError(404, "feedback_entry_not_found")
}
