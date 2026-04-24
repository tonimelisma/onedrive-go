// Package sync owns the single mounted content-root runtime, including pure helpers for
// active-scope evaluation used by watch-mode execution and its tests.
//
// This file contains the pure helper functions for evaluating active scope
// blocks once watch-mode runtime ownership moved fully into the engine.
package sync

import (
	"time"
)

// FindBlockingScope returns the highest-priority active scope that blocks the
// action, or the zero-value key when no scope matches.
//
// The caller owns the blocks slice and decides how to persist or mutate it.
// This function is pure — no locking, no persistence, no callbacks.
func FindBlockingScope(blocks []ActiveScope, ta *TrackedAction) ScopeKey {
	if len(blocks) == 0 {
		return ScopeKey{}
	}

	throttleTargetKey := ta.Action.ThrottleTargetKey()

	bestRank := scopePriorityMax
	bestSpecificity := -1
	var best ScopeKey

	for i := range blocks {
		key := blocks[i].Key
		if !scopeBlocksTrackedAction(key, ta, throttleTargetKey) {
			continue
		}

		rank := DescribeScopeKey(key).Priority
		specificity := len(key.Param)
		if rank < bestRank || (rank == bestRank && specificity > bestSpecificity) {
			bestRank = rank
			bestSpecificity = specificity
			best = key
		}
	}

	return best
}

func scopeBlocksTrackedAction(key ScopeKey, ta *TrackedAction, throttleTargetKey string) bool {
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
func UpsertScope(blocks []ActiveScope, block *ActiveScope) []ActiveScope {
	if block == nil {
		return append([]ActiveScope(nil), blocks...)
	}

	for i := range blocks {
		if blocks[i].Key == block.Key {
			next := append([]ActiveScope(nil), blocks...)
			next[i] = *block
			return next
		}
	}

	next := append([]ActiveScope(nil), blocks...)
	next = append(next, *block)
	return next
}

// RemoveScope returns a copy of blocks with the given key removed.
func RemoveScope(blocks []ActiveScope, key ScopeKey) []ActiveScope {
	for i := range blocks {
		if blocks[i].Key != key {
			continue
		}

		next := append([]ActiveScope(nil), blocks[:i]...)
		next = append(next, blocks[i+1:]...)
		return next
	}

	return append([]ActiveScope(nil), blocks...)
}

// HasScope reports whether the given scope key is active.
func HasScope(blocks []ActiveScope, key ScopeKey) bool {
	_, ok := LookupScope(blocks, key)
	return ok
}

// LookupScope returns a value copy of the active block scope for the key.
func LookupScope(blocks []ActiveScope, key ScopeKey) (ActiveScope, bool) {
	for i := range blocks {
		if blocks[i].Key == key {
			return blocks[i], true
		}
	}

	return ActiveScope{}, false
}

// ExtendScopeTrial returns a copy of blocks with the given scope's trial
// metadata updated. The boolean reports whether the scope existed.
func ExtendScopeTrial(
	blocks []ActiveScope,
	key ScopeKey,
	nextAt time.Time,
	newInterval time.Duration,
) ([]ActiveScope, bool) {
	for i := range blocks {
		if blocks[i].Key != key {
			continue
		}

		next := append([]ActiveScope(nil), blocks...)
		next[i].NextTrialAt = nextAt
		next[i].TrialInterval = newInterval
		return next, true
	}

	return append([]ActiveScope(nil), blocks...), false
}

// DueTrials returns the active scope keys whose trial is due at now. Scopes
// with zero NextTrialAt are excluded.
func DueTrials(blocks []ActiveScope, now time.Time) []ScopeKey {
	var due []ScopeKey

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

// EarliestTrialAt returns the earliest pending trial time across all active
// scopes. Scopes with zero NextTrialAt are skipped.
func EarliestTrialAt(blocks []ActiveScope) (time.Time, bool) {
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

// ScopeKeys returns the active scope keys in slice order.
func ScopeKeys(blocks []ActiveScope) []ScopeKey {
	keys := make([]ScopeKey, len(blocks))
	for i := range blocks {
		keys[i] = blocks[i].Key
	}
	return keys
}
