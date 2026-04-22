package sync

import (
	"context"
	"fmt"
)

// loadActiveScopes refreshes watch runtime scope state from the persisted
// block_scopes table. The store remains the restart/recovery record; watch
// mode keeps only the current working set in memory.
func (flow *engineFlow) loadActiveScopes(ctx context.Context, watch *watchRuntime) error {
	if watch == nil {
		return nil
	}

	blocks, err := flow.engine.baseline.ListBlockScopes(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing active scopes: %w", err)
	}

	activeScopes := make([]ActiveScope, 0, len(blocks))
	for i := range blocks {
		activeScopes = append(activeScopes, activeScopeFromBlockScopeRow(blocks[i]))
	}
	watch.replaceActiveScopes(activeScopes)

	return nil
}

// normalizePersistedScopes removes stale persisted scopes before any admission
// begins. block_scopes now owns only timed shared blockers for blocked work,
// so a persisted scope with no blocked retry_work is dead state and must be
// discarded immediately on startup.
func (flow *engineFlow) normalizePersistedScopes(
	ctx context.Context,
	watch *watchRuntime,
) error {
	blocks, listScopeErr := flow.engine.baseline.ListBlockScopes(ctx)
	if listScopeErr != nil {
		return fmt.Errorf("sync: listing block scopes: %w", listScopeErr)
	}

	blockedRetries, err := flow.loadNormalizedPersistedBlockedRetries(ctx)
	if err != nil {
		return err
	}
	if err := flow.applyPersistedScopeNormalization(
		ctx,
		planPersistedScopeNormalization(blocks, blockedRetries),
	); err != nil {
		return err
	}

	flow.mustAssertInvariants(ctx, watch, "normalize persisted scopes")

	return nil
}

func (flow *engineFlow) loadNormalizedPersistedBlockedRetries(
	ctx context.Context,
) ([]RetryWorkRow, error) {
	rows, err := flow.engine.baseline.ListBlockedRetryWork(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing blocked retry_work rows: %w", err)
	}

	return rows, nil
}

func (flow *engineFlow) applyPersistedScopeNormalization(
	ctx context.Context,
	plan []persistedScopeNormalizationStep,
) error {
	for i := range plan {
		if err := flow.dropStartupScopeRow(ctx, plan[i].Key, plan[i].Note); err != nil {
			return err
		}
	}

	return nil
}

func (flow *engineFlow) dropStartupScopeRow(ctx context.Context, key ScopeKey, note string) error {
	if err := flow.engine.baseline.DeleteBlockScope(ctx, key); err != nil {
		return fmt.Errorf("sync: deleting startup scope %s: %w", key.String(), err)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventStartupScopeNormalized,
		ScopeKey: key,
		Note:     note,
	})
	return nil
}
