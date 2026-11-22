package syncsched

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// HasWindowForTest reports whether scope detection holds a window for key.
func (ss *ScopeState) HasWindowForTest(key syncdomain.ScopeKey) bool {
	_, ok := ss.windows[key]
	return ok
}

// WindowPathCountForTest is how many distinct paths the window for key holds
// at now; open is false when there is no window.
func (ss *ScopeState) WindowPathCountForTest(key syncdomain.ScopeKey, now time.Time) (count int, open bool) {
	window, ok := ss.windows[key]
	if !ok {
		return 0, false
	}

	return window.uniqueCount(now), true
}

// HasWindowForTest reports whether the scheduler's scope detection holds a
// window for key.
func (s *Scheduler) HasWindowForTest(key syncdomain.ScopeKey) bool {
	if s.windows == nil {
		return false
	}
	_, ok := s.windows.windows[key]

	return ok
}

// WindowPathCountForTest is how many distinct paths the scheduler's window
// for key holds at now; open is false when there is no window.
func (s *Scheduler) WindowPathCountForTest(key syncdomain.ScopeKey, now time.Time) (count int, open bool) {
	if s.windows == nil {
		return 0, false
	}
	window, ok := s.windows.windows[key]
	if !ok {
		return 0, false
	}

	return window.uniqueCount(now), true
}

// AddForTest registers one action and its dependencies in one call
// (DepGraph.Add), so a test builds a graph without a plan. It returns the
// action when it is ready at once.
func (s *Scheduler) AddForTest(action *syncdomain.Action, id int64, depIDs []int64) *syncdomain.TrackedAction {
	return s.graph.Add(action, id, depIDs)
}
