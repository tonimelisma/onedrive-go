package sync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// scopeInvariantChecksEnabled gates expensive invariant assertions used by
// tests and debug sessions. Production keeps this disabled by default.
func (flow *engineFlow) scopeInvariantChecksEnabled() bool {
	return flow.engine.assertScopeInvariants
}

func (flow *engineFlow) mustAssertScopeInvariants(ctx context.Context, watch *watchRuntime, stage string) {
	if !flow.scopeInvariantChecksEnabled() {
		return
	}
	if err := flow.assertCurrentScopeInvariants(context.WithoutCancel(ctx), watch); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertReleasedScope(ctx context.Context, watch *watchRuntime, key synctypes.ScopeKey, stage string) {
	if !flow.scopeInvariantChecksEnabled() {
		return
	}
	if err := flow.assertReleasedScope(context.WithoutCancel(ctx), watch, key); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertDiscardedScope(ctx context.Context, watch *watchRuntime, key synctypes.ScopeKey, stage string) {
	if !flow.scopeInvariantChecksEnabled() {
		return
	}
	if err := flow.assertDiscardedScope(context.WithoutCancel(ctx), watch, key); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) assertCurrentScopeInvariants(ctx context.Context, watch *watchRuntime) error {
	if watch != nil {
		activeScopes := watch.snapshotActiveScopes()
		seen := make(map[synctypes.ScopeKey]struct{}, len(activeScopes))
		for i := range activeScopes {
			key := activeScopes[i].Key
			if _, ok := seen[key]; ok {
				return fmt.Errorf("duplicate active scope key %s", key.String())
			}
			seen[key] = struct{}{}
		}
	}

	blocks, err := flow.engine.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return fmt.Errorf("listing scope blocks: %w", err)
	}

	rows, err := flow.engine.baseline.ListSyncFailures(ctx)
	if err != nil {
		return fmt.Errorf("listing sync failures: %w", err)
	}
	facts := summarizePersistedScopeFailures(rows)
	boundaryKeys := make(map[synctypes.ScopeKey]struct{})

	for i := range rows {
		if err := validatePersistedFailureRow(&rows[i]); err != nil {
			return err
		}
		if rows[i].Role != synctypes.FailureRoleBoundary {
			continue
		}
		if _, ok := boundaryKeys[rows[i].ScopeKey]; ok {
			return fmt.Errorf("duplicate boundary row for scope %s", rows[i].ScopeKey.String())
		}
		boundaryKeys[rows[i].ScopeKey] = struct{}{}
	}

	for i := range blocks {
		key := blocks[i].Key
		if !isPermissionScopeKey(key) {
			continue
		}
		if !facts.boundaryKeys[key] {
			return fmt.Errorf("permission scope %s has no actionable boundary row", key.String())
		}
	}

	return nil
}

func (flow *engineFlow) assertReleasedScope(ctx context.Context, watch *watchRuntime, key synctypes.ScopeKey) error {
	if watch != nil && flow.scopeController().isScopeBlocked(watch, key) {
		return fmt.Errorf("released scope %s still active in watch state", key.String())
	}

	blocks, err := flow.engine.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return fmt.Errorf("listing scope blocks: %w", err)
	}
	for i := range blocks {
		if blocks[i].Key == key {
			return fmt.Errorf("released scope %s still persisted", key.String())
		}
	}

	rows, err := flow.engine.baseline.ListSyncFailures(ctx)
	if err != nil {
		return fmt.Errorf("listing sync failures: %w", err)
	}
	for i := range rows {
		if rows[i].ScopeKey != key {
			continue
		}
		if rows[i].Role == synctypes.FailureRoleBoundary {
			return fmt.Errorf("released scope %s still has actionable boundary row %s", key.String(), rows[i].Path)
		}
		if rows[i].Role == synctypes.FailureRoleHeld {
			return fmt.Errorf("released scope %s still has held transient row %s", key.String(), rows[i].Path)
		}
		if rows[i].Role == synctypes.FailureRoleItem &&
			rows[i].Category == synctypes.CategoryTransient &&
			rows[i].NextRetryAt <= 0 {
			return fmt.Errorf("released scope %s still has non-retryable transient row %s", key.String(), rows[i].Path)
		}
	}

	return nil
}

func (flow *engineFlow) assertDiscardedScope(ctx context.Context, watch *watchRuntime, key synctypes.ScopeKey) error {
	if watch != nil && flow.scopeController().isScopeBlocked(watch, key) {
		return fmt.Errorf("discarded scope %s still active in watch state", key.String())
	}

	blocks, err := flow.engine.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return fmt.Errorf("listing scope blocks: %w", err)
	}
	for i := range blocks {
		if blocks[i].Key == key {
			return fmt.Errorf("discarded scope %s still persisted", key.String())
		}
	}

	rows, err := flow.engine.baseline.ListSyncFailures(ctx)
	if err != nil {
		return fmt.Errorf("listing sync failures: %w", err)
	}
	for i := range rows {
		if rows[i].ScopeKey == key {
			return fmt.Errorf("discarded scope %s still has failure row %s", key.String(), rows[i].Path)
		}
	}

	return nil
}

func validatePersistedFailureRow(row *synctypes.SyncFailureRow) error {
	switch row.Role {
	case synctypes.FailureRoleHeld:
		if row.ScopeKey.IsZero() {
			return fmt.Errorf("held row %s is missing scope key", row.Path)
		}
		if row.Category != synctypes.CategoryTransient {
			return fmt.Errorf("held row %s must be transient", row.Path)
		}
		if row.NextRetryAt != 0 {
			return fmt.Errorf("held row %s must not be retryable before release", row.Path)
		}
	case synctypes.FailureRoleBoundary:
		if row.ScopeKey.IsZero() {
			return fmt.Errorf("boundary row %s is missing scope key", row.Path)
		}
		if row.Category != synctypes.CategoryActionable {
			return fmt.Errorf("boundary row %s must be actionable", row.Path)
		}
		if row.NextRetryAt != 0 {
			return fmt.Errorf("boundary row %s must not have retry timing", row.Path)
		}
	case synctypes.FailureRoleItem:
	default:
		return fmt.Errorf("row %s has invalid failure role %q", row.Path, row.Role)
	}

	return nil
}
