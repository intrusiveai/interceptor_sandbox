package sandboxd

import (
	"encoding/json"
	"errors"

	"github.com/intrusive-ai/interceptor-sandbox/internal/recording"
)

// Database outcomes describe committed state observed during this turn, not
// causal effects of its injection. Never use attempt-wide or inherited results.
func databaseFeedbackEvents(events []recording.Event, turn Turn, through uint64) ([]recording.Event, error) {
	var observations []recording.Event
	for _, event := range events {
		if event.Seq > through {
			break
		}
		if event.Kind != "database_oracle" {
			continue
		}
		if event.TurnID != "" && event.TurnID != turn.ID ||
			event.Attribution.ObservedAttemptID != "" && event.Attribution.ObservedAttemptID != turn.AttemptID {
			continue
		}
		if event.ObservedAt.Before(turn.Started) {
			continue
		}
		var value struct {
			Detail struct {
				Source string `json:"source"`
			} `json:"detail"`
		}
		if err := json.Unmarshal(event.Semantic, &value); err != nil {
			return nil, err
		}
		if value.Detail.Source == "baseline" || value.Detail.Source == "harness" {
			continue
		}
		if value.Detail.Source != "application" || event.TurnID != turn.ID ||
			event.Attribution.Window != "observed" || event.Attribution.ObservedAttemptID != turn.AttemptID {
			return nil, errors.New("database oracle observation has no unambiguous turn binding")
		}
		observations = append(observations, event)
	}
	return observations, nil
}
