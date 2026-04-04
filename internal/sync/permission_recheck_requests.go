package sync

import (
	"context"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func scopeKeySet(keys []synctypes.ScopeKey) map[synctypes.ScopeKey]bool {
	set := make(map[synctypes.ScopeKey]bool, len(keys))
	for i := range keys {
		set[keys[i]] = true
	}
	return set
}

func clearRequestedScopeRechecks(
	ctx context.Context,
	store interface {
		ClearScopeRecheckRequest(context.Context, synctypes.ScopeKey) error
	},
	logger *slog.Logger,
	keys []synctypes.ScopeKey,
) {
	for i := range keys {
		if err := store.ClearScopeRecheckRequest(ctx, keys[i]); err != nil {
			logger.Warn("failed to clear scope recheck request",
				slog.String("scope_key", keys[i].String()),
				slog.String("error", err.Error()),
			)
		}
	}
}

func (rt *watchRuntime) handleRequestedPermissionRechecks(ctx context.Context) {
	if !rt.engine.permHandler.HasPermChecker() || rt.scopeController().isObservationSuppressed(rt) {
		return
	}

	requestedKeys, err := rt.engine.baseline.ListRequestedScopeRechecks(ctx)
	if err != nil {
		rt.engine.logger.Warn("failed to list requested permission rechecks",
			slog.String("error", err.Error()),
		)
		return
	}
	if len(requestedKeys) == 0 {
		return
	}

	bl, err := rt.engine.baseline.Load(ctx)
	if err != nil {
		rt.engine.logger.Warn("failed to load baseline for requested permission rechecks",
			slog.String("error", err.Error()),
		)
		return
	}

	shortcuts, err := rt.engine.baseline.ListShortcuts(ctx)
	if err != nil {
		rt.engine.logger.Warn("failed to load shortcuts for requested permission rechecks",
			slog.String("error", err.Error()),
		)
		return
	}

	decisions := rt.engine.permHandler.recheckPermissionsForScopeKeys(ctx, bl, shortcuts, scopeKeySet(requestedKeys))
	rt.scopeController().applyPermissionRecheckDecisions(ctx, rt, decisions)
	clearRequestedScopeRechecks(ctx, rt.engine.baseline, rt.engine.logger, requestedKeys)
}
