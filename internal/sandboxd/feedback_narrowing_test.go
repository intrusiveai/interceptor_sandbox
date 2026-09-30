package sandboxd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
)

// A narrower harness view must use native selection, not replace the session profile.
func TestFeedbackNarrowingPreservesNativeProfileAndRetainedReads(t *testing.T) {
	selection := &adaptive.ObservationSelection{Mode: "selected", Kinds: []string{"target_output"}}
	s, g, a := feedbackFixture(t, "diagnostic", selection)
	wrong := a
	wrong.AttemptID = "wrong-profile"
	wrong.AttemptIndex = 2
	wrong.FeedbackProfile = "black-box"
	if _, err := s.attempts.Register(wrong); err == nil || !strings.Contains(err.Error(), "immutable session policy") {
		t.Fatalf("effective harness profile accepted as native profile: %v", err)
	}
	now := time.Now().UTC()
	body := []byte("target response")
	turn := Turn{ID: "turn-narrow", AttemptID: a.AttemptID, Status: "complete", Body: body, MediaType: "text/plain", Started: now.Add(-time.Second), Finished: now}
	s.turns[turn.ID] = turn
	for _, event := range []recording.Draft{
		{Provenance: recording.Application, Channel: recording.Evaluation, Layer: "injection", Kind: "injection_applied", AttemptID: a.AttemptID, InjectionID: "hidden-injection", TurnID: turn.ID, Semantic: map[string]any{"applied": true}},
		{Provenance: recording.Application, Channel: recording.Attack, Layer: "application_bridge", Kind: "turn_completed", AttemptID: a.AttemptID, TurnID: turn.ID, Semantic: map[string]any{"body": string(body)}},
	} {
		if _, err := s.recorder.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	view := collectFeedback(t, s, g, a, adaptive.ObservationRequest{TurnID: turn.ID})
	if view.FeedbackProfile != "diagnostic" || view.CollectionState != "complete" || len(view.Entries) != 1 || view.Entries[0].Kind != "target_output" {
		t.Fatalf("unexpected native view: %+v", view)
	}
	if len(view.Categories) != 4 {
		t.Fatal(view.Categories)
	}
	for _, category := range view.Categories {
		want := "not_requested"
		if category.Kind == "target_output" {
			want = "available"
		}
		if category.State != want {
			t.Fatalf("category %+v; want %s", category, want)
		}
	}
	// Operator must subsequently translate these native not_requested states to withheld.
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hidden-injection") {
		t.Fatal("unselected delivery escaped through an alias")
	}
	s.metadata.Phase = session.Stopped
	if err := session.Save(s.paths.Metadata, s.metadata); err != nil {
		t.Fatal(err)
	}
	retained := collectFeedback(t, s, g, a, adaptive.ObservationRequest{ReceiptID: view.ReceiptID})
	if retained.Hash != view.Hash || retained.FeedbackProfile != "diagnostic" {
		t.Fatal("retained view changed")
	}
	got := nativeCall(t, g, s, "retained-narrow-read", "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: view.ReceiptID, EntryID: view.Entries[0].ID, MaxBytes: 256}, &a)
	var chunk adaptive.FeedbackChunk
	if got.Status != 200 || json.Unmarshal(got.Body, &chunk) != nil || string(chunk.Content) != string(body) || !chunk.EOF {
		t.Fatalf("retained narrowed content: %d %s", got.Status, got.Body)
	}
}
