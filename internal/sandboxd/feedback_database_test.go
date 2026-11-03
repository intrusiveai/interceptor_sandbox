package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evaluation"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

func databaseOracleDraft(turn, attempt, source string, matched bool) recording.Draft {
	return recording.Draft{Channel: recording.Evaluation, Layer: "database", Kind: "database_oracle", Provenance: recording.SandboxControl,
		TurnID: turn, Attribution: recording.Attribution{Window: "observed", ObservedAttemptID: attempt},
		Semantic: map[string]any{"service": "database", "detail": map[string]any{"oracle": "refund", "source": source, "matched": matched, "result": [][]int{{1}}, "query": "private query"}}}
}

func appendFeedbackEvent(t *testing.T, s *Server, draft recording.Draft) {
	t.Helper()
	if _, err := s.recorder.Append(draft); err != nil {
		t.Fatal(err)
	}
}

func completeDatabaseTurn(t *testing.T, s *Server, a adaptive.AttemptContext) Turn {
	t.Helper()
	turn := Turn{ID: "turn-db", AttemptID: a.AttemptID, Status: "complete", Started: time.Now().Add(-time.Second), Finished: time.Now()}
	s.turns[turn.ID] = turn
	appendFeedbackEvent(t, s, recording.Draft{Channel: recording.Attack, Layer: "application_bridge", Kind: "turn_completed", Provenance: recording.Application, TurnID: turn.ID, AttemptID: a.AttemptID, Semantic: map[string]any{}})
	return turn
}

func oracleFeedbackCategory(t *testing.T, view adaptive.ObservationView) adaptive.FeedbackCategory {
	t.Helper()
	for _, category := range view.Categories {
		if category.Kind == "oracle_outcome" {
			return category
		}
	}
	t.Fatal("missing oracle category")
	return adaptive.FeedbackCategory{}
}

func TestDatabaseFeedbackBoundariesAndCompleteness(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    string
		positive bool
	}{
		{"committed", "available", true}, {"nonmatching", "empty", false},
		{"missing", "unavailable", false}, {"missing_matched", "unavailable", false},
		{"missing_result", "unavailable", false}, {"null_result", "unavailable", false},
		{"missing_source", "unavailable", false}, {"corrupt_recording", "unavailable", false},
		{"baseline_only", "unavailable", false}, {"harness_only", "unavailable", false},
		{"seeded_match", "empty", false}, {"harness_match", "empty", false},
		{"earlier_turn", "empty", false}, {"other_attempt", "empty", false},
		{"later_turn", "empty", false}, {"after_boundary", "empty", false},
		{"late_only", "unavailable", false}, {"pending_capture", "unavailable", false},
		{"ambiguous", "unavailable", false}, {"outside_turn", "unavailable", false},
		{"uncertain_execution", "unavailable", false}, {"missing_observation", "unavailable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, g, a := feedbackFixture(t, "oracle-assisted", nil)
			s.blueprint.Oracles = []environment.Oracle{{ID: "refund", Kind: "database_query_match", Service: "database"}}
			draft := databaseOracleDraft("turn-db", a.AttemptID, "application", true)
			appendNonmatch := func() { appendFeedbackEvent(t, s, databaseOracleDraft("turn-db", a.AttemptID, "application", false)) }
			switch tc.name {
			case "missing", "late_only":
			case "nonmatching":
				appendNonmatch()
			case "baseline_only", "harness_only", "seeded_match", "harness_match":
				source := "baseline"
				if strings.HasPrefix(tc.name, "harness") {
					source = "harness"
				}
				appendFeedbackEvent(t, s, databaseOracleDraft("turn-db", a.AttemptID, source, true))
				if strings.HasSuffix(tc.name, "match") {
					appendNonmatch()
				}
			case "earlier_turn", "later_turn", "other_attempt":
				if tc.name == "other_attempt" {
					draft.Attribution.ObservedAttemptID = "other"
				} else {
					draft.TurnID = tc.name
				}
				appendFeedbackEvent(t, s, draft)
				appendNonmatch()
			case "after_boundary":
				appendNonmatch()
			case "missing_matched", "missing_result", "null_result", "missing_source":
				detail := draft.Semantic.(map[string]any)["detail"].(map[string]any)
				if tc.name == "null_result" {
					detail["result"] = nil
				} else {
					delete(detail, strings.TrimPrefix(tc.name, "missing_"))
				}
				appendFeedbackEvent(t, s, draft)
			case "ambiguous", "outside_turn", "uncertain_execution", "missing_observation":
				draft.Attribution = recording.Attribution{Window: tc.name}
				if tc.name != "missing_observation" {
					draft.TurnID = ""
				}
				appendFeedbackEvent(t, s, draft)
				appendNonmatch() // Incomplete ownership must not look like a complete empty result.
			default:
				appendFeedbackEvent(t, s, draft)
			}
			turn := completeDatabaseTurn(t, s, a)
			if tc.name == "pending_capture" {
				turn.EvidencePartial = true
				s.turns[turn.ID] = turn
			}
			if tc.name == "after_boundary" || tc.name == "late_only" {
				appendFeedbackEvent(t, s, draft)
			}
			if tc.name == "corrupt_recording" {
				if err := s.recorder.Flush(); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(s.paths.Events, []byte("corrupt\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			view := collectFeedback(t, s, g, a, adaptive.ObservationRequest{TurnID: turn.ID})
			category := oracleFeedbackCategory(t, view)
			if category.State != tc.state || category.Reason == "state_oracles_require_post_run_evidence" {
				t.Fatalf("category %+v; want %s", category, tc.state)
			}
			if (view.CollectionState == "partial") != (tc.state == "unavailable") {
				t.Fatalf("collection: %+v", view)
			}
			positive := false
			for _, observation := range view.Observations {
				positive = positive || observation.Kind == "oracle_outcome"
			}
			if positive != tc.positive {
				t.Fatalf("observations %+v", view.Observations)
			}
		})
	}
}

func TestDatabaseFeedbackProfilesAndIndependentOracles(t *testing.T) {
	for _, name := range []string{"black-box", "diagnostic", "unrequested", "mixed", "missing_oracle"} {
		t.Run(name, func(t *testing.T) {
			profile := name
			if name != "black-box" && name != "diagnostic" {
				profile = "oracle-assisted"
			}
			var selection *adaptive.ObservationSelection
			if name == "unrequested" {
				selection = &adaptive.ObservationSelection{Mode: "selected", Kinds: []string{"target_output"}}
			}
			s, g, a := feedbackFixture(t, profile, selection)
			s.blueprint.Oracles = []environment.Oracle{{ID: "refund", Kind: "database_query_match", Service: "database"}}
			if name == "mixed" {
				s.blueprint.Oracles = append(s.blueprint.Oracles, environment.Oracle{ID: "state", Kind: "service_state_match"})
			}
			if name == "missing_oracle" {
				s.blueprint.Oracles = append(s.blueprint.Oracles, environment.Oracle{ID: "missing", Kind: "database_query_match", Service: "database"})
			}
			appendFeedbackEvent(t, s, databaseOracleDraft("turn-db", a.AttemptID, "application", true))
			turn := completeDatabaseTurn(t, s, a)
			view := collectFeedback(t, s, g, a, adaptive.ObservationRequest{TurnID: turn.ID})
			category := oracleFeedbackCategory(t, view)
			want := "withheld"
			if name == "unrequested" {
				want = "not_requested"
			}
			if name == "mixed" || name == "missing_oracle" {
				want = "partial"
			}
			if category.State != want {
				t.Fatal(category)
			}
			count := 0
			for _, entry := range view.Entries {
				if entry.Kind != "oracle_outcome" {
					continue
				}
				count++
				got := nativeCall(t, g, s, "oracle-bytes", "observation.content.read", adaptive.FeedbackReadRequest{ReceiptID: view.ReceiptID, EntryID: entry.ID, MaxBytes: 1024}, &a)
				var chunk adaptive.FeedbackChunk
				if got.Status != 200 || json.Unmarshal(got.Body, &chunk) != nil {
					t.Fatal(got)
				}
				if string(chunk.Content) != `{"fired":true,"oracle_id":"refund","oracle_kind":"database_query_match"}` {
					t.Fatalf("private oracle details escaped: %s", chunk.Content)
				}
			}
			if (count == 1) != (want == "partial") {
				t.Fatalf("entries: %+v", view.Entries)
			}
		})
	}
}

func TestDatabaseFeedbackIgnoresLatchedStateAndRetainsReceipt(t *testing.T) {
	s, g, a := feedbackFixture(t, "oracle-assisted", nil)
	s.blueprint.Oracles = []environment.Oracle{{ID: "refund", Kind: "database_query_match", Service: "database"}}
	// Restored cumulative results must not substitute for this turn's observation.
	s.oracleState = map[string]evaluation.OracleResult{"refund": {ID: "refund", Kind: "database_query_match", Fired: true}}
	appendFeedbackEvent(t, s, databaseOracleDraft("turn-db", a.AttemptID, "application", false))
	turn := completeDatabaseTurn(t, s, a)
	view := collectFeedback(t, s, g, a, adaptive.ObservationRequest{TurnID: turn.ID})
	if oracleFeedbackCategory(t, view).State != "empty" {
		t.Fatal(view)
	}
	appendFeedbackEvent(t, s, databaseOracleDraft("turn-db", a.AttemptID, "application", true))
	delete(s.oracleState, "refund")
	again := collectFeedback(t, s, g, a, adaptive.ObservationRequest{ReceiptID: view.ReceiptID})
	if again.Hash != view.Hash {
		t.Fatal("receipt changed with later database state")
	}
}

func TestDatabaseFeedbackStorageFailureBlocksNewWork(t *testing.T) {
	for _, stage := range []string{"capture", "feedback_blob", "feedback_receipt"} {
		t.Run(stage, func(t *testing.T) {
			s, _, a := feedbackFixture(t, "oracle-assisted", nil)
			s.blueprint.Oracles = []environment.Oracle{{ID: "refund", Kind: "database_query_match", Service: "database"}}
			appendFeedbackEvent(t, s, databaseOracleDraft("turn-db", a.AttemptID, "application", true))
			turn := completeDatabaseTurn(t, s, a)
			switch stage {
			case "capture":
				s.recorder.Fail(errors.New("required oracle capture lost"))
			case "feedback_blob":
				if err := os.Rename(s.paths.Blobs, s.paths.Blobs+"-unavailable"); err != nil {
					t.Fatal(err)
				}
			case "feedback_receipt":
				if err := os.Mkdir(s.paths.Root+"/feedback", 0500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(s.paths.Root+"/feedback", 0700) })
			}
			result := s.feedbackView(context.Background(), adaptive.OperationRequest{SessionID: s.metadata.ID, CampaignID: a.CampaignID, AttemptID: a.AttemptID, AttemptContextDigest: adaptive.DigestAttemptContext(a)}, adaptive.ObservationRequest{TurnID: turn.ID})
			if stage == "feedback_receipt" {
				if result.Status != 503 {
					t.Fatal(result)
				}
			} else {
				var view adaptive.ObservationView
				if result.Status != 200 || json.Unmarshal(result.Body, &view) != nil || view.CollectionState != "partial" {
					t.Fatal(result)
				}
				if oracleFeedbackCategory(t, view).State == "available" {
					t.Fatal(view)
				}
			}
			if s.recorder.Err() == nil {
				t.Fatal("failure not latched")
			}
			if _, err := s.beginInvocation(context.Background(), applicationInvocation{TurnID: "next"}); err == nil {
				t.Fatal("new work admitted")
			}
		})
	}
}
