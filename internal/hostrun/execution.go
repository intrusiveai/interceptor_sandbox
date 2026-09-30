package hostrun

import (
	"context"
	"errors"

	"github.com/intrusive-ai/interceptor-sandbox/internal/session"
)

// PlannedStop marks an intentional host/API stop before any target shutdown.
// Native error metadata still takes precedence over this cancellation reason.
var PlannedStop = errors.New("planned target stop")

func plannedShutdown(ctx context.Context) bool {
	cause := context.Cause(ctx)
	return errors.Is(cause, PlannedStop) || errors.Is(cause, context.Canceled)
}

func terminalError(m session.Metadata) error {
	if m.Phase != session.Error {
		return nil
	}
	if m.TerminationReason != "" {
		return errors.New(m.TerminationReason)
	}
	return errors.New("TARGET_FAILED")
}
