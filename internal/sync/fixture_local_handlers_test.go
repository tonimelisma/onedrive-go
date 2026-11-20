package sync

import (
	stdsync "sync"

	"github.com/fsnotify/fsnotify"
)

// mockFsWatcher implements FsWatcher with injectable channels for testing.
// Add() is a no-op — it silently accepts the real directory paths that
// addWatchesRecursive passes during Watch() startup. This is intentional:
// the backoff/safety-scan tests only exercise watchLoop event handling,
// not the watch-setup walk.
type mockFsWatcher struct {
	events   chan fsnotify.Event
	errs     chan error
	addOne   stdsync.Once
	addCh    chan struct{}
	addHook  func()
	closeOne stdsync.Once // idempotent Close prevents panic on double-close
}

func (m *mockFsWatcher) Add(string) error {
	if m.addCh == nil {
		m.addCh = make(chan struct{})
	}
	m.addOne.Do(func() {
		if m.addHook != nil {
			m.addHook()
		}
		close(m.addCh)
	})
	return nil
}

func (m *mockFsWatcher) Remove(string) error { return nil }

func (m *mockFsWatcher) Events() <-chan fsnotify.Event { return m.events }

func (m *mockFsWatcher) Errors() <-chan error { return m.errs }

func (m *mockFsWatcher) Added() <-chan struct{} {
	if m.addCh == nil {
		m.addCh = make(chan struct{})
	}

	return m.addCh
}

func (m *mockFsWatcher) Close() error {
	m.closeOne.Do(func() { close(m.events); close(m.errs) })
	return nil
}
