package services

import (
	"encoding/json"
	"net/http"

	"github.com/intrusive-ai/interceptor-sandbox/internal/statejournal"
)

// ManagedHandler is a synthetic HTTP service whose canonical records are
// journaled and can therefore participate in evaluation, injection, snapshot,
// and restore.
type ManagedHandler interface {
	http.Handler
	Apply(statejournal.Operation) error
	Snapshot() map[string]map[string]json.RawMessage
}
