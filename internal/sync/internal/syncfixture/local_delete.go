package syncfixture

import (
	stdsync "sync"

	"github.com/fsnotify/fsnotify"
)

// recordingFsWatcher extends mockFsWatcher to record Remove() calls.
// This allows tests to verify which paths were passed to watcher.Remove().
type recordingFsWatcher struct {
	MockFsWatcher
	mu        stdsync.Mutex // guards removed and removeErr
	removed   []string
	removeErr error // injectable error for Remove()
}

func NewRecordingFsWatcher() *recordingFsWatcher {
	return &recordingFsWatcher{
		MockFsWatcher: MockFsWatcher{
			EventsCh: make(chan fsnotify.Event, 10),
			Errs:     make(chan error, 10),
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

func (r *recordingFsWatcher) GetRemovedPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]string, len(r.removed))
	copy(result, r.removed)
	return result
}
