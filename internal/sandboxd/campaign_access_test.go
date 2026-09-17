package sandboxd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/adaptive"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/injection"
	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

func TestRestoredInjectionCleanupUsesCampaignAccess(t *testing.T) {
	source, _ := nativeOperationServer(t)
	g := nativeGateway(t, source)
	owner := adaptive.OwnerRequest{Action: "bind", SessionID: source.metadata.ID, CampaignID: "campaign", WorkerInstanceID: "worker"}
	if got := nativeCall(t, g, source, "bind", "session.owner", owner, nil); got.Status != 200 {
		t.Fatal(got)
	}
	payload := []byte(`"payload"`)
	descriptor := adaptive.ArtifactDescriptor{Digest: digest.Bytes(payload), SizeBytes: int64(len(payload)), MediaType: "application/json", Canonicalization: "raw"}
	if got := nativeCall(t, g, source, "artifact", "artifact.register", artifactRegistration{Descriptor: descriptor, Content: payload}, nil); got.Status != 201 {
		t.Fatal(got)
	}
	attempt := adaptive.AttemptContext{APIVersion: adaptive.AttemptContextVersion, CampaignID: "campaign", ThreadID: "thread", AttemptID: "attempt", AttemptIndex: 1, Generation: 1, Payload: descriptor, Generator: adaptive.Generator{Kind: "custom-harness"}, FeedbackProfile: "black-box", CreatedAt: time.Now().UTC()}
	if got := nativeCall(t, g, source, "attempt", "attempt.register", attempt, &attempt); got.Status != 201 {
		t.Fatal(got)
	}
	definition := injection.Definition{ID: "restored", Surface: "service_response", Mode: "simulation", Scope: "standing", Enabled: true, Selector: injection.Selector{Service: "c2", Path: "/collect", CallOrdinal: "first"}, Placement: injection.Placement{Operation: "replace"}, Payload: json.RawMessage(payload), AttemptID: attempt.AttemptID, PayloadDigest: descriptor.Digest}
	if got := nativeCall(t, g, source, "arm", "injection.arm", injectionArm{Definition: definition}, &attempt); got.Status != 201 {
		t.Fatal(got)
	}
	tx, err := statejournal.VerifyFile(source.paths.Journal)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := nativeOperationServer(t, "unix", "sess-2-000000000000000000000000")
	// Reproduce restore's journal replay into a fresh session/execution store.
	for _, transaction := range tx {
		for _, operation := range transaction.Operations {
			if operation.Kind == "injection_put" || operation.Kind == "injection_delete" {
				if _, err = target.journal.Append("restore", []statejournal.Operation{operation}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err = target.replayManagedState(); err != nil {
		t.Fatal(err)
	}
	if len(target.injections.ListPublic()) != 1 || len(target.execution.State().Records) != 0 {
		t.Fatal("fixture must have restored injection and no original arm record")
	}
	g = nativeGateway(t, target)
	owner.SessionID = target.metadata.ID
	if got := nativeCallAs(t, g, target, "bind", "session.owner", owner, nil, "campaign", "second-worker", 2); got.Status != 200 {
		t.Fatal(got)
	}
	if got := nativeCallAs(t, g, target, "wrong", "injection.delete", injectionLookup{InjectionID: definition.ID}, nil, "other-campaign", "third-worker", 3); got.Status != 403 {
		t.Fatal("cross-campaign cleanup allowed")
	}
	if len(target.injections.ListPublic()) != 1 {
		t.Fatal("rejection removed injection")
	}
	owner.Action = "close_execution"
	if got := nativeCallAs(t, g, target, "close", "session.owner", owner, nil, "campaign", "third-worker", 3); got.Status != 200 {
		t.Fatal(got)
	}
	if got := nativeCallAs(t, g, target, "delete", "injection.delete", injectionLookup{InjectionID: definition.ID}, nil, "campaign", "fourth-worker", 4); got.Status != 204 {
		t.Fatalf("restored cleanup rejected: %s", got.Body)
	}
	if len(target.injections.ListPublic()) != 0 {
		t.Fatal("injection survived cleanup")
	}
	record, ok := target.execution.Lookup("delete")
	if !ok || record.Request.WorkerInstanceID != "fourth-worker" || record.Request.RunRevision != 4 {
		t.Fatal("cleanup attribution lost")
	}
}
