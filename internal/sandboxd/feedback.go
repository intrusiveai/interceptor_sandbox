package sandboxd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/environment"
	"github.com/intrusive-ai/interceptor-sandbox/internal/evaluation"
	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

func (s *Server) observationViewHandler(w http.ResponseWriter, r *http.Request) {
	a, ok := s.attempts.Get(r.PathValue("attempt"))
	if !ok {
		writeError(w, 404, "not_found", "attempt not found")
		return
	}
	q := adaptive.OperationRequest{SessionID: s.metadata.ID, CampaignID: a.CampaignID, AttemptID: a.AttemptID, AttemptContextDigest: adaptive.DigestAttemptContext(a)}
	input := adaptive.ObservationRequest{TurnID: r.URL.Query().Get("turn_id"), ReceiptID: r.URL.Query().Get("receipt_id")}
	if input.Validate() != nil {
		writeError(w, 400, "invalid_observation_request", "invalid lookup")
		return
	}
	result := s.feedbackView(r.Context(), q, input)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(result.Status)
	_, _ = w.Write(result.Body)
}

func (s *Server) feedbackView(ctx context.Context, r adaptive.OperationRequest, input adaptive.ObservationRequest) adaptive.GatewayResponse {
	if input.ReceiptID != "" {
		return adaptive.RetainedFeedback(s.paths.Root, r, input.ReceiptID)
	}
	// Only receipt publication is serialized; this does not block terminal closure.
	s.feedbackMu.Lock()
	defer s.feedbackMu.Unlock()
	if ctx.Err() != nil {
		return adaptive.OperationError(408, "deadline_exceeded")
	}
	a, ok := s.attempts.Get(r.AttemptID)
	if !ok || a.CampaignID != r.CampaignID || adaptive.DigestAttemptContext(a) != r.AttemptContextDigest {
		return adaptive.OperationError(403, "attempt_binding_mismatch")
	}
	var turn Turn
	count := 0
	s.turnMu.RLock()
	for _, candidate := range s.turns {
		if candidate.AttemptID == r.AttemptID && (input.TurnID == "" || input.TurnID == candidate.ID) {
			turn = candidate
			count++
		}
	}
	s.turnMu.RUnlock()
	if count == 0 {
		return adaptive.OperationError(409, "observation_not_ready")
	}
	if count > 1 {
		return adaptive.OperationError(409, "observation_turn_required")
	}
	id := adaptive.FeedbackReceiptID(r.SessionID, turn.ID)
	if _, err := adaptive.LoadFeedback(s.paths.Root, id); err == nil {
		return adaptive.RetainedFeedback(s.paths.Root, r, id)
	} else if !os.IsNotExist(err) {
		return adaptive.OperationError(503, "feedback_integrity_unavailable")
	}
	if turn.Status != "complete" && turn.Status != "error" {
		return adaptive.OperationError(409, "observation_not_ready")
	}
	s.metadataMu.RLock()
	metadata := s.metadata
	s.metadataMu.RUnlock()
	state, code := "SUCCEEDED", ""
	if turn.Status == "error" {
		state, code = "FAILED", "APPLICATION_OPERATION_FAILED"
	}
	v := adaptive.ObservationView{APIVersion: adaptive.ObservationViewVersion, ReceiptID: id, CampaignID: a.CampaignID, SessionID: metadata.ID, AttemptID: a.AttemptID, AttemptContextDigest: r.AttemptContextDigest, TurnID: turn.ID, FeedbackProfile: metadata.FeedbackProfile, SessionRevision: metadata.Revision, CapturedAt: time.Now().UTC(), WindowStart: turn.Started, WindowEnd: turn.Finished, CollectionState: "complete", Operation: adaptive.OperationView{State: state, ReceiptID: turn.ID, ErrorCode: code, ResponseCode: turn.Code, ExitCode: turn.ExitCode}, Observations: []adaptive.Observation{}, Entries: []adaptive.FeedbackEntry{}, Categories: []adaptive.FeedbackCategory{}}
	for _, kind := range adaptive.FeedbackKinds {
		c := adaptive.FeedbackCategory{Kind: kind, State: "empty", Reason: "no_recorded_observation_not_proof_of_absence"}
		if !adaptive.FeedbackSelected(a.ObservationSelection, kind) {
			c.State, c.Reason = "not_requested", "selection"
		} else if !adaptive.FeedbackPermitted(metadata.FeedbackProfile, kind) {
			c.State, c.Reason = "withheld", "feedback_profile"
		}
		v.Categories = append(v.Categories, c)
	}
	category := func(kind string) *adaptive.FeedbackCategory {
		for idx := range v.Categories {
			if v.Categories[idx].Kind == kind {
				return &v.Categories[idx]
			}
		}
		panic("unknown feedback kind")
	}
	selected := func(kind string) bool {
		c := category(kind)
		return c.State != "withheld" && c.State != "not_requested"
	}
	add := func(kind, source, assurance, media string, content []byte, value any) {
		c := category(kind)
		if !selected(kind) {
			return
		}
		if len(v.Entries) >= adaptive.MaxFeedbackEntries {
			c.State, c.Reason = "partial", "entry_limit"
			v.CollectionState = "partial"
			return
		}
		visibility := adaptive.HarnessVisible
		if kind == "target_output" {
			visibility = adaptive.TargetVisible
		}
		entry := adaptive.FeedbackEntry{ID: fmt.Sprintf("entry-%d", len(v.Entries)+1), Kind: kind, Visibility: visibility, Source: source, Assurance: assurance, Availability: "available", OriginalSizeBytes: int64(len(content))}
		ceiling := min(int64(adaptive.MaxFeedbackArtifact), metadata.Limits.MaxBlobBytes)
		if int64(len(content)) > ceiling {
			content = content[:ceiling]
			entry.Truncated = true
			entry.Reason = "artifact_byte_limit"
			v.CollectionState = "partial"
		}
		if media == "" {
			media = "application/octet-stream"
		}
		object, err := s.blobs.Put(ctx, bytes.NewReader(content))
		if err != nil {
			s.recorder.Fail(err)
			_ = s.evidenceError()
			entry.Availability, entry.Reason = "unavailable", "content_storage_failed"
			v.CollectionState = "partial"
		} else {
			entry.Artifact = &adaptive.ArtifactDescriptor{Digest: object.Digest, SizeBytes: object.Size, MediaType: media, Canonicalization: "raw"}
		}
		v.Entries = append(v.Entries, entry)
		if c.State != "partial" {
			c.State, c.Reason = "available", ""
		}
		if entry.Availability != "available" {
			c.State, c.Reason = "partial", entry.Reason
		} else if entry.Truncated {
			c.State, c.Reason = "partial", entry.Reason
		}
		if value != nil && entry.Availability == "available" && !entry.Truncated {
			v.Observations = append(v.Observations, adaptive.Observation{Kind: kind, Visibility: visibility, Value: value})
		}
	}
	addValue := func(kind string, value any) {
		content, _ := json.Marshal(value)
		add(kind, "interceptor", "recorded-attributed-fact", "application/json", content, value)
	}
	// Empty output is a real zero-byte artifact; it differs from unavailable output.
	if selected("target_output") {
		if turn.Status == "error" && len(turn.Body) == 0 {
			v.Entries = append(v.Entries, adaptive.FeedbackEntry{ID: "entry-1", Kind: "target_output", Visibility: adaptive.TargetVisible, Source: "application", Assurance: "target-response", Availability: "unavailable", Reason: "application_output_unavailable"})
			c := category("target_output")
			c.State, c.Reason = "unavailable", "application_output_unavailable"
			v.CollectionState = "partial"
		} else {
			add("target_output", "application", "target-response", turn.MediaType, turn.Body, nil)
		}
		if turn.Status == "error" && len(turn.Body) > 0 {
			v.Entries[len(v.Entries)-1].Reason = "invocation_failed_capture_may_be_partial"
			c := category("target_output")
			c.State, c.Reason = "partial", "invocation_failed_capture_may_be_partial"
			v.CollectionState = "partial"
		}
		e := v.Entries[len(v.Entries)-1]
		if e.Artifact != nil {
			e.Artifact.MediaType = turn.MediaType
			if e.Artifact.MediaType == "" {
				e.Artifact.MediaType = "application/octet-stream"
			}
			v.TargetOutput = adaptive.TargetOutput{ArtifactDigest: e.Artifact.Digest, MediaType: e.Artifact.MediaType}
		}
	}
	if selected("operation_error") && turn.Status == "error" {
		addValue("operation_error", map[string]any{"code": code, "failed": true})
	}
	var events []recording.Event
	err := s.recorder.Flush()
	if err == nil {
		events, err = recording.VerifyFile(s.paths.Events)
	}
	if err == nil {
		for _, e := range events {
			if e.Kind == "turn_completed" && e.TurnID == turn.ID && e.AttemptID == a.AttemptID {
				v.ThroughEventSeq = e.Seq
				v.WindowEnd = e.ObservedAt
				break
			}
		}
	}
	if err != nil || v.ThroughEventSeq == 0 || s.evidenceError() != nil || turn.EvidencePartial {
		v.CollectionState = "partial"
		for _, kind := range []string{"injection_delivery", "oracle_outcome"} {
			if selected(kind) {
				c := category(kind)
				c.State, c.Reason = "unavailable", "recording_boundary_unavailable"
			}
		}
	} else {
		// Exclude later work; never re-evaluate mutable target state for an old turn.
		attributed := []recording.Event{}
		deliveries := map[string][]map[string]any{}
		for _, e := range events {
			if e.Seq > v.ThroughEventSeq {
				break
			}
			if e.AttemptID != a.AttemptID && e.Attribution.ObservedAttemptID != a.AttemptID {
				continue
			}
			if e.InjectionID != "" && e.AttemptID == a.AttemptID && selected("injection_delivery") {
				if _, ok := deliveries[e.InjectionID]; !ok {
					deliveries[e.InjectionID] = nil
				}
			}
			if e.ObservedAt.Before(v.WindowStart) {
				v.WindowStart = e.ObservedAt
			}
			if e.TurnID != "" && e.TurnID != turn.ID {
				continue
			}
			var semantic map[string]any
			_ = json.Unmarshal(e.Semantic, &semantic)
			if tid, ok := semantic["turn_id"].(string); ok && tid != "" && tid != turn.ID {
				continue
			}
			attributed = append(attributed, e)
			if e.InjectionID != "" && e.AttemptID == a.AttemptID && selected("injection_delivery") {
				disposition := ""
				switch e.Kind {
				case "injection_applied", "injection_delivered":
					disposition = "applied"
					if applied, ok := semantic["applied"].(bool); ok && !applied {
						disposition = "failed"
					}
				case "injection_placement_failed":
					disposition = "failed"
				}
				if disposition != "" {
					scope := "attempt"
					if e.TurnID == turn.ID || semantic["turn_id"] == turn.ID {
						scope = "turn"
					}
					deliveries[e.InjectionID] = append(deliveries[e.InjectionID], map[string]any{"injection_id": e.InjectionID, "state": disposition, "scope": scope, "event_seq": e.Seq})
				}
			}
		}
		ids := []string{}
		for id := range deliveries {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if len(deliveries[id]) == 0 {
				addValue("injection_delivery", map[string]any{"injection_id": id, "state": "unknown", "reason": "no_confirmed_delivery_in_window"})
			} else {
				for _, fact := range deliveries[id] {
					addValue("injection_delivery", fact)
				}
			}
		}
		if selected("oracle_outcome") {
			databaseEvents, databaseErr := databaseFeedbackEvents(events, turn, v.ThroughEventSeq)
			unsupported, unavailable := false, false
			for _, oracle := range s.blueprint.Oracles {
				oracleEvents := attributed
				switch oracle.Kind {
				case "attacker_sink_match", "output_contains", "model_output_contains", "tool_call_match", "event_match":
				case "database_query_match":
					if databaseErr != nil {
						unavailable = true
						continue
					}
					oracleEvents = databaseEvents
				default:
					unsupported = true
					continue
				}
				bp := &environment.Blueprint{Oracles: []environment.Oracle{oracle}}
				report, err := evaluation.Evaluate(evaluation.Input{Blueprint: bp, Events: oracleEvents, Blobs: s.blobs})
				if err != nil {
					unavailable = true
					continue
				}
				for _, o := range report.Oracles {
					if o.Fired {
						addValue("oracle_outcome", map[string]any{"oracle_id": o.ID, "oracle_kind": o.Kind, "fired": true})
					}
				}
			}
			c := category("oracle_outcome")
			if c.State == "empty" {
				c.Reason = "no_positive_attributed_result_not_proof_of_failure"
			}
			if unsupported {
				c.State, c.Reason = "partial", "state_oracles_require_post_run_evidence"
				v.CollectionState = "partial"
			}
			if unavailable {
				if c.State == "empty" {
					c.State = "unavailable"
				} else {
					c.State = "partial"
				}
				c.Reason = "oracle_evaluation_unavailable"
				v.CollectionState = "partial"
			}
		}
	}
	if err := adaptive.SaveFeedback(s.paths.Root, v); err != nil {
		s.recorder.Fail(err)
		_ = s.evidenceError()
		return adaptive.OperationError(503, "feedback_persistence_failed")
	}
	return adaptive.RetainedFeedback(s.paths.Root, r, id)
}
