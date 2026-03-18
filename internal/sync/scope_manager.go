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

// scopeManager abstracts Engine's scope lifecycle operations. Extracted so
// that PermissionHandler (and future extracted structs) can depend on a
// narrow interface rather than function callbacks.
type scopeManager interface {
	setScopeBlock(key synctypes.ScopeKey, block *synctypes.ScopeBlock)
	onScopeClear(ctx context.Context, key synctypes.ScopeKey)
	isWatchMode() bool
}
