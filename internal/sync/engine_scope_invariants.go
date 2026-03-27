package sync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// scopeInvariantChecksEnabled gates expensive invariant assertions used by
// tests and debug sessions. Production keeps this disabled by default.
func (e *Engine) scopeInvariantChecksEnabled() bool {
	return e.assertScopeInvariants
}

func (e *Engine) mustAssertScopeInvariants(ctx context.Context, stage string) {
	if !e.scopeInvariantChecksEnabled() {
		return
	}
	if err := e.assertCurrentScopeInvariants(context.WithoutCancel(ctx)); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (e *Engine) mustAssertReleasedScope(ctx context.Context, key synctypes.ScopeKey, stage string) {
	if !e.scopeInvariantChecksEnabled() {
		return
	}
	if err := e.assertReleasedScope(context.WithoutCancel(ctx), key); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (e *Engine) mustAssertDiscardedScope(ctx context.Context, key synctypes.ScopeKey, stage string) {
	if !e.scopeInvariantChecksEnabled() {
		return
	}
	if err := e.assertDiscardedScope(context.WithoutCancel(ctx), key); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (e *Engine) assertCurrentScopeInvariants(ctx context.Context) error {
	if e.watch != nil {
		seen := make(map[synctypes.ScopeKey]struct{}, len(e.watch.activeScopes))
		for i := range e.watch.activeScopes {
			key := e.watch.activeScopes[i].Key
			if _, ok := seen[key]; ok {
				return fmt.Errorf("duplicate active scope key %s", key.String())
			}
			seen[key] = struct{}{}
		}
	}

	blocks, err := e.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return fmt.Errorf("listing scope blocks: %w", err)
	}

	localIssues, err := e.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssueLocalPermissionDenied)
	if err != nil {
		return fmt.Errorf("listing local permission failures: %w", err)
	}
	remoteIssues, err := e.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssuePermissionDenied)
	if err != nil {
		return fmt.Errorf("listing remote permission failures: %w", err)
	}
	active := activePermissionScopes(localIssues, remoteIssues)

	for i := range blocks {
		key := blocks[i].Key
		if !isPermissionScopeKey(key) {
			continue
		}
		if !active[key] {
			return fmt.Errorf("permission scope %s has no actionable boundary row", key.String())
		}
	}

	return nil
}

func (e *Engine) assertReleasedScope(ctx context.Context, key synctypes.ScopeKey) error {
	if e.watch != nil && e.isScopeBlocked(key) {
		return fmt.Errorf("released scope %s still active in watch state", key.String())
	}

	blocks, err := e.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return fmt.Errorf("listing scope blocks: %w", err)
	}
	for i := range blocks {
		if blocks[i].Key == key {
			return fmt.Errorf("released scope %s still persisted", key.String())
		}
	}

	rows, err := e.baseline.ListSyncFailures(ctx)
	if err != nil {
		return fmt.Errorf("listing sync failures: %w", err)
	}
	for i := range rows {
		if rows[i].ScopeKey != key {
			continue
		}
		if rows[i].Category == synctypes.CategoryActionable {
			return fmt.Errorf("released scope %s still has actionable boundary row %s", key.String(), rows[i].Path)
		}
		if rows[i].Category == synctypes.CategoryTransient && rows[i].NextRetryAt == 0 {
			return fmt.Errorf("released scope %s still has held transient row %s", key.String(), rows[i].Path)
		}
	}

	return nil
}

func (e *Engine) assertDiscardedScope(ctx context.Context, key synctypes.ScopeKey) error {
	if e.watch != nil && e.isScopeBlocked(key) {
		return fmt.Errorf("discarded scope %s still active in watch state", key.String())
	}

	blocks, err := e.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return fmt.Errorf("listing scope blocks: %w", err)
	}
	for i := range blocks {
		if blocks[i].Key == key {
			return fmt.Errorf("discarded scope %s still persisted", key.String())
		}
	}

	rows, err := e.baseline.ListSyncFailures(ctx)
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
