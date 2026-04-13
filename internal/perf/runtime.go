package perf

import (
	"context"
	"sync"
)

type Runtime struct {
	collector *Collector
	captureMu sync.Mutex
	capturing bool

	mu     sync.RWMutex
	drives map[string]*Collector
}

func NewRuntime(parent *Collector) *Runtime {
	return &Runtime{
		collector: NewCollector(parent),
		drives:    make(map[string]*Collector),
	}
}

func (r *Runtime) Collector() *Collector {
	if r == nil {
		return nil
	}

	return r.collector
}

func (r *Runtime) RegisterDrive(canonicalID string) *Collector {
	if r == nil {
		return nil
	}

	collector := NewCollector(r.collector)

	r.mu.Lock()
	r.drives[canonicalID] = collector
	r.mu.Unlock()

	return collector
}

func (r *Runtime) RemoveDrive(canonicalID string) {
	if r == nil {
		return
	}

	r.mu.Lock()
	delete(r.drives, canonicalID)
	r.mu.Unlock()
}

func (r *Runtime) SnapshotByDrive() map[string]Snapshot {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	snapshots := make(map[string]Snapshot, len(r.drives))
	for canonicalID, collector := range r.drives {
		snapshots[canonicalID] = collector.Snapshot()
	}

	return snapshots
}

func (r *Runtime) AggregateSnapshot() Snapshot {
	if r == nil || r.collector == nil {
		return Snapshot{}
	}

	return r.collector.Snapshot()
}

func (r *Runtime) Capture(ctx context.Context, opts CaptureOptions) (CaptureResult, error) {
	if r == nil {
		return CaptureResult{}, ErrCaptureUnavailable
	}

	r.captureMu.Lock()
	if r.capturing {
		r.captureMu.Unlock()
		return CaptureResult{}, ErrCaptureInProgress
	}
	r.capturing = true
	r.captureMu.Unlock()
	defer func() {
		r.captureMu.Lock()
		r.capturing = false
		r.captureMu.Unlock()
	}()

	aggregate := r.AggregateSnapshot()
	return captureBundle(ctx, opts, &aggregate, r.SnapshotByDrive())
}
