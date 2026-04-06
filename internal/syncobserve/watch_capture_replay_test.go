package syncobserve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	stdsync "sync"
	"testing"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/devtool"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type replayTrackingWatcher struct {
	events  chan fsnotify.Event
	errs    chan error
	mu      stdsync.Mutex
	added   []string
	removed []string
}

func newReplayTrackingWatcher() *replayTrackingWatcher {
	return &replayTrackingWatcher{
		events: make(chan fsnotify.Event, 1),
		errs:   make(chan error, 1),
	}
}

func (w *replayTrackingWatcher) Add(name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.added = append(w.added, filepath.Clean(name))
	return nil
}

func (w *replayTrackingWatcher) Remove(name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.removed = append(w.removed, filepath.Clean(name))
	return nil
}

func (w *replayTrackingWatcher) Close() error                  { return nil }
func (w *replayTrackingWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *replayTrackingWatcher) Errors() <-chan error          { return w.errs }

func (w *replayTrackingWatcher) removedContains(path string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	cleanPath := filepath.Clean(path)
	for _, removed := range w.removed {
		if removed == cleanPath {
			return true
		}
	}

	return false
}

type replayOutcome struct {
	root         string
	observer     *LocalObserver
	watcher      *replayTrackingWatcher
	scopeChanges []syncscope.Change
	events       []synctypes.ChangeEvent
}

func replayWatchCaptureScenario(
	t *testing.T,
	scenarioName string,
	scopeConfig syncscope.Config,
) replayOutcome {
	t.Helper()

	scenario, err := devtool.LookupWatchCaptureScenario(scenarioName)
	require.NoError(t, err)

	records := loadWatchCaptureFixture(t, scenarioName)
	root := t.TempDir()
	require.NoError(t, scenario.SetUp(root))

	watcher := newReplayTrackingWatcher()
	observer := newWatchTestObserver(t, watcher, watchObserverTestOptions{})
	tree := mustOpenSyncTree(t, root)

	snapshot, err := observer.BuildScopeSnapshot(t.Context(), tree, scopeConfig)
	require.NoError(t, err)
	observer.SetScopeSnapshot(snapshot)

	scopeChangesCh := make(chan syncscope.Change, 16)
	observer.SetScopeChangeChannel(scopeChangesCh)

	require.NoError(t, observer.AddWatchesRecursive(t.Context(), watcher, tree))
	initialGeneration := observer.currentScopeGeneration()

	eventsCh := make(chan synctypes.ChangeEvent, 64)
	for _, stepName := range scenario.StepNames() {
		require.NoError(t, scenario.RunStep(root, stepName))

		for _, record := range records {
			if record.Step != stepName {
				continue
			}
			observer.HandleFsEvent(
				t.Context(),
				fsnotify.Event{
					Name: replayRecordPath(root, record.Path),
					Op:   fsnotify.Op(record.OpBits),
				},
				watcher,
				tree,
				eventsCh,
			)
		}
	}

	outcome := replayOutcome{
		root:         root,
		observer:     observer,
		watcher:      watcher,
		scopeChanges: drainScopeChanges(scopeChangesCh),
		events:       drainReplayEvents(eventsCh),
	}
	assert.GreaterOrEqual(t, observer.currentScopeGeneration(), initialGeneration)
	return outcome
}

func loadWatchCaptureFixture(t *testing.T, scenarioName string) []devtool.WatchCaptureRecord {
	t.Helper()

	fixturePath := filepath.Join(
		"testdata",
		"watch_capture",
		runtime.GOOS,
		scenarioName+".json",
	)
	raw, err := localpath.ReadFile(fixturePath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("watch-capture fixture missing for %s on %s", scenarioName, runtime.GOOS)
		}
		require.NoError(t, err)
	}

	var records []devtool.WatchCaptureRecord
	require.NoError(t, json.Unmarshal(raw, &records))
	require.NotEmpty(t, records)
	return records
}

func replayRecordPath(root, recordPath string) string {
	if recordPath == "." {
		return root
	}

	return filepath.Join(root, filepath.FromSlash(recordPath))
}

func drainScopeChanges(ch <-chan syncscope.Change) []syncscope.Change {
	changes := make([]syncscope.Change, 0)
	for {
		select {
		case change := <-ch:
			changes = append(changes, change)
		default:
			return changes
		}
	}
}

func drainReplayEvents(ch <-chan synctypes.ChangeEvent) []synctypes.ChangeEvent {
	events := make([]synctypes.ChangeEvent, 0)
	for {
		select {
		case event := <-ch:
			events = append(events, event)
		default:
			return events
		}
	}
}

// Validates: R-2.4.4
func TestReplayWatchCapture_MarkerRenamePublishesSingleScopeChange(t *testing.T) {
	t.Parallel()

	outcome := replayWatchCaptureScenario(t, "marker_rename", syncscope.Config{
		IgnoreMarker: ".odignore",
	})

	require.Len(t, outcome.scopeChanges, 1)
	assert.Equal(t, []string{"blocked"}, outcome.scopeChanges[0].New.MarkerDirs())
	assert.Equal(t, int64(2), outcome.observer.currentScopeGeneration())
	assert.Contains(t, outcome.observer.watchedDirs, filepath.Join(outcome.root, "blocked"))
	assert.NotContains(t, outcome.observer.watchedDirs, filepath.Join(outcome.root, "blocked", "nested"))
	assert.True(t, outcome.watcher.removedContains(filepath.Join(outcome.root, "blocked", "nested")))
}

// Validates: R-2.4.4
func TestReplayWatchCapture_MarkerDeleteRestoresDescendantWatches(t *testing.T) {
	t.Parallel()

	outcome := replayWatchCaptureScenario(t, "marker_delete", syncscope.Config{
		IgnoreMarker: ".odignore",
	})

	require.Len(t, outcome.scopeChanges, 1)
	assert.Empty(t, outcome.scopeChanges[0].New.MarkerDirs())
	assert.Equal(t, int64(2), outcome.observer.currentScopeGeneration())
	assert.Contains(t, outcome.observer.watchedDirs, filepath.Join(outcome.root, "blocked", "nested"))
}

// Validates: R-2.4.4
func TestReplayWatchCapture_MarkerParentRenamePublishesSingleScopeChange(t *testing.T) {
	t.Parallel()

	outcome := replayWatchCaptureScenario(t, "marker_parent_rename", syncscope.Config{
		IgnoreMarker: ".odignore",
	})

	require.Len(t, outcome.scopeChanges, 1)
	assert.Equal(t, []string{"parent/blocked"}, outcome.scopeChanges[0].Old.MarkerDirs())
	assert.Equal(t, []string{"renamed/blocked"}, outcome.scopeChanges[0].New.MarkerDirs())
	assert.Equal(t, int64(2), outcome.observer.currentScopeGeneration())
}

// Validates: R-2.4.5
func TestReplayWatchCapture_DirMoveIntoScopeRemainsDataOnly(t *testing.T) {
	t.Parallel()

	outcome := replayWatchCaptureScenario(t, "dir_move_into_scope", syncscope.Config{
		SyncPaths: []string{"/docs"},
	})

	assert.Empty(t, outcome.scopeChanges)
	assert.Equal(t, int64(1), outcome.observer.currentScopeGeneration())
	require.NotEmpty(t, outcome.events)
	for _, event := range outcome.events {
		assert.NotContains(t, event.Path, "parking/album")
	}
}

// Validates: R-2.4.5
func TestReplayWatchCapture_DirMoveOutOfScopeRemainsDataOnly(t *testing.T) {
	t.Parallel()

	outcome := replayWatchCaptureScenario(t, "dir_move_out_of_scope", syncscope.Config{
		SyncPaths: []string{"/docs"},
	})

	assert.Empty(t, outcome.scopeChanges)
	assert.Equal(t, int64(1), outcome.observer.currentScopeGeneration())
	for _, event := range outcome.events {
		assert.NotContains(t, event.Path, "parking/album")
	}
}
