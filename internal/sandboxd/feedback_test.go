package sandboxd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
)

func feedbackFixture(t *testing.T, profile string, selection *adaptive.ObservationSelection) (*Server, http.Handler, adaptive.AttemptContext) {
	t.Helper()
	s, _ := nativeOperationServer(t)
	s.metadata.FeedbackProfile = profile
	var err error
	s.attempts, err = adaptive.OpenAttemptRegistry(filepath.Join(s.paths.Root, "feedback-attempts.json"), profile, 2, s.artifacts)
	if err != nil {
		t.Fatal(err)
	}
	g := nativeGateway(t, s)
	owner := adaptive.OwnerRequest{Action: "bind", SessionID: s.metadata.ID, CampaignID: "campaign", WorkerInstanceID: "worker"}
	if got := nativeCall(t, g, s, "bind", "session.owner", owner, nil); got.Status != 200 {
		t.Fatal(got)
	}
	payload := []byte(`"payload"`)
	desc := adaptive.ArtifactDescriptor{Digest: digest.Bytes(payload), SizeBytes: int64(len(payload)), MediaType: "application/json", Canonicalization: "raw"}
	if got := nativeCall(t, g, s, "artifact", "artifact.register", artifactRegistration{Descriptor: desc, Content: payload}, nil); got.Status != 201 {
		t.Fatal(got)
	}
	a := adaptive.AttemptContext{APIVersion: adaptive.AttemptContextVersion, CampaignID: "campaign", ThreadID: "thread", AttemptID: "attempt", Generation: 1, AttemptIndex: 1, Payload: desc, Generator: adaptive.Generator{Kind: "test"}, FeedbackProfile: profile, CreatedAt: time.Now().UTC(), ObservationSelection: selection}
	if got := nativeCall(t, g, s, "attempt", "attempt.register", a, &a); got.Status != 201 {
		t.Fatal(got)
	}
	return s, g, a
}
func collectFeedback(t *testing.T, s *Server, g http.Handler, a adaptive.AttemptContext, body any) adaptive.ObservationView {
	t.Helper()
	got := nativeCall(t, g, s, "feedback", "observation.read", body, &a)
	if got.Status != 200 {
		t.Fatalf("feedback: %d %s", got.Status, got.Body)
	}
	var v adaptive.ObservationView
	if err := json.Unmarshal(got.Body, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func TestFeedbackProfilesAndStableReceipts(t *testing.T) {
	for _, profile := range []string{"black-box", "diagnostic", "oracle-assisted"} {
		t.Run(profile, func(t *testing.T) {
			s, g, a := feedbackFixture(t, profile, nil)
			s.blueprint.Oracles = []environment.Oracle{{ID: "detector", Kind: "output_contains", Value: "visible"}}
			now := time.Now().UTC()
			body := []byte("visible target response")
			turn := Turn{ID: "turn-1", AttemptID: a.AttemptID, Status: "complete", Body: body, MediaType: "text/plain", Started: now.Add(-time.Second), Finished: now}
			s.turns[turn.ID] = turn
			_, _ = s.recorder.Append(recording.Draft{Provenance: recording.Application, Channel: recording.Evaluation, Layer: "injection", Kind: "injection_applied", AttemptID: a.AttemptID, InjectionID: "setup-1", TurnID: turn.ID, Semantic: map[string]any{"applied": true, "error": "secret detail must not escape", "private": "hidden"}})
			_, _ = s.recorder.Append(recording.Draft{Provenance: recording.Application, Channel: recording.Attack, Layer: "application_bridge", Kind: "turn_completed", AttemptID: a.AttemptID, TurnID: turn.ID, Semantic: map[string]any{"body": string(body)}})
			v := collectFeedback(t, s, g, a, adaptive.ObservationRequest{TurnID: turn.ID})
			encoded, _ := json.Marshal(v)
			if v.CollectionState != "complete" || v.ThroughEventSeq == 0 || v.TargetOutput.MediaType != "text/plain" {
				t.Fatalf("bad manifest: %s", encoded)
			}
			if strings.Contains(string(encoded), "secret detail") || strings.Contains(string(encoded), "hidden") {
				t.Fatal("raw diagnostics escaped")
			}
			counts := map[string]int{}
			for _, entry := range v.Entries {
				counts[entry.Kind]++
			}
			if counts["target_output"] != 1 || (counts["injection_delivery"] > 0) != (profile != "black-box") || (counts["oracle_outcome"] > 0) != (profile == "oracle-assisted") {
				t.Fatal(counts)
			}
			// Later facts cannot rewrite a previously published view.
			_, _ = s.recorder.Append(recording.Draft{Provenance: recording.Application, Channel: recording.Attack, Layer: "test", Kind: "turn_completed", AttemptID: a.AttemptID, TurnID: "turn-later", Semantic: map[string]any{"body": "late"}})
			again := collectFeedback(t, s, g, a, adaptive.ObservationRequest{ReceiptID: v.ReceiptID})
			raw, _ := json.Marshal(again)
			if !bytes.Equal(raw, encoded) {
				t.Fatal("receipt changed")
			}
			var assembled []byte
			for offset := int64(0); ; {
				got := nativeCall(t, g, s, "chunk", "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: v.ReceiptID, EntryID: v.Entries[0].ID, Offset: offset, MaxBytes: 3}, &a)
				var chunk adaptive.FeedbackChunk
				if got.Status != 200 || json.Unmarshal(got.Body, &chunk) != nil {
					t.Fatal(got)
				}
				assembled = append(assembled, chunk.Content...)
				offset += int64(chunk.RawLength)
				if chunk.EOF {
					break
				}
			}
			if !bytes.Equal(assembled, body) || digest.Bytes(assembled) != v.Entries[0].Artifact.Digest {
				t.Fatal("chunk assembly differs")
			}
			// Retained reads use exactly the same receipt and never start a target.
			s.metadata.Phase = session.Stopped
			if err := session.Save(s.paths.Metadata, s.metadata); err != nil {
				t.Fatal(err)
			}
			old := collectFeedback(t, s, g, a, adaptive.ObservationRequest{ReceiptID: v.ReceiptID})
			if old.Hash != v.Hash {
				t.Fatal("retained receipt changed")
			}
			got := nativeCall(t, g, s, "retained-chunk", "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: v.ReceiptID, EntryID: v.Entries[0].ID, MaxBytes: 256}, &a)
			if got.Status != 200 {
				t.Fatalf("retained: %s", got.Body)
			}
		})
	}
}
func TestFeedbackSelectionLimitsAndMissingContent(t *testing.T) {
	selected := &adaptive.ObservationSelection{Mode: "selected", Kinds: []string{"target_output", "oracle_outcome"}}
	s, g, a := feedbackFixture(t, "black-box", selected)
	now := time.Now().UTC()
	s.turns["turn-1"] = Turn{ID: "turn-1", AttemptID: a.AttemptID, Status: "complete", Body: []byte("abcdef"), Started: now.Add(-time.Second), Finished: now}
	s.metadata.Limits.MaxBlobBytes = 4
	_, _ = s.recorder.Append(recording.Draft{Provenance: recording.Application, Channel: recording.Attack, Layer: "test", Kind: "turn_completed", AttemptID: a.AttemptID, TurnID: "turn-1", Semantic: map[string]any{}})
	v := collectFeedback(t, s, g, a, adaptive.ObservationRequest{})
	if len(v.Entries) != 1 || !v.Entries[0].Truncated || v.Entries[0].OriginalSizeBytes != 6 || v.Entries[0].Artifact.SizeBytes != 4 || v.CollectionState != "partial" {
		t.Fatal(v)
	}
	if v.Categories[1].State != "not_requested" || v.Categories[3].State != "withheld" {
		t.Fatal(v.Categories)
	}
	for _, input := range []adaptive.FeedbackReadRequest{{ReceiptID: v.ReceiptID, EntryID: "entry-1", MaxBytes: adaptive.MaxFeedbackChunk + 1}, {ReceiptID: v.ReceiptID, EntryID: "entry-1", Offset: -1, MaxBytes: 1}} {
		if got := nativeCall(t, g, s, "invalid", "observation.content.read", input, &a); got.Status != 400 {
			t.Fatal(got)
		}
	}
	if got := nativeCall(t, g, s, "outside", "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: v.ReceiptID, EntryID: "entry-1", Offset: 5, MaxBytes: 1}, &a); got.Status != 416 {
		t.Fatal(got)
	}
	// Last chunk is EOF even though original target content was truncated.
	got := nativeCall(t, g, s, "end", "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: v.ReceiptID, EntryID: "entry-1", MaxBytes: 4}, &a)
	var chunk adaptive.FeedbackChunk
	_ = json.Unmarshal(got.Body, &chunk)
	if !chunk.EOF || !chunk.Entry.Truncated {
		t.Fatal(chunk)
	}
	s.turns["turn-2"] = Turn{ID: "turn-2", AttemptID: a.AttemptID, Status: "complete"}
	if got := nativeCall(t, g, s, "ambiguous", "observation.read", adaptive.ObservationRequest{}, &a); got.Status != 409 {
		t.Fatal("selected latest invocation")
	}
	// Missing stored content returns an explicit unavailable entry, not an empty success.
	storeRoot := s.paths.Blobs
	if err := os.RemoveAll(storeRoot); err != nil {
		t.Fatal(err)
	}
	got = nativeCall(t, g, s, "missing", "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: v.ReceiptID, EntryID: "entry-1", MaxBytes: 4}, &a)
	_ = json.Unmarshal(got.Body, &chunk)
	if got.Status != 200 || chunk.Entry.Availability != "unavailable" || chunk.EOF {
		t.Fatal(got)
	}
}

func TestFailedInvocationDoesNotFabricateEmptyOutput(t *testing.T) {
	s, g, a := feedbackFixture(t, "diagnostic", nil)
	now := time.Now().UTC()
	exit := 1
	s.turns["turn-1"] = Turn{ID: "turn-1", AttemptID: a.AttemptID, Status: "error", Error: "secret private path", Code: 500, ExitCode: &exit, Started: now.Add(-time.Second), Finished: now}
	if _, err := s.recorder.Append(recording.Draft{Channel: recording.Attack, Layer: "application_bridge", Kind: "turn_completed", Provenance: recording.Application, AttemptID: a.AttemptID, TurnID: "turn-1", Semantic: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	v := collectFeedback(t, s, g, a, adaptive.ObservationRequest{})
	if v.Entries[0].Availability != "unavailable" || v.Entries[0].Artifact != nil || v.Operation.State != "FAILED" || v.Operation.ResponseCode != 500 || v.Operation.ExitCode == nil || *v.Operation.ExitCode != 1 {
		t.Fatal(v)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "private path") {
		t.Fatal("raw error disclosed")
	}
}

func TestFeedbackEntryLimitIsExplicit(t *testing.T) {
	s, g, a := feedbackFixture(t, "diagnostic", nil)
	now := time.Now().UTC()
	s.turns["turn-1"] = Turn{ID: "turn-1", AttemptID: a.AttemptID, Status: "complete", Started: now.Add(-time.Second), Finished: now}
	for idx := 0; idx < adaptive.MaxFeedbackEntries+1; idx++ {
		if _, err := s.recorder.Append(recording.Draft{Channel: recording.Evaluation, Layer: "injection", Kind: "injection_applied", Provenance: recording.HarnessInjected, AttemptID: a.AttemptID, TurnID: "turn-1", InjectionID: fmt.Sprintf("action-%03d", idx), Semantic: map[string]any{"applied": true}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.recorder.Append(recording.Draft{Channel: recording.Attack, Layer: "application_bridge", Kind: "turn_completed", Provenance: recording.Application, AttemptID: a.AttemptID, TurnID: "turn-1", Semantic: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	v := collectFeedback(t, s, g, a, adaptive.ObservationRequest{})
	if len(v.Entries) != adaptive.MaxFeedbackEntries || v.CollectionState != "partial" || v.Categories[2].State != "partial" || v.Categories[2].Reason != "entry_limit" {
		t.Fatal(v)
	}
}
