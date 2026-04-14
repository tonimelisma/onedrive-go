// Package sync owns the single-drive runtime, including pure helpers for
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
func FindBlockingScope(blocks []ScopeBlock, ta *TrackedAction) ScopeKey {
	if len(blocks) == 0 {
		return ScopeKey{}
	}

	bestRank := scopePriorityMax
	bestSpecificity := -1
	var best ScopeKey

	for i := range blocks {
		key := blocks[i].Key
		if !key.BlocksTrackedAction(ta) {
			continue
		}

		rank := scopePriority(key)
		specificity := len(key.Param)
		if rank < bestRank || (rank == bestRank && specificity > bestSpecificity) {
			bestRank = rank
			bestSpecificity = specificity
			best = key
		}
	}

	return best
}

// UpsertScope returns a copy of blocks with the provided scope inserted or
// replaced by key.
func UpsertScope(blocks []ScopeBlock, block *ScopeBlock) []ScopeBlock {
	if block == nil {
		return append([]ScopeBlock(nil), blocks...)
	}

	for i := range blocks {
		if blocks[i].Key == block.Key {
			next := append([]ScopeBlock(nil), blocks...)
			next[i] = *block
			return next
		}
	}

	next := append([]ScopeBlock(nil), blocks...)
	next = append(next, *block)
	return next
}

// RemoveScope returns a copy of blocks with the given key removed.
func RemoveScope(blocks []ScopeBlock, key ScopeKey) []ScopeBlock {
	for i := range blocks {
		if blocks[i].Key != key {
			continue
		}

		next := append([]ScopeBlock(nil), blocks[:i]...)
		next = append(next, blocks[i+1:]...)
		return next
	}

	return append([]ScopeBlock(nil), blocks...)
}

// HasScope reports whether the given scope key is active.
func HasScope(blocks []ScopeBlock, key ScopeKey) bool {
	_, ok := LookupScope(blocks, key)
	return ok
}

// LookupScope returns a value copy of the active scope block for the key.
func LookupScope(blocks []ScopeBlock, key ScopeKey) (ScopeBlock, bool) {
	for i := range blocks {
		if blocks[i].Key == key {
			return blocks[i], true
		}
	}

	return ScopeBlock{}, false
}

// ExtendScopeTrial returns a copy of blocks with the given scope's trial
// metadata updated. The boolean reports whether the scope existed.
func ExtendScopeTrial(
	blocks []ScopeBlock,
	key ScopeKey,
	nextAt time.Time,
	newInterval time.Duration,
) ([]ScopeBlock, bool) {
	for i := range blocks {
		if blocks[i].Key != key {
			continue
		}

		next := append([]ScopeBlock(nil), blocks...)
		next[i].NextTrialAt = nextAt
		next[i].TrialInterval = newInterval
		next[i].TrialCount++
		return next, true
	}

	return append([]ScopeBlock(nil), blocks...), false
}

// DueTrials returns the active scope keys whose trial is due at now. Scopes
// with zero NextTrialAt are excluded.
func DueTrials(blocks []ScopeBlock, now time.Time) []ScopeKey {
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
func EarliestTrialAt(blocks []ScopeBlock) (time.Time, bool) {
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
func ScopeKeys(blocks []ScopeBlock) []ScopeKey {
	keys := make([]ScopeKey, len(blocks))
	for i := range blocks {
		keys[i] = blocks[i].Key
	}
	return keys
}

const (
	scopePriorityAuthAccount = iota
	scopePriorityThrottleAccount
	scopePriorityThrottleTarget
	scopePriorityService
	scopePriorityDiskLocal
	scopePriorityQuotaOwn
	scopePriorityQuotaShortcut
	scopePriorityPermLocalRead
	scopePriorityPermLocalWrite
	scopePriorityPermRemoteWrite
)

const scopePriorityMax = 99

func scopePriority(key ScopeKey) int {
	switch key.Kind {
	case ScopeAuthAccount:
		return scopePriorityAuthAccount
	case ScopeThrottleAccount:
		return scopePriorityThrottleAccount
	case ScopeThrottleTarget:
		return scopePriorityThrottleTarget
	case ScopeService:
		return scopePriorityService
	case ScopeDiskLocal:
		return scopePriorityDiskLocal
	case ScopeQuotaOwn:
		return scopePriorityQuotaOwn
	case ScopeQuotaShortcut:
		return scopePriorityQuotaShortcut
	case ScopePermLocalRead:
		return scopePriorityPermLocalRead
	case ScopePermLocalWrite:
		return scopePriorityPermLocalWrite
	case ScopePermRemoteWrite:
		return scopePriorityPermRemoteWrite
	default:
		return scopePriorityMax
	}
}
