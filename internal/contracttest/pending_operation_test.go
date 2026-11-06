package contracttest

import (
	"encoding/json"
	"testing"
)

func TestPendingOperationResponseIsLimitedToRunningRecords(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"api_version": "interceptor.dev/operation-request/v1alpha2", "request_id": "r", "operation_id": "o", "operation": "artifact.register", "session_id": "s", "campaign_id": "c", "worker_instance_id": "w", "run_revision": 1, "body_digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "deadline": "2026-01-01T00:00:00Z"}
	for _, tc := range []struct {
		name, state, response string
		valid                 bool
	}{
		{"running placeholder", "running", `{"status":0,"session_revision":0}`, true},
		{"completed placeholder", "completed", `{"status":0,"session_revision":0}`, false},
		{"unknown placeholder", "unknown", `{"status":0,"session_revision":0}`, false},
		{"other state placeholder", "other", `{"status":0,"session_revision":0}`, false},
		{"missing status", "running", `{"session_revision":0}`, false},
		{"missing revision", "running", `{"status":0}`, false},
		{"nonzero revision", "running", `{"status":0,"session_revision":1}`, false},
		{"unexpected body", "running", `{"status":0,"session_revision":0,"body":null}`, false},
		{"unexpected field", "running", `{"status":0,"session_revision":0,"extra":true}`, false},
		{"missing finished response body", "completed", `{"status":200,"session_revision":1}`, false},
		{"completed rejection", "completed", `{"status":409,"body":{"code":"operation_conflict"},"session_revision":1}`, true},
		{"unknown outcome", "unknown", `{"status":503,"body":{"code":"operation_outcome_unknown"},"session_revision":1}`, true},
		{"running explicit in-progress", "running", `{"status":202,"body":{"code":"operation_in_progress"},"session_revision":1}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := map[string]any{"request": request, "fingerprint": "fingerprint", "state": tc.state, "response": json.RawMessage(tc.response), "admitted_at": "2026-01-01T00:00:00Z"}
			wrapped := map[string]any{"status": 200, "body": record, "session_revision": 1}
			raw, err := json.Marshal(wrapped)
			if err != nil {
				t.Fatal(err)
			}
			err = v.Validate("#/components/schemas/OperationStatusResponse", raw)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
	placeholder := []byte(`{"status":0,"session_revision":0}`)
	for _, name := range []string{"adaptive.GatewayResponse", "OperationStatusResponse", "InProgressResponse", "LifecycleResponse"} {
		if v.Validate("#/components/schemas/"+name, placeholder) == nil {
			t.Errorf("%s accepted status zero as an ordinary response", name)
		}
	}
	lifecycle := map[string]any{"request": map[string]any{"api_version": "interceptor.dev/local-lifecycle/v1alpha1", "campaign_id": "c", "worker_instance_id": "w", "run_revision": 1, "session_id": "s", "operation_id": "o", "operation": "session.stop"}, "fingerprint": "f", "state": "running", "response": json.RawMessage(placeholder)}
	raw, _ := json.Marshal(lifecycle)
	if v.Validate("#/components/schemas/localapi.Record", raw) == nil {
		t.Fatal("lifecycle record accepted native pending placeholder")
	}
}
