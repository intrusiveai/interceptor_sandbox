package gatewayhost

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
)

func TestValidateTypedBodyBindsAttemptToLease(t *testing.T) {
	attempt := adaptive.AttemptContext{APIVersion: adaptive.AttemptContextVersion, CampaignID: "other-campaign", ThreadID: "thread-1", AttemptID: "attempt-1", Generation: 1, AttemptIndex: 1, FeedbackProfile: "black-box", CreatedAt: time.Now().UTC()}
	body, _ := json.Marshal(attempt)
	request := adaptive.CrossVMRequest{Operation: "attempt.register", AttemptID: attempt.AttemptID, AttemptContextDigest: adaptive.DigestAttemptContext(attempt)}
	if err := validateTypedBody(adaptive.ExecutionLease{CampaignID: "campaign-1"}, request, body); err == nil {
		t.Fatal("expected cross-campaign attempt to be rejected")
	}
	attempt.CampaignID = "campaign-1"
	body, _ = json.Marshal(attempt)
	request.AttemptContextDigest = adaptive.DigestAttemptContext(attempt)
	if err := validateTypedBody(adaptive.ExecutionLease{CampaignID: "campaign-1"}, request, body); err != nil {
		t.Fatalf("bound attempt was rejected: %v", err)
	}
}

func TestValidateTypedBodyRejectsCallerSnapshotLimit(t *testing.T) {
	err := validateTypedBody(adaptive.ExecutionLease{}, adaptive.CrossVMRequest{Operation: "snapshot.create"}, []byte(`{"label":"safe","maximum_committed_bytes":999999}`))
	if err == nil {
		t.Fatal("guest-controlled snapshot limit must be rejected")
	}
}

func TestValidateTypedBodyAcceptsBoundedInjectionDeletion(t *testing.T) {
	request := adaptive.CrossVMRequest{Operation: "injection.delete"}
	if err := validateTypedBody(adaptive.ExecutionLease{}, request, []byte(`{"injection_id":"c2-command-1"}`)); err != nil {
		t.Fatalf("valid injection deletion was rejected: %v", err)
	}
	if err := validateTypedBody(adaptive.ExecutionLease{}, request, []byte(`{"injection_id":"../escape"}`)); err == nil {
		t.Fatal("unsafe injection deletion ID was accepted")
	}
}
