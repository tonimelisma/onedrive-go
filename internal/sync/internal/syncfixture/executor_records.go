package syncfixture

import (
	"context"
	"errors"
	"fmt"
	stdsync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// MemoryExecutorRecords is the executor's durable records held in memory:
// the pending creates and the local move intents, with the store's semantics
// (syncstore's RecordPendingCreateIntent and the rest), for executor tests
// that open no store. The executor requires both records; an executor-only
// test gets these by default.
type MemoryExecutorRecords struct {
	now func() time.Time

	// mu guards pending and moves; the executor writes them from its
	// workers and detached commits.
	mu      stdsync.Mutex
	pending map[string]syncdomain.PendingCreate
	moves   map[string]string
}

// NewMemoryExecutorRecords returns empty records whose intents are dated by
// now, the clock the store would date them by.
func NewMemoryExecutorRecords(now func() time.Time) *MemoryExecutorRecords {
	return &MemoryExecutorRecords{
		now:     now,
		pending: make(map[string]syncdomain.PendingCreate),
		moves:   make(map[string]string),
	}
}

// RecordPendingCreateIntent records that a create at pending.Path is about
// to be requested; a record already naming an item keeps its item and eTag.
func (r *MemoryExecutorRecords) RecordPendingCreateIntent(ctx context.Context, pending *syncdomain.PendingCreate) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("recording a pending create intent: %w", err)
	}
	if pending == nil || pending.Path == "" {
		return errors.New("recording a pending create intent: missing path")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.pending[pending.Path]
	row.Path = pending.Path
	row.DriveID, row.ParentID = pending.DriveID, pending.ParentID
	row.LocalSize, row.LocalHash = pending.LocalSize, pending.LocalHash
	row.IntendedAt = syncdomain.StoredTimeNano(r.now())
	r.pending[pending.Path] = row

	return nil
}

// RecordPendingCreate records a create whose empty item exists, keeping the
// time an earlier intent was recorded at.
func (r *MemoryExecutorRecords) RecordPendingCreate(ctx context.Context, pending *syncdomain.PendingCreate) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("recording a pending create: %w", err)
	}
	if pending == nil || pending.ItemID == "" || pending.Path == "" {
		return errors.New("recording a pending create: missing item ID or path")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	row := *pending
	row.IntendedAt = syncdomain.StoredTimeNano(r.now())
	if prior, ok := r.pending[pending.Path]; ok && prior.IntendedAt != 0 {
		row.IntendedAt = prior.IntendedAt
	}
	r.pending[pending.Path] = row

	return nil
}

// ClearPendingCreateIntent drops an intent at path that never named an item.
func (r *MemoryExecutorRecords) ClearPendingCreateIntent(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("clearing a pending create intent: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if row, ok := r.pending[path]; ok && row.ItemID == "" {
		delete(r.pending, path)
	}

	return nil
}

// ForgetRemovedPlaceholder drops the record naming itemID, and the intent at
// path when it never named an item.
func (r *MemoryExecutorRecords) ForgetRemovedPlaceholder(ctx context.Context, path, itemID string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("forgetting a removed placeholder: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for at, row := range r.pending {
		if (itemID != "" && row.ItemID == itemID) || (at == path && row.ItemID == "") {
			delete(r.pending, at)
		}
	}

	return nil
}

// PendingCreateAt returns the record at path, if there is one.
func (r *MemoryExecutorRecords) PendingCreateAt(ctx context.Context, path string) (syncdomain.PendingCreate, bool, error) {
	if err := ctx.Err(); err != nil {
		return syncdomain.PendingCreate{}, false, fmt.Errorf("reading a pending create: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.pending[path]

	return row, ok, nil
}

// RecordLocalMoveIntent records that the file at oldPath is about to be
// moved to newPath.
func (r *MemoryExecutorRecords) RecordLocalMoveIntent(ctx context.Context, oldPath, newPath string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("recording a local move intent: %w", err)
	}
	if oldPath == "" || newPath == "" {
		return errors.New("recording a local move intent: missing path")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.moves[oldPath] = newPath

	return nil
}

// ClearLocalMoveIntent forgets the move from oldPath.
func (r *MemoryExecutorRecords) ClearLocalMoveIntent(ctx context.Context, oldPath string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("clearing a local move intent: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.moves, oldPath)

	return nil
}

// LocalMoveIntents is every recorded local move, old path to new path.
func (r *MemoryExecutorRecords) LocalMoveIntents() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	moves := make(map[string]string, len(r.moves))
	for oldPath, newPath := range r.moves {
		moves[oldPath] = newPath
	}

	return moves
}
