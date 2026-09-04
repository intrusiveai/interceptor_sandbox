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

// ValidatingManagedHandler can reject a proposed journal operation before it
// is committed. It is used for state whose identifiers and structural fields
// are immutable, such as synthetic identity grants and delegations.
type ValidatingManagedHandler interface {
	ValidateApply(statejournal.Operation) error
}

// CreatingManagedHandler validates a new resource against the same sealed
// create contract exposed by the simulated service. Interceptor never invents
// a looser injection-only resource schema.
type CreatingManagedHandler interface {
	SupportsCreate(collection string) bool
	ValidateCreate(statejournal.Operation) error
}
