package localapi

import (
	"context"
	"testing"

	"github.com/intrusive-ai/interceptor-sandbox/internal/hostlifetime"
	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
)

type lifetimeRuntime struct {
	*fakeRuntime
	check    func() error
	held     bool
	acquired int
	t        *testing.T
}

func (r *lifetimeRuntime) CheckHost() error {
	if r.check != nil {
		return r.check()
	}
	return nil
}
func (r *lifetimeRuntime) HoldRestore(context.Context) (func(), error) {
	r.held = true
	r.acquired++
	return func() { r.held = false }, nil
}
func (r *lifetimeRuntime) Stop(ctx context.Context, id string) error {
	if !r.held {
		r.t.Error("source stopped without restore lease")
	}
	return r.fakeRuntime.Stop(ctx, id)
}
func (r *lifetimeRuntime) Restore(ctx context.Context, source, id string) (session.Metadata, error) {
	if !r.held {
		r.t.Error("replacement started without restore lease")
	}
	return r.fakeRuntime.Restore(ctx, source, id)
}
func TestHostCheckClosesBeforeRestoreAdmission(t *testing.T) {
	s, f := fixture(t, true)
	checks := 0
	s.runtime = &lifetimeRuntime{fakeRuntime: f, t: t, check: func() error {
		checks++
		_ = s.failTransport()
		return hostlifetime.Interrupted("HOST_SLEEP_INTERRUPTED")
	}}
	request := LifecycleRequest{APIVersion: Version, CampaignID: "campaign", SessionID: f.active, OperationID: "after-wake", Operation: "snapshot.restore", CheckpointID: "checkpoint", WorkerInstanceID: "worker", RunRevision: 1}
	if got := post(t, s, "/v1/lifecycle", request); got.Status != 409 {
		t.Fatal(got)
	}
	if checks != 1 || f.restores != 0 {
		t.Fatal("admitted restore before power check", checks, f.restores)
	}
	assertCampaignClosed(t, s)
}
func TestRestoreHoldsActivityAcrossSourceAndReplacement(t *testing.T) {
	s, f := fixture(t, true)
	runtime := &lifetimeRuntime{fakeRuntime: f, t: t}
	s.runtime = runtime
	request := LifecycleRequest{APIVersion: Version, CampaignID: "campaign", SessionID: f.active, OperationID: "continuous-assertion", Operation: "snapshot.restore", CheckpointID: "checkpoint", WorkerInstanceID: "worker", RunRevision: 1}
	if got := post(t, s, "/v1/lifecycle", request); got.Status != 200 {
		t.Fatal(got)
	}
	if runtime.held || runtime.acquired != 1 || f.restores != 1 {
		t.Fatal("restore lease lifetime incorrect")
	}
}
