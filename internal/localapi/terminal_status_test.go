package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/intrusive-ai/interceptor-sandbox/internal/hostrun"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
	"github.com/intrusive-ai/interceptor-sandbox/internal/testdocker"
)

func statusState(t *testing.T, s *Service) struct {
	Phase          string
	Closed         bool
	Failure        *ExecutionFailure
	StoreAvailable bool `json:"store_available"`
} {
	t.Helper()
	var state struct {
		Phase          string
		Closed         bool
		Failure        *ExecutionFailure
		StoreAvailable bool `json:"store_available"`
	}
	got := post(t, s, "/v1/status", map[string]string{"api_version": Version, "campaign_id": "campaign"})
	if got.Status != 200 {
		t.Fatal(got)
	}
	if err := json.Unmarshal(got.Body, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestTargetEndPersistsCauseAndClosesAdmission(t *testing.T) {
	for _, mode := range []string{"native-limit", "planned-but-failed", "connector-return", "unexpected-stop"} {
		t.Run(mode, func(t *testing.T) {
			s, f := fixture(t, true)
			run := &nativeRun{sessionID: "session-1", metadata: session.Metadata{ID: "session-1", Phase: session.Error, TerminationReason: "WALL_TIME_LIMIT"}}
			reason := "WALL_TIME_LIMIT"
			switch mode {
			case "planned-but-failed":
				run.plannedStop.Store(true)
			case "connector-return":
				run.metadata.Phase = session.Running
				run.metadata.TerminationReason = ""
				run.err = errors.New("MODEL_CONNECTOR_FAILED: disconnected")
				reason = run.err.Error()
			case "unexpected-stop":
				run.metadata.Phase = session.Stopped
				run.metadata.TerminationReason = ""
				reason = "CONTAINER_INTERRUPTED"
			}
			n := &NativeRuntime{ctx: context.Background(), active: run, service: s}
			n.targetEnded(run)
			state := statusState(t, s)
			if state.Phase != "error" || !state.Closed || state.Failure == nil || state.Failure.Reason != reason || state.Failure.SessionID != "session-1" || state.Failure.ObservedAt.IsZero() {
				t.Fatalf("stale status: %+v", state)
			}
			data, err := os.ReadFile(s.path)
			if err != nil {
				t.Fatal(err)
			}
			var saved State
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Failure == nil || saved.Failure.Reason != reason || !saved.Closed || saved.Phase != "error" {
				t.Fatalf("not persisted: %s", data)
			}
			// Duplicate cleanup reports preserve the original diagnostic and timestamp.
			n.targetFailed(run, "session-1", errors.New("secondary cleanup failure"))
			again := statusState(t, s)
			if *again.Failure != *state.Failure {
				t.Fatal("original cause overwritten")
			}
			request := LifecycleRequest{APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", OperationID: "after-failure", Operation: "snapshot.restore", CheckpointID: "checkpoint", WorkerInstanceID: "worker", RunRevision: 1}
			if got := post(t, s, "/v1/lifecycle", request); got.Status != 409 || f.restores != 0 {
				t.Fatal(got)
			}
			request.Operation = "session.stop"
			request.OperationID = "cleanup-after-failure"
			request.CheckpointID = ""
			if got := post(t, s, "/v1/lifecycle", request); got.Status != 200 {
				t.Fatal(got)
			}
			afterStop := statusState(t, s)
			if afterStop.Phase != "error" || !afterStop.Closed || *afterStop.Failure != *state.Failure {
				t.Fatal("stop erased failure", afterStop)
			}
		})
	}
}

func TestPlannedEndAndDelayedOldCallbackDoNotCloseReplacement(t *testing.T) {
	s, _ := fixture(t, true)
	old := &nativeRun{sessionID: "session-1", metadata: session.Metadata{ID: "session-1", Phase: session.Stopped}}
	old.plannedStop.Store(true)
	n := &NativeRuntime{ctx: context.Background(), active: old, service: s}
	n.targetEnded(old)
	if statusState(t, s).Closed {
		t.Fatal("planned stop treated as failure")
	}
	next := &nativeRun{sessionID: "session-2"}
	n.active = next
	n.targetFailed(old, "session-1", errors.New("late old callback"))
	if statusState(t, s).Closed {
		t.Fatal("old target closed replacement")
	}
	// Replacement failures count before the API has published its new binding.
	n.targetFailed(next, "session-2", errors.New("replacement failed"))
	state := statusState(t, s)
	if !state.Closed || state.Failure.SessionID != "session-2" {
		t.Fatal(state)
	}
}

func TestTargetFailureBypassesGateAndSurvivesLedgerFailure(t *testing.T) {
	s, _ := fixture(t, true)
	s.path = filepath.Join(s.path, "unwritable-child")
	if !s.lockGate(t.Context()) {
		t.Fatal("gate unavailable")
	}
	defer s.unlockGate()
	done := make(chan error, 1)
	go func() {
		done <- s.failExecution(ExecutionFailure{SessionID: "session-1", Reason: "WALL_TIME_LIMIT", ObservedAt: time.Now().UTC()})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected ledger error")
		}
	case <-time.After(time.Second):
		t.Fatal("notification waited for mutation gate")
	}
	state := statusState(t, s)
	if !state.Closed || state.Phase != "error" || state.StoreAvailable || state.Failure.Reason != "WALL_TIME_LIMIT" {
		t.Fatal(state)
	}
}

type failingPreflight struct {
	*fakeRuntime
	service *Service
}

func (f *failingPreflight) ValidateRestore(context.Context, string, string) error {
	_ = f.service.failExecution(ExecutionFailure{SessionID: "session-1", Reason: "WALL_TIME_LIMIT", ObservedAt: time.Now().UTC()})
	return errors.New("checkpoint unavailable")
}
func TestFailureDuringRejectedPreflightCannotPublishReady(t *testing.T) {
	s, f := fixture(t, true)
	s.runtime = &failingPreflight{f, s}
	q := LifecycleRequest{APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", OperationID: "restore", Operation: "snapshot.restore", CheckpointID: "checkpoint", WorkerInstanceID: "worker", RunRevision: 1}
	if got := post(t, s, "/v1/lifecycle", q); got.Status != 503 {
		t.Fatal(got)
	}
	state := statusState(t, s)
	if state.Phase != "error" || !state.Closed || f.restores != 0 {
		t.Fatal(state)
	}
}

func TestReplacementFailureBeforeBindingCannotPublishReady(t *testing.T) {
	s, f := fixture(t, true)
	f.enteredBind = make(chan struct{})
	f.releaseBind = make(chan struct{})
	q := LifecycleRequest{APIVersion: Version, CampaignID: "campaign", SessionID: "session-1", OperationID: "restore", Operation: "snapshot.restore", CheckpointID: "checkpoint", WorkerInstanceID: "worker", RunRevision: 1}
	done := make(chan int, 1)
	go func() { done <- post(t, s, "/v1/lifecycle", q).Status }()
	select {
	case <-f.enteredBind:
	case <-time.After(3 * time.Second):
		t.Fatal("binding not reached")
	}
	run := &nativeRun{sessionID: "session-2"}
	n := &NativeRuntime{ctx: context.Background(), active: run, service: s}
	n.targetFailed(run, "session-2", errors.New("WALL_TIME_LIMIT"))
	close(f.releaseBind)
	select {
	case status := <-done:
		if status != 503 {
			t.Fatal(status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restore stuck")
	}
	state := statusState(t, s)
	if state.Phase != "error" || !state.Closed || state.Failure.SessionID != "session-2" {
		t.Fatal(state)
	}
}

func TestStopConfirmationCannotHideNativeOrCleanupFailure(t *testing.T) {
	for _, mode := range []string{"unfinalized", "native-error", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := fixture(t, true)
			root := t.TempDir()
			docker, fake := testdocker.New(t)
			id := "sess-1-000000000000000000000000"
			m := session.Metadata{SchemaVersion: 1, ID: id, Phase: session.Running, ContainerID: testdocker.Retired(t, root, id)}
			if mode == "native-error" {
				m.Phase = session.Error
				m.TerminationReason = "WALL_TIME_LIMIT"
			}
			if err := session.Save(session.PathsFor(root, id).Metadata, m); err != nil {
				t.Fatal(err)
			}
			if mode == "unavailable" {
				if err := os.WriteFile(filepath.Join(fake, "unavailable"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			run := &nativeRun{sessionID: id, metadata: m}
			run.plannedStop.Store(true)
			n := &NativeRuntime{ctx: context.Background(), root: root, config: hostrun.Config{Docker: docker}, active: run, service: s}
			err := n.confirmStopped(t.Context(), id, run)
			if mode != "native-error" && err == nil {
				t.Fatal("cleanup uncertainty hidden")
			}
			if mode == "native-error" && err != nil {
				t.Fatal("confirmed cleanup should succeed", err)
			}
			state := statusState(t, s)
			if state.Phase != "error" || !state.Closed || state.Failure == nil || state.Failure.SessionID != id {
				t.Fatal(state)
			}
			if mode == "native-error" && state.Failure.Reason != "WALL_TIME_LIMIT" {
				t.Fatal(state)
			}
		})
	}
}
