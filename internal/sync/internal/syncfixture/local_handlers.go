package syncfixture

import (
	stdsync "sync"

	"github.com/fsnotify/fsnotify"
)

// MockFsWatcher implements FsWatcher with injectable channels for testing.
// Add() is a no-op — it silently accepts the real directory paths that
// addWatchesRecursive passes during Watch() startup. This is intentional:
// the backoff/safety-scan tests only exercise watchLoop event handling,
// not the watch-setup walk.
type MockFsWatcher struct {
	EventsCh chan fsnotify.Event
	Errs     chan error
	AddOne   stdsync.Once
	AddCh    chan struct{}
	AddHook  func()
	closeOne stdsync.Once // idempotent Close prevents panic on double-close
}

func (m *MockFsWatcher) Add(string) error {
	if m.AddCh == nil {
		m.AddCh = make(chan struct{})
	}
	m.AddOne.Do(func() {
		if m.AddHook != nil {
			m.AddHook()
		}
		close(m.AddCh)
	})
	return nil
}

func (m *MockFsWatcher) Remove(string) error { return nil }

func (m *MockFsWatcher) Events() <-chan fsnotify.Event { return m.EventsCh }

func (m *MockFsWatcher) Errors() <-chan error { return m.Errs }

func (m *MockFsWatcher) Added() <-chan struct{} {
	if m.AddCh == nil {
		m.AddCh = make(chan struct{})
	}

	return m.AddCh
}

func (m *MockFsWatcher) Close() error {
	m.closeOne.Do(func() { close(m.EventsCh); close(m.Errs) })
	return nil
}
