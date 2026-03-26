// scope_manager.go — scopeManager interface for Engine scope lifecycle.
//
// Extracted so that PermissionHandler (and future extracted structs) can depend
// on a narrow interface rather than function callbacks. Engine satisfies this
// interface implicitly via its existing setScopeBlock and onScopeClear methods.
package sync

import (
	"context"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// scopeManager abstracts semantic scope lifecycle operations. Callers express
// meaning ("activate", "release", "discard"), and the engine owns how those
// transitions update durable and runtime state.
type scopeManager interface {
	activateScope(ctx context.Context, block synctypes.ScopeBlock) error
	releaseScope(ctx context.Context, key synctypes.ScopeKey) error
	discardScope(ctx context.Context, key synctypes.ScopeKey) error
}
