package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// retryWork is the engine's one path to the retry_work table, which is the
// one authority for retry work. rows is a cache of the table for this run's
// decisions (admission and holds read it): it is written only here, and only
// from what a store call returned, so it never becomes a second authority.
// Nothing else in the engine calls a retry_work mutator of the store, which a
// test holds to the sources (TestRetryWork_IsTheOnlyEngineWriterOfRetryWork).
// Owned by the goroutine that runs the engine's RunOnce or RunWatch.
type retryWork struct {
	rows map[syncdomain.RetryWorkKey]syncdomain.RetryWorkRow
	// installed is set once a plan has installed the cache; before that the
	// cache holds only what this run wrote, so it is not compared with the
	// store (assertRetryCacheMatchesStore).
	installed bool
}

// install replaces the cache with the rows the store listed for a plan.
func (w *retryWork) install(rows []syncdomain.RetryWorkRow) {
	w.rows = make(map[syncdomain.RetryWorkKey]syncdomain.RetryWorkRow, len(rows))
	w.installed = true
	for i := range rows {
		w.rows[rows[i].WorkKey()] = rows[i]
	}
}

// row is the cached row of key.
func (w *retryWork) row(key syncdomain.RetryWorkKey) (syncdomain.RetryWorkRow, bool) {
	row, ok := w.rows[key]
	return row, ok
}

// cache records row as the store returned it.
func (w *retryWork) cache(row *syncdomain.RetryWorkRow) {
	if w.rows == nil {
		w.rows = make(map[syncdomain.RetryWorkKey]syncdomain.RetryWorkRow)
	}
	w.rows[row.WorkKey()] = *row
}

// recordFailure records a failure of failure.Work and returns the row the
// store holds afterwards.
func (w *retryWork) recordFailure(
	ctx context.Context,
	store *syncstore.SyncStore,
	failure *syncdomain.RetryWorkFailure,
	delay func(int) time.Duration,
) (*syncdomain.RetryWorkRow, error) {
	row, err := store.RecordRetryWorkFailure(ctx, failure, delay)
	if err != nil {
		return nil, fmt.Errorf("record retry_work failure for %s: %w", failure.Work.Path, err)
	}
	if row == nil {
		return nil, fmt.Errorf("record retry_work failure for %s: missing persisted row", failure.Work.Path)
	}
	w.cache(row)

	return row, nil
}

// recordBlocked records work blocked under scope and returns the row the
// store holds afterwards.
func (w *retryWork) recordBlocked(
	ctx context.Context,
	store *syncstore.SyncStore,
	work syncdomain.RetryWorkKey,
	scope syncdomain.ScopeKey,
) (*syncdomain.RetryWorkRow, error) {
	row, err := store.RecordBlockedRetryWork(ctx, work, scope)
	if err != nil {
		return nil, fmt.Errorf("persist blocked retry_work for %s under %s: %w", work.Path, scope.String(), err)
	}
	if row == nil {
		return nil, fmt.Errorf("persist blocked retry_work for %s under %s: missing persisted row", work.Path, scope.String())
	}
	w.cache(row)

	return row, nil
}

// resolve deletes work's row after its action succeeded, and drops the
// cached row whether or not the store had one.
func (w *retryWork) resolve(
	ctx context.Context, store *syncstore.SyncStore, work syncdomain.RetryWorkKey,
) (*syncdomain.RetryWorkRow, bool, error) {
	row, found, err := store.ResolveRetryWork(ctx, work)
	if err != nil {
		return nil, false, fmt.Errorf("resolve retry_work for %s: %w", work.Path, err)
	}
	delete(w.rows, work)

	return row, found, nil
}

// clearBlocked deletes work's row when it is blocked under scope, and drops
// the cached row when the store deleted one.
func (w *retryWork) clearBlocked(
	ctx context.Context, store *syncstore.SyncStore, work syncdomain.RetryWorkKey, scope syncdomain.ScopeKey,
) error {
	cleared, err := store.ClearBlockedRetryWork(ctx, work, scope)
	if err != nil {
		return fmt.Errorf("clear blocked retry_work for %s under %s: %w", work.Path, scope.String(), err)
	}
	if cleared {
		delete(w.rows, work)
	}

	return nil
}

// releaseScope releases one ramp batch of scope's blocked rows (R-6.8.24),
// caches the rows the store released, and returns them with whether the
// scope row went.
func (w *retryWork) releaseScope(
	ctx context.Context,
	store *syncstore.SyncStore,
	scope syncdomain.ScopeKey,
	work []syncdomain.RetryWorkKey,
	now time.Time,
) ([]syncdomain.RetryWorkRow, bool, error) {
	released, scopeRemoved, err := store.ReleaseScopeWork(ctx, scope, work, now)
	if err != nil {
		return nil, false, fmt.Errorf("release scope %s: %w", scope.String(), err)
	}
	for i := range released {
		w.cache(&released[i])
	}

	return released, scopeRemoved, nil
}

// discardScope deletes scope and the rows blocked under it, drops the cached
// rows the store deleted, and returns their keys.
func (w *retryWork) discardScope(
	ctx context.Context, store *syncstore.SyncStore, scope syncdomain.ScopeKey,
) ([]syncdomain.RetryWorkKey, error) {
	deleted, err := store.DiscardScope(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("discard scope %s: %w", scope.String(), err)
	}
	for _, key := range deleted {
		delete(w.rows, key)
	}

	return deleted, nil
}

// prune deletes the rows of work no current action names, in the store and
// then in the cache.
func (w *retryWork) prune(ctx context.Context, store *syncstore.SyncStore, keep []syncdomain.RetryWorkKey) error {
	if err := store.PruneRetryWorkToCurrentActions(ctx, keep); err != nil {
		return fmt.Errorf("prune retry_work to current actions: %w", err)
	}
	kept := make(map[syncdomain.RetryWorkKey]struct{}, len(keep))
	for _, key := range keep {
		kept[key] = struct{}{}
	}
	for key := range w.rows {
		if _, ok := kept[key]; !ok {
			delete(w.rows, key)
		}
	}

	return nil
}

// matchesStore reports the first difference between the cache and rows, the
// store's listing, or nil when they agree. A cache no plan has installed is
// not compared.
func (w *retryWork) matchesStore(rows []syncdomain.RetryWorkRow) error {
	if !w.installed {
		return nil
	}
	if len(rows) != len(w.rows) {
		return fmt.Errorf("retry_work cache holds %d rows, the store %d", len(w.rows), len(rows))
	}
	for i := range rows {
		cached, ok := w.rows[rows[i].WorkKey()]
		if !ok {
			return fmt.Errorf("retry_work cache lacks the store's row for %s", rows[i].Path)
		}
		if cached != rows[i] {
			return fmt.Errorf("retry_work cache row for %s differs from the store's", rows[i].Path)
		}
	}

	return nil
}

// releaseOrphans makes every blocked row without its scope ready at now
// (R-2.10.33). It runs at start-up, before any plan installs the cache.
func (w *retryWork) releaseOrphans(ctx context.Context, store *syncstore.SyncStore, now time.Time) (int, error) {
	released, err := store.ReleaseOrphanBlockedRetryWork(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("release orphan blocked retry_work: %w", err)
	}

	return released, nil
}
