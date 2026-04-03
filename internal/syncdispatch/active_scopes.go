// Package syncdispatch holds sync execution primitives that are shared across
// the engine and its tests.
//
// This file contains the pure helper functions for evaluating active scope
// blocks once watch-mode runtime ownership moved fully into the engine.
package syncdispatch

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// FindBlockingScope returns the highest-priority active scope that blocks the
// action, or the zero-value key when no scope matches.
//
// The caller owns the blocks slice and decides how to persist or mutate it.
// This function is pure — no locking, no persistence, no callbacks.
func FindBlockingScope(blocks []synctypes.ScopeBlock, ta *synctypes.TrackedAction) synctypes.ScopeKey {
	if len(blocks) == 0 {
		return synctypes.ScopeKey{}
	}

	scKey := ta.Action.ShortcutKey()
	targetsOwn := ta.Action.TargetsOwnDrive()

	bestRank := scopePriorityMax
	bestSpecificity := -1
	var best synctypes.ScopeKey

	for i := range blocks {
		key := blocks[i].Key
		if !key.BlocksAction(ta.Action.Path, scKey, ta.Action.Type, targetsOwn) {
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
func UpsertScope(blocks []synctypes.ScopeBlock, block *synctypes.ScopeBlock) []synctypes.ScopeBlock {
	if block == nil {
		return append([]synctypes.ScopeBlock(nil), blocks...)
	}

	for i := range blocks {
		if blocks[i].Key == block.Key {
			next := append([]synctypes.ScopeBlock(nil), blocks...)
			next[i] = *block
			return next
		}
	}

	next := append([]synctypes.ScopeBlock(nil), blocks...)
	next = append(next, *block)
	return next
}

// RemoveScope returns a copy of blocks with the given key removed.
func RemoveScope(blocks []synctypes.ScopeBlock, key synctypes.ScopeKey) []synctypes.ScopeBlock {
	for i := range blocks {
		if blocks[i].Key != key {
			continue
		}

		next := append([]synctypes.ScopeBlock(nil), blocks[:i]...)
		next = append(next, blocks[i+1:]...)
		return next
	}

	return append([]synctypes.ScopeBlock(nil), blocks...)
}

// HasScope reports whether the given scope key is active.
func HasScope(blocks []synctypes.ScopeBlock, key synctypes.ScopeKey) bool {
	_, ok := LookupScope(blocks, key)
	return ok
}

// LookupScope returns a value copy of the active scope block for the key.
func LookupScope(blocks []synctypes.ScopeBlock, key synctypes.ScopeKey) (synctypes.ScopeBlock, bool) {
	for i := range blocks {
		if blocks[i].Key == key {
			return blocks[i], true
		}
	}

	return synctypes.ScopeBlock{}, false
}

// ExtendScopeTrial returns a copy of blocks with the given scope's trial
// metadata updated. The boolean reports whether the scope existed.
func ExtendScopeTrial(
	blocks []synctypes.ScopeBlock,
	key synctypes.ScopeKey,
	nextAt time.Time,
	newInterval time.Duration,
) ([]synctypes.ScopeBlock, bool) {
	for i := range blocks {
		if blocks[i].Key != key {
			continue
		}

		next := append([]synctypes.ScopeBlock(nil), blocks...)
		next[i].NextTrialAt = nextAt
		next[i].TrialInterval = newInterval
		next[i].TrialCount++
		return next, true
	}

	return append([]synctypes.ScopeBlock(nil), blocks...), false
}

// DueTrials returns the active scope keys whose trial is due at now. Scopes
// with zero NextTrialAt are excluded.
func DueTrials(blocks []synctypes.ScopeBlock, now time.Time) []synctypes.ScopeKey {
	var due []synctypes.ScopeKey

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
func EarliestTrialAt(blocks []synctypes.ScopeBlock) (time.Time, bool) {
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
func ScopeKeys(blocks []synctypes.ScopeBlock) []synctypes.ScopeKey {
	keys := make([]synctypes.ScopeKey, len(blocks))
	for i := range blocks {
		keys[i] = blocks[i].Key
	}
	return keys
}

const (
	scopePriorityAuthAccount = iota
	scopePriorityThrottleAccount
	scopePriorityService
	scopePriorityDiskLocal
	scopePriorityQuotaOwn
	scopePriorityQuotaShortcut
	scopePriorityPermDir
	scopePriorityPermRemote
)

const scopePriorityMax = 99

func scopePriority(key synctypes.ScopeKey) int {
	switch key.Kind {
	case synctypes.ScopeAuthAccount:
		return scopePriorityAuthAccount
	case synctypes.ScopeThrottleAccount:
		return scopePriorityThrottleAccount
	case synctypes.ScopeService:
		return scopePriorityService
	case synctypes.ScopeDiskLocal:
		return scopePriorityDiskLocal
	case synctypes.ScopeQuotaOwn:
		return scopePriorityQuotaOwn
	case synctypes.ScopeQuotaShortcut:
		return scopePriorityQuotaShortcut
	case synctypes.ScopePermDir:
		return scopePriorityPermDir
	case synctypes.ScopePermRemote:
		return scopePriorityPermRemote
	default:
		return scopePriorityMax
	}
}
