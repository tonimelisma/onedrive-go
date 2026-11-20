package sync

import (
	stdsync "sync"

	"github.com/fsnotify/fsnotify"
)

// recordingFsWatcher extends mockFsWatcher to record Remove() calls.
// This allows tests to verify which paths were passed to watcher.Remove().
type recordingFsWatcher struct {
	mockFsWatcher
	mu        stdsync.Mutex
	removed   []string
	removeErr error // injectable error for Remove()
}

func newRecordingFsWatcher() *recordingFsWatcher {
	return &recordingFsWatcher{
		mockFsWatcher: mockFsWatcher{
			events: make(chan fsnotify.Event, 10),
			errs:   make(chan error, 10),
		},
	}
}

func (r *recordingFsWatcher) Remove(name string) error {
	r.mu.Lock()
	r.removed = append(r.removed, name)
	err := r.removeErr
	r.mu.Unlock()
	return err
}

func (r *recordingFsWatcher) getRemovedPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]string, len(r.removed))
	copy(result, r.removed)
	return result
}
