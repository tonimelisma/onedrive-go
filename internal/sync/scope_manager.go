// Package sync coordinates end-to-end sync engine behavior.
//
// Extracted so that PermissionHandler (and future extracted structs) can depend
// on a narrow interface rather than function callbacks. Engine satisfies this
// interface via its explicit scope lifecycle operations.
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
