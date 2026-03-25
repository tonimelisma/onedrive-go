// scope_gate.go — Scope-based admission control with persistent scope blocks.
//
// ScopeGate owns scope block state and admission decisions. No held queue,
// no dependency awareness, no channels.
//
// Admit checks if an action matches any active scope block. The caller
// (engine) decides what to do with blocked actions — typically record as
// a sync_failure and complete in DepGraph. This separation means ScopeGate
// never holds actions or manages their lifecycle.
//
// SetScopeBlock, ClearScopeBlock, ExtendTrialInterval are write-through:
// persist to DB first, then update memory. On store error, memory is unchanged.
// LoadFromStore reads all rows on startup.
//
// AllDueTrials and EarliestTrialAt do NOT check held queue length (there is
// no held queue). Any scope block with non-zero NextTrialAt is eligible for
// trials. The engine uses PickTrialCandidate from sync_failures to find
// actual items to retry.
//
// Thread-safety: all methods are safe for concurrent use via mu.
//
// Fixes D-2 (no onHeld callback — no cross-lock paths),
// fixes D-8 (scope blocks persisted, survive crash).
package syncdispatch

import (
	"context"
	"log/slog"
	stdsync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ScopeGate is scope-based admission control with persistent scope blocks.
// No held queue, no dependency awareness, no channels.
type ScopeGate struct {
	mu     stdsync.Mutex
	blocks map[synctypes.ScopeKey]*synctypes.ScopeBlock
	store  synctypes.ScopeBlockStore
	logger *slog.Logger
}

// NewScopeGate creates a ScopeGate with the given store and logger.
// The in-memory blocks map starts empty — call LoadFromStore to populate
// from persisted state at startup.
func NewScopeGate(store synctypes.ScopeBlockStore, logger *slog.Logger) *ScopeGate {
	return &ScopeGate{
		blocks: make(map[synctypes.ScopeKey]*synctypes.ScopeBlock),
		store:  store,
		logger: logger,
	}
}

// Admit checks if the action matches any active scope block. Returns the
// blocking ScopeKey or the zero-value ScopeKey if not blocked. Does NOT
// hold the action — the caller records it as a sync_failure and completes
// it in DepGraph.
//
// Priority-ordered: global blocks (throttle:account, service) are checked
// first, then progressively narrower scopes (disk:local, quota:own).
// Dynamic-key scopes (quota:shortcut, perm:dir) are checked last via O(n)
// iteration over active blocks — expected to be tiny (1-5 typically).
func (g *ScopeGate) Admit(ta *synctypes.TrackedAction) synctypes.ScopeKey {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(g.blocks) == 0 {
		return synctypes.ScopeKey{}
	}

	// Priority-ordered fixed keys: global blocks first, then narrower scopes.
	priorityKeys := [...]synctypes.ScopeKey{
		synctypes.SKThrottleAccount, // blocks ALL actions (R-6.8.4, R-2.10.26)
		synctypes.SKService,         // blocks ALL actions (R-2.10.28)
		synctypes.SKDiskLocal,       // blocks downloads only (R-2.10.43)
		synctypes.SKQuotaOwn,        // blocks own-drive uploads (R-2.10.19)
	}

	scKey := ta.Action.ShortcutKey()
	targetsOwn := ta.Action.TargetsOwnDrive()

	for _, sk := range priorityKeys {
		if _, ok := g.blocks[sk]; ok && sk.BlocksAction(ta.Action.Path, scKey, ta.Action.Type, targetsOwn) {
			return sk
		}
	}

	// Dynamic-key scopes: shortcut quota and permission boundaries depend on
	// action context.
	for sk := range g.blocks {
		switch sk.Kind { //nolint:exhaustive // only parameterized scopes need per-action checking
		case synctypes.ScopeQuotaShortcut, synctypes.ScopePermDir, synctypes.ScopePermRemote:
			if sk.BlocksAction(ta.Action.Path, scKey, ta.Action.Type, targetsOwn) {
				return sk
			}
		}
	}

	return synctypes.ScopeKey{}
}

// SetScopeBlock registers a scope block. Persists to DB first, then updates
// memory. If the store returns an error, memory is unchanged.
//
// If there's an existing block for the same key, it is replaced (updated
// trial timing).
func (g *ScopeGate) SetScopeBlock(ctx context.Context, key synctypes.ScopeKey, block *synctypes.ScopeBlock) error {
	// Persist first — store is source of truth.
	if err := g.store.UpsertScopeBlock(ctx, block); err != nil {
		return err
	}

	g.mu.Lock()
	g.blocks[key] = block
	g.mu.Unlock()

	g.logger.Info("scope_gate: scope blocked",
		slog.String("scope_key", key.String()),
		slog.String("issue_type", block.IssueType),
	)

	return nil
}

// ClearScopeBlock removes a scope block. Deletes from DB first, then removes
// from memory. If the store returns an error, memory is unchanged.
func (g *ScopeGate) ClearScopeBlock(ctx context.Context, key synctypes.ScopeKey) error {
	// Delete from store first.
	if err := g.store.DeleteScopeBlock(ctx, key); err != nil {
		return err
	}

	g.mu.Lock()
	delete(g.blocks, key)
	g.mu.Unlock()

	g.logger.Info("scope_gate: scope cleared",
		slog.String("scope_key", key.String()),
	)

	return nil
}

// IsScopeBlocked returns true if the given scope key has an active block.
func (g *ScopeGate) IsScopeBlocked(key synctypes.ScopeKey) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	_, ok := g.blocks[key]
	return ok
}

// GetScopeBlock returns a snapshot of the ScopeBlock for the given key, or
// (ScopeBlock{}, false) if the scope is not blocked. Returns a copy to
// prevent unsynchronized mutation of gate-owned state.
func (g *ScopeGate) GetScopeBlock(key synctypes.ScopeKey) (synctypes.ScopeBlock, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	block, ok := g.blocks[key]
	if !ok {
		return synctypes.ScopeBlock{}, false
	}

	return *block, true
}

// ExtendTrialInterval atomically updates the block's TrialInterval, sets
// NextTrialAt, increments TrialCount, and persists the change. If the scope
// key is unknown, this is a no-op (returns nil). On store error, memory is
// unchanged.
func (g *ScopeGate) ExtendTrialInterval(ctx context.Context, key synctypes.ScopeKey, nextAt time.Time, newInterval time.Duration) error {
	g.mu.Lock()

	block, ok := g.blocks[key]
	if !ok {
		g.mu.Unlock()
		return nil
	}

	// Build updated block for persistence check.
	updated := *block
	updated.TrialInterval = newInterval
	updated.NextTrialAt = nextAt
	updated.TrialCount++

	g.mu.Unlock()

	// Persist the updated block.
	if err := g.store.UpsertScopeBlock(ctx, &updated); err != nil {
		return err
	}

	// Apply to memory only after successful persist. Re-read the pointer
	// from the map to avoid TOCTOU — if SetScopeBlock replaced the entry
	// between locks, the captured `block` pointer is orphaned.
	g.mu.Lock()
	if current, stillExists := g.blocks[key]; stillExists && current == block {
		current.TrialInterval = newInterval
		current.NextTrialAt = nextAt
		current.TrialCount = updated.TrialCount
	}
	g.mu.Unlock()

	return nil
}

// AllDueTrials returns all scope keys whose NextTrialAt is due (now >= NextTrialAt).
// Returns a snapshot — the caller iterates each scope exactly once, making
// the loop structurally incapable of infinite iteration.
// Scopes with zero NextTrialAt are excluded (not trial-eligible).
//
// Does NOT check held queue length — any scope block with non-zero
// NextTrialAt is eligible. The engine uses
// PickTrialCandidate from sync_failures to find actual items.
func (g *ScopeGate) AllDueTrials(now time.Time) []synctypes.ScopeKey {
	g.mu.Lock()
	defer g.mu.Unlock()

	var due []synctypes.ScopeKey
	for key, block := range g.blocks {
		if block.NextTrialAt.IsZero() {
			continue
		}

		if !now.Before(block.NextTrialAt) {
			due = append(due, key)
		}
	}

	return due
}

// EarliestTrialAt returns the earliest NextTrialAt across all scope blocks.
// Returns (time.Time{}, false) if no trials are pending.
//
// Does NOT check held queue length — any scope block with non-zero
// NextTrialAt is eligible.
func (g *ScopeGate) EarliestTrialAt() (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var earliest time.Time
	found := false

	for _, block := range g.blocks {
		if block.NextTrialAt.IsZero() {
			continue
		}

		if !found || block.NextTrialAt.Before(earliest) {
			earliest = block.NextTrialAt
			found = true
		}
	}

	return earliest, found
}

// ScopeBlockKeys returns the keys of all active scope blocks. Used by
// handleExternalChanges to detect when perm:dir failures have been cleared
// via CLI.
func (g *ScopeGate) ScopeBlockKeys() []synctypes.ScopeKey {
	g.mu.Lock()
	defer g.mu.Unlock()

	keys := make([]synctypes.ScopeKey, 0, len(g.blocks))
	for k := range g.blocks {
		keys = append(keys, k)
	}

	return keys
}

// LoadFromStore reads all persisted scope blocks into the in-memory map.
// Called at startup to restore scope gate state after a crash or restart.
// Replaces any existing in-memory state.
func (g *ScopeGate) LoadFromStore(ctx context.Context) error {
	blocks, err := g.store.ListScopeBlocks(ctx)
	if err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// Clear existing state and repopulate from store.
	g.blocks = make(map[synctypes.ScopeKey]*synctypes.ScopeBlock, len(blocks))
	for _, block := range blocks {
		g.blocks[block.Key] = block
	}

	if len(blocks) > 0 {
		g.logger.Info("scope_gate: loaded persisted scope blocks",
			slog.Int("count", len(blocks)),
		)
	}

	return nil
}
