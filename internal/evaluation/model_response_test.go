package evaluation

import (
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"testing"
)

func TestModelResponseOutcomeDoesNotClaimToolExecution(t *testing.T) {
	for _, tc := range []struct {
		delivery string
		causal   bool
		want     string
	}{{"response_written", false, "RESPONSE_WRITTEN"}, {"write_failed", false, "DELIVERY_UNKNOWN"}, {"response_written", true, "DELIVERED_SUCCEEDED"}, {"not_confirmed", false, "NOT_DELIVERED"}} {
		oracles := []OracleResult{{Fired: tc.causal, CausalIDs: []string{"response"}}}
		got := injectionResults(oracles, []injection.OperatorState{{Definition: injection.Definition{ID: "response", Surface: "model_response", Mode: "simulation"}, Evaluation: injection.EvaluationState{Delivery: tc.delivery, DeliveryObservable: true}}})
		if got[0].Outcome != tc.want {
			t.Fatal(tc, got)
		}
	}
}
