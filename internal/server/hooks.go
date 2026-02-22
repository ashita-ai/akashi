package server

import (
	"context"

	"github.com/ashita-ai/akashi/internal/model"
)

// DecisionHook receives decision lifecycle events within the server layer.
// Defined here (not in the root akashi package) to avoid a circular import:
// internal/server → akashi → internal/server would be a cycle.
// The root akashi package wraps akashi.EventHook into DecisionHook via an adapter.
//
// Hook methods are called asynchronously in goroutines. Implementations must not
// block indefinitely. Failures are logged and do not fail the originating request.
type DecisionHook interface {
	OnDecisionTraced(ctx context.Context, decision model.Decision) error
	OnConflictDetected(ctx context.Context, conflict model.DecisionConflict) error
}
