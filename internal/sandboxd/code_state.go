package sandboxd

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (a *protocolApplicationAdapter) stateEnabled() bool {
	c := a.server.blueprint.Application.Interface.Code
	return a.Kind() == "code" && c != nil && c.State != nil
}

// The same gate covers direct requests, control requests, export and cleanup.
// The snapshot caller holds it through filesystem capture, not just serialization.
type stateInvocationKey struct{}

func (a *protocolApplicationAdapter) lockState(ctx context.Context) (func(), error) {
	if ctx.Value(stateInvocationKey{}) == a {
		return func() {}, nil
	}
	if !a.stateEnabled() {
		return func() {}, nil
	}
	select {
	case a.stateGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-a.stateGate
			return nil, err
		}
		return func() { <-a.stateGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *protocolApplicationAdapter) saveState(ctx context.Context) error {
	id := fmt.Sprintf("state-%d", a.server.nextInvocation.Add(1))
	ch := make(chan protocolFrame, 1)
	a.pendingMu.Lock()
	if a.failed != nil {
		err := a.failed
		a.pendingMu.Unlock()
		return err
	}
	a.pending[id] = ch
	a.pendingMu.Unlock()
	defer func() { a.pendingMu.Lock(); delete(a.pending, id); a.pendingMu.Unlock() }()
	if err := a.writeInvocation(ctx, protocolFrame{Protocol: applicationProtocol, Type: "save_state", ID: id}); err != nil {
		a.failSession(err)
		return err
	}
	select {
	case <-ctx.Done():
		a.failSession(ctx.Err())
		return ctx.Err()
	case frame := <-ch:
		if frame.Error != "" {
			return fmt.Errorf("save application state: %s", frame.Error)
		}
		if frame.Type != "state_saved" {
			a.failSession(errors.New("invalid state acknowledgement"))
			return errors.New("invalid state acknowledgement")
		}
		return nil
	}
}

// Unlike a blocking mutex acquisition, lifecycle waiting honors cancellation.
func lockContext(ctx context.Context, try func() bool) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if try() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
