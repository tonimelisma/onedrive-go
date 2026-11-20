// Deterministic helpers that evaluate active scope blocks for watch-mode
// execution and its tests.

package syncexec

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// FindBlockingScope returns the highest-priority active scope that blocks the
// action, or the zero-value key when no scope matches.
//
// The caller owns the blocks slice and decides how to persist or mutate it.
// This function is pure — no locking, no persistence, no callbacks.
func FindBlockingScope(blocks []syncdomain.ActiveScope, ta *syncdomain.TrackedAction) syncdomain.ScopeKey {
	if len(blocks) == 0 {
		return syncdomain.ScopeKey{}
	}

	throttleTargetKey := ta.Action.ThrottleTargetKey()

	bestRank := syncdomain.ScopePriorityMax
	bestSpecificity := -1
	var best syncdomain.ScopeKey

	for i := range blocks {
		key := blocks[i].Key
		if !ScopeBlocksTrackedAction(key, ta, throttleTargetKey) {
			continue
		}

		rank := syncdomain.DescribeScopeKey(key).Priority
		specificity := len(key.Param)
		if rank < bestRank || (rank == bestRank && specificity > bestSpecificity) {
			bestRank = rank
			bestSpecificity = specificity
			best = key
		}
	}

	return best
}

func ScopeBlocksTrackedAction(key syncdomain.ScopeKey, ta *syncdomain.TrackedAction, throttleTargetKey string) bool {
	if ta == nil {
		return false
	}

	paths := []string{ta.Action.Path}
	if ta.Action.OldPath != "" && ta.Action.OldPath != ta.Action.Path {
		paths = append(paths, ta.Action.OldPath)
	}

	for _, candidate := range paths {
		if key.BlocksAction(candidate, throttleTargetKey, ta.Action.Type) {
			return true
		}
	}

	return false
}

// UpsertScope returns a copy of blocks with the provided scope inserted or
// replaced by key.
func UpsertScope(blocks []syncdomain.ActiveScope, block *syncdomain.ActiveScope) []syncdomain.ActiveScope {
	if block == nil {
		return append([]syncdomain.ActiveScope(nil), blocks...)
	}

	for i := range blocks {
		if blocks[i].Key == block.Key {
			next := append([]syncdomain.ActiveScope(nil), blocks...)
			next[i] = *block
			return next
		}
	}

	next := append([]syncdomain.ActiveScope(nil), blocks...)
	next = append(next, *block)
	return next
}

// RemoveScope returns a copy of blocks with the given key removed.
func RemoveScope(blocks []syncdomain.ActiveScope, key syncdomain.ScopeKey) []syncdomain.ActiveScope {
	for i := range blocks {
		if blocks[i].Key != key {
			continue
		}

		next := append([]syncdomain.ActiveScope(nil), blocks[:i]...)
		next = append(next, blocks[i+1:]...)
		return next
	}

	return append([]syncdomain.ActiveScope(nil), blocks...)
}

// HasScope reports whether the given scope key is active.
func HasScope(blocks []syncdomain.ActiveScope, key syncdomain.ScopeKey) bool {
	_, ok := LookupScope(blocks, key)
	return ok
}

// LookupScope returns a value copy of the active block scope for the key.
func LookupScope(blocks []syncdomain.ActiveScope, key syncdomain.ScopeKey) (syncdomain.ActiveScope, bool) {
	for i := range blocks {
		if blocks[i].Key == key {
			return blocks[i], true
		}
	}

	return syncdomain.ActiveScope{}, false
}

// extendScopeTrial returns a copy of blocks with the given scope's trial
// metadata updated. The boolean reports whether the scope existed.
func extendScopeTrial(
	blocks []syncdomain.ActiveScope,
	key syncdomain.ScopeKey,
	nextAt time.Time,
	newInterval time.Duration,
) ([]syncdomain.ActiveScope, bool) {
	for i := range blocks {
		if blocks[i].Key != key {
			continue
		}

		next := append([]syncdomain.ActiveScope(nil), blocks...)
		next[i].NextTrialAt = nextAt
		next[i].TrialInterval = newInterval
		return next, true
	}

	return append([]syncdomain.ActiveScope(nil), blocks...), false
}

// dueTrials returns the active scope keys whose trial is due at now. Scopes
// with zero NextTrialAt are excluded.
func dueTrials(blocks []syncdomain.ActiveScope, now time.Time) []syncdomain.ScopeKey {
	var due []syncdomain.ScopeKey

	for i := range blocks {
		if blocks[i].NextTrialAt.IsZero() {
			continue
		}
		if !now.Before(blocks[i].NextTrialAt) {
			due = append(due, blocks[i].Key)
		}
	}

	return due
}

// earliestTrialAt returns the earliest pending trial time across all active
// scopes. Scopes with zero NextTrialAt are skipped.
func earliestTrialAt(blocks []syncdomain.ActiveScope) (time.Time, bool) {
	var earliest time.Time
	found := false

	for i := range blocks {
		if blocks[i].NextTrialAt.IsZero() {
			continue
		}
		if !found || blocks[i].NextTrialAt.Before(earliest) {
			earliest = blocks[i].NextTrialAt
			found = true
		}
	}

	return earliest, found
}

// scopeKeys returns the active scope keys in slice order.
func scopeKeys(blocks []syncdomain.ActiveScope) []syncdomain.ScopeKey {
	keys := make([]syncdomain.ScopeKey, len(blocks))
	for i := range blocks {
		keys[i] = blocks[i].Key
	}
	return keys
}
