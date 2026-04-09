// Package syncobserve watches and scans the local filesystem for sync changes.
// It sits between observers and planner in the sync pipeline: observers
// produce []synctypes.ChangeEvent, Buffer groups them into []synctypes.PathChanges,
// planner consumes the grouped view. Thread-safe for concurrent observer output.
package syncobserve

import (
	"context"
	"log/slog"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Buffer collects ChangeEvents from both observers and groups them
// by path into PathChanges values for the planner. All methods are
// safe for concurrent use.
type Buffer struct {
	mu      sync.Mutex // guards pending and notify
	pending map[string]*synctypes.PathChanges
	notify  chan struct{} // signaled on Add/AddAll when FlushDebounced is active; nil otherwise
	logger  *slog.Logger
}

// NewBuffer creates an empty Buffer ready to accept events.
func NewBuffer(logger *slog.Logger) *Buffer {
	logger.Debug("buffer created")

	return &Buffer{
		pending: make(map[string]*synctypes.PathChanges),
		logger:  logger,
	}
}

// Add records a single event in the buffer, routing it to the correct
// path group and retaining only the planner-relevant per-path history.
// Thread-safe. Takes a pointer to avoid copying the 192-byte ChangeEvent
// struct on each call.
func (b *Buffer) Add(ev *synctypes.ChangeEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.addLocked(ev)
}

// AddAll appends a batch of events under a single lock acquisition.
// This avoids per-event lock overhead when processing the full output
// of a one-shot observer (thousands of events from FullDelta or FullScan).
func (b *Buffer) AddAll(events []synctypes.ChangeEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for i := range events {
		b.addLocked(&events[i])
	}
}

// FlushImmediate returns all buffered PathChanges sorted by path
// (deterministic ordering for the planner) and clears the buffer.
// Returns nil for an empty buffer.
func (b *Buffer) FlushImmediate() []synctypes.PathChanges {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.pending) == 0 {
		b.logger.Debug("buffer flushed (empty)")
		return nil
	}

	result := make([]synctypes.PathChanges, 0, len(b.pending))
	for _, pc := range b.pending {
		result = append(result, *pc)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Path < result[j].Path
	})

	count := len(b.pending)
	b.pending = make(map[string]*synctypes.PathChanges)

	b.logger.Info("buffer flushed", "paths", count)

	return result
}

// Len returns the number of distinct paths currently buffered.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.pending)
}

// FlushDebounced returns a channel that emits batches of PathChanges after
// a debounce window elapses with no new events. Each batch is equivalent to
// calling FlushImmediate(). The debounce timer resets every time Add() or
// AddAll() is called. The output channel is closed when the context is
// canceled; any remaining events are drained in a final batch.
func (b *Buffer) FlushDebounced(ctx context.Context, debounce time.Duration) <-chan []synctypes.PathChanges {
	out := make(chan []synctypes.PathChanges, 1)

	b.mu.Lock()
	if b.notify != nil {
		b.mu.Unlock()
		panic("sync: FlushDebounced called twice on the same Buffer")
	}

	b.notify = make(chan struct{}, 1)
	b.mu.Unlock()

	go b.debounceLoop(ctx, debounce, out)

	return out
}

// debounceLoop is the goroutine driving FlushDebounced. It waits for new-event
// signals, resets the debounce timer, and flushes when the timer expires.
func (b *Buffer) debounceLoop(ctx context.Context, debounce time.Duration, out chan<- []synctypes.PathChanges) {
	defer close(out)

	timer := time.NewTimer(debounce)
	timer.Stop() // start idle — no events yet
	defer timer.Stop()

	timerActive := false

	for {
		select {
		case <-ctx.Done():
			// Drain remaining events. Use non-blocking send because
			// the consumer may have stopped reading (B-103).
			if batch := b.FlushImmediate(); batch != nil {
				select {
				case out <- batch:
				default:
					b.logger.Warn("final drain discarded: output channel full",
						slog.Int("paths", len(batch)),
					)
				}
			}

			return

		case _, ok := <-b.notify:
			if !ok {
				return
			}

			// New event arrived — reset the debounce timer.
			if !timer.Stop() && timerActive {
				<-timer.C
			}

			timer.Reset(debounce)
			timerActive = true

		case <-timer.C:
			timerActive = false

			if batch := b.FlushImmediate(); batch != nil {
				// Blocking send is intentional backpressure (B-128). When the
				// consumer is slow, events accumulate in the buffer rather than
				// being dropped. This is correct: the debounce timer won't
				// re-fire until the consumer reads the batch, at which point
				// the buffer flushes all accumulated events as one batch.
				select {
				case out <- batch:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// signalNew sends a non-blocking notification to the debounce goroutine.
// Called from addLocked while the mutex is held. The notify channel is nil
// until FlushDebounced() is called, so one-shot mode (FlushImmediate only)
// pays no cost.
func (b *Buffer) signalNew() {
	if b.notify == nil {
		return
	}

	select {
	case b.notify <- struct{}{}:
	default:
		// Already signaled — debounce goroutine hasn't consumed yet.
	}
}

// addLocked is the internal add logic called while the mutex is held.
// It routes the event to the correct PathChanges entry and handles
// move dual-keying: a ChangeMove with OldPath produces a synthetic
// ChangeDelete at the old path so the planner sees both paths.
func (b *Buffer) addLocked(ev *synctypes.ChangeEvent) {
	pc := b.getOrCreate(ev.Path)
	b.routeEvent(pc, ev)

	b.logger.Debug("event added",
		"path", ev.Path,
		"source", ev.Source.String(),
		"type", ev.Type.String(),
	)

	// Move dual-keying: ensure the old path enters the planner so
	// stale baseline entries get cleaned up (no orphaned records).
	//
	// Currently only RemoteObserver produces ChangeMove events (from
	// delta's parentReference changes). LocalObserver detects moves via
	// hash correlation in the planner, not ChangeMove events. This
	// dual-keying stays correct if watch-mode local observation later emits
	// rename events directly instead of relying only on planner correlation.
	if ev.Type == synctypes.ChangeMove && ev.OldPath != "" {
		synthetic := synctypes.ChangeEvent{
			Source:    ev.Source,
			Type:      synctypes.ChangeDelete,
			Path:      ev.OldPath,
			ItemID:    ev.ItemID,
			ParentID:  ev.ParentID,
			DriveID:   ev.DriveID,
			ItemType:  ev.ItemType,
			Name:      path.Base(ev.OldPath),
			IsDeleted: true,
		}

		oldPC := b.getOrCreate(ev.OldPath)
		b.routeEvent(oldPC, &synthetic)

		b.logger.Debug("synthetic delete for move old path",
			"old_path", ev.OldPath,
			"source", ev.Source.String(),
		)
	}

	b.signalNew()
}

// getOrCreate returns the PathChanges for the given path, creating it
// if it does not yet exist in the pending map.
func (b *Buffer) getOrCreate(p string) *synctypes.PathChanges {
	pc, ok := b.pending[p]
	if !ok {
		pc = &synctypes.PathChanges{Path: p}
		b.pending[p] = pc
	}

	return pc
}

// routeEvent records the event in the correct source slice within PathChanges.
// Local history keeps only the latest event. Remote history keeps the latest
// event plus one move marker when needed so the planner can still detect
// remote moves after a follow-up remote modify in the same debounce window.
func (b *Buffer) routeEvent(pc *synctypes.PathChanges, ev *synctypes.ChangeEvent) {
	switch ev.Source {
	case synctypes.SourceRemote:
		pc.RemoteEvents = retainRemoteEvents(pc.RemoteEvents, ev)
	case synctypes.SourceLocal:
		pc.LocalEvents = []synctypes.ChangeEvent{*ev}
	}
}

func retainRemoteEvents(existing []synctypes.ChangeEvent, ev *synctypes.ChangeEvent) []synctypes.ChangeEvent {
	if ev.Type == synctypes.ChangeMove {
		return []synctypes.ChangeEvent{*ev}
	}

	if len(existing) > 0 && existing[0].Type == synctypes.ChangeMove {
		return []synctypes.ChangeEvent{existing[0], *ev}
	}

	return []synctypes.ChangeEvent{*ev}
}
