package sync

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
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
	variant      string
	root         string
	observer     *LocalObserver
	watcher      *replayTrackingWatcher
	scopeChanges []syncscope.Change
	events       []ChangeEvent
}

type watchCaptureFixtureVariant struct {
	name    string
	records []watchCaptureRecord
}

type watchCaptureRecord struct {
	Scenario         string   `json:"scenario"`
	Step             string   `json:"step"`
	Path             string   `json:"path"`
	OpBits           uint32   `json:"op_bits"`
	OpNames          []string `json:"op_names"`
	TimeOffsetMicros int64    `json:"time_offset_micros"`
}

type watchCaptureScenario struct {
	setup func(root string) error
	steps map[string]func(root string) error
}

func (scenario watchCaptureScenario) SetUp(root string) error {
	if scenario.setup == nil {
		return nil
	}

	return scenario.setup(root)
}

func (scenario watchCaptureScenario) StepNames() []string {
	names := make([]string, 0, len(scenario.steps))
	for name := range scenario.steps {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (scenario watchCaptureScenario) RunStep(root, name string) error {
	run, ok := scenario.steps[name]
	if !ok {
		return fmt.Errorf("watch capture scenario: unknown step %q", name)
	}

	return run(root)
}

func replayWatchCaptureScenarioVariants(
	t *testing.T,
	scenarioName string,
	scopeConfig syncscope.Config,
) []replayOutcome {
	t.Helper()

	scenario := lookupWatchCaptureScenario(t, scenarioName)

	variants := loadWatchCaptureFixtures(t, scenarioName)
	outcomes := make([]replayOutcome, 0, len(variants))
	for _, variant := range variants {
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

		eventsCh := make(chan ChangeEvent, 64)
		for _, stepName := range scenario.StepNames() {
			require.NoError(t, scenario.RunStep(root, stepName))

			for _, record := range variant.records {
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
			variant:      variant.name,
			root:         root,
			observer:     observer,
			watcher:      watcher,
			scopeChanges: drainScopeChanges(scopeChangesCh),
			events:       drainReplayEvents(eventsCh),
		}
		assert.GreaterOrEqual(t, observer.currentScopeGeneration(), initialGeneration)
		outcomes = append(outcomes, outcome)
	}

	return outcomes
}

func loadWatchCaptureFixtures(t *testing.T, scenarioName string) []watchCaptureFixtureVariant {
	t.Helper()

	pattern := filepath.Join(
		"testdata",
		"watch_capture",
		runtime.GOOS,
		scenarioName,
		"*.json",
	)
	fixturePaths, err := filepath.Glob(pattern)
	if err != nil {
		require.NoError(t, err)
	}
	if len(fixturePaths) == 0 {
		t.Skipf("watch-capture fixture missing for %s on %s", scenarioName, runtime.GOOS)
	}

	variants := make([]watchCaptureFixtureVariant, 0, len(fixturePaths))
	for _, fixturePath := range fixturePaths {
		raw, err := localpath.ReadFile(fixturePath)
		require.NoError(t, err)

		var records []watchCaptureRecord
		require.NoError(t, json.Unmarshal(raw, &records))
		require.NotEmpty(t, records)
		variants = append(variants, watchCaptureFixtureVariant{
			name:    strings.TrimSuffix(filepath.Base(fixturePath), filepath.Ext(fixturePath)),
			records: records,
		})
	}

	return variants
}

func lookupWatchCaptureScenario(t *testing.T, name string) watchCaptureScenario {
	t.Helper()

	switch name {
	case "marker_create":
		return watchCaptureMarkerCreateScenario()
	case "marker_delete":
		return watchCaptureMarkerDeleteScenario()
	case "marker_rename":
		return watchCaptureMarkerRenameScenario()
	case "marker_parent_rename":
		return watchCaptureMarkerParentRenameScenario()
	case "marker_move_between_dirs":
		return watchCaptureMarkerMoveBetweenDirsScenario()
	case "dir_move_into_scope":
		return watchCaptureMoveDirScenario("parking/album", "docs/album", "move_into_scope", "move into scope")
	case "dir_move_out_of_scope":
		return watchCaptureMoveDirScenario("docs/album", "parking/album", "move_out_of_scope", "move out of scope")
	default:
		t.Fatalf("unknown watch capture scenario %q", name)
		return watchCaptureScenario{}
	}
}

func watchCaptureWriteFile(root, relPath, content string) error {
	fullPath := filepath.Join(root, filepath.FromSlash(relPath))
	if err := localpath.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(fullPath), err)
	}
	if err := localpath.WriteFile(fullPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write file %q: %w", fullPath, err)
	}

	return nil
}

func watchCaptureEnsureDir(root, relPath string) error {
	if err := localpath.MkdirAll(filepath.Join(root, filepath.FromSlash(relPath)), 0o700); err != nil {
		return fmt.Errorf("mkdir %q: %w", relPath, err)
	}

	return nil
}

func watchCaptureMarkerCreateScenario() watchCaptureScenario {
	return watchCaptureScenario{
		setup: func(root string) error {
			return watchCaptureEnsureDir(root, "blocked/nested")
		},
		steps: map[string]func(root string) error{
			"create_marker": func(root string) error {
				return watchCaptureWriteFile(root, "blocked/.odignore", "marker")
			},
		},
	}
}

func watchCaptureMarkerDeleteScenario() watchCaptureScenario {
	return watchCaptureScenario{
		setup: func(root string) error {
			if err := watchCaptureEnsureDir(root, "blocked/nested"); err != nil {
				return err
			}
			if err := watchCaptureWriteFile(root, "blocked/.odignore", "marker"); err != nil {
				return err
			}
			return watchCaptureWriteFile(root, "blocked/nested/keep.txt", "keep")
		},
		steps: map[string]func(root string) error{
			"delete_marker": func(root string) error {
				if err := localpath.Remove(filepath.Join(root, "blocked", ".odignore")); err != nil {
					return fmt.Errorf("remove marker: %w", err)
				}
				return nil
			},
		},
	}
}

func watchCaptureMarkerRenameScenario() watchCaptureScenario {
	return watchCaptureScenario{
		setup: func(root string) error {
			if err := watchCaptureEnsureDir(root, "blocked/nested"); err != nil {
				return err
			}
			return watchCaptureWriteFile(root, "blocked/pending.marker", "marker")
		},
		steps: map[string]func(root string) error{
			"rename_to_marker": func(root string) error {
				if err := localpath.Rename(
					filepath.Join(root, "blocked", "pending.marker"),
					filepath.Join(root, "blocked", ".odignore"),
				); err != nil {
					return fmt.Errorf("rename to marker: %w", err)
				}
				return nil
			},
		},
	}
}

func watchCaptureMarkerParentRenameScenario() watchCaptureScenario {
	return watchCaptureScenario{
		setup: func(root string) error {
			if err := watchCaptureEnsureDir(root, "parent/blocked/nested"); err != nil {
				return err
			}
			if err := watchCaptureWriteFile(root, "parent/blocked/.odignore", "marker"); err != nil {
				return err
			}
			return watchCaptureWriteFile(root, "parent/blocked/nested/keep.txt", "keep")
		},
		steps: map[string]func(root string) error{
			"rename_parent": func(root string) error {
				if err := localpath.Rename(
					filepath.Join(root, "parent"),
					filepath.Join(root, "renamed"),
				); err != nil {
					return fmt.Errorf("rename parent: %w", err)
				}
				return nil
			},
		},
	}
}

func watchCaptureMarkerMoveBetweenDirsScenario() watchCaptureScenario {
	return watchCaptureScenario{
		setup: func(root string) error {
			if err := watchCaptureEnsureDir(root, "left/blocked/nested"); err != nil {
				return err
			}
			if err := watchCaptureEnsureDir(root, "right"); err != nil {
				return err
			}
			if err := watchCaptureWriteFile(root, "left/blocked/.odignore", "marker"); err != nil {
				return err
			}
			return watchCaptureWriteFile(root, "left/blocked/nested/keep.txt", "keep")
		},
		steps: map[string]func(root string) error{
			"move_marker_dir": func(root string) error {
				if err := localpath.Rename(
					filepath.Join(root, "left", "blocked"),
					filepath.Join(root, "right", "blocked"),
				); err != nil {
					return fmt.Errorf("move marker directory: %w", err)
				}
				return nil
			},
		},
	}
}

func watchCaptureMoveDirScenario(sourceDir, destDir, stepName, moveLabel string) watchCaptureScenario {
	sourceFile := filepath.ToSlash(filepath.Join(sourceDir, "photo.jpg"))
	return watchCaptureScenario{
		setup: func(root string) error {
			if err := watchCaptureEnsureDir(root, sourceDir); err != nil {
				return err
			}
			if err := watchCaptureEnsureDir(root, filepath.Dir(destDir)); err != nil {
				return err
			}
			return watchCaptureWriteFile(root, sourceFile, "img")
		},
		steps: map[string]func(root string) error{
			stepName: func(root string) error {
				if err := localpath.Rename(
					filepath.Join(root, filepath.FromSlash(sourceDir)),
					filepath.Join(root, filepath.FromSlash(destDir)),
				); err != nil {
					return fmt.Errorf("%s: %w", moveLabel, err)
				}
				return nil
			},
		},
	}
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

func drainReplayEvents(ch <-chan ChangeEvent) []ChangeEvent {
	events := make([]ChangeEvent, 0)
	for {
		select {
		case event := <-ch:
			events = append(events, event)
		default:
			return events
		}
	}
}

func assertSingleMarkerScopeTransition(
	t *testing.T,
	outcomes []replayOutcome,
	wantOld []string,
	wantNew []string,
) {
	t.Helper()

	for _, outcome := range outcomes {
		t.Run(outcome.variant, func(t *testing.T) {
			require.Len(t, outcome.scopeChanges, 1)
			assert.Equal(t, wantOld, outcome.scopeChanges[0].Old.MarkerDirs())
			assert.Equal(t, wantNew, outcome.scopeChanges[0].New.MarkerDirs())
			assert.Equal(t, int64(2), outcome.observer.currentScopeGeneration())
		})
	}
}

// Validates: R-2.4.4
func TestReplayWatchCapture_MarkerRenamePublishesSingleScopeChange(t *testing.T) {
	t.Parallel()

	outcomes := replayWatchCaptureScenarioVariants(t, "marker_rename", syncscope.Config{
		IgnoreMarker: ".odignore",
	})

	for _, outcome := range outcomes {
		t.Run(outcome.variant, func(t *testing.T) {
			require.Len(t, outcome.scopeChanges, 1)
			assert.Equal(t, []string{"blocked"}, outcome.scopeChanges[0].New.MarkerDirs())
			assert.Equal(t, int64(2), outcome.observer.currentScopeGeneration())
			assert.Contains(t, outcome.observer.watchedDirs, filepath.Join(outcome.root, "blocked"))
			assert.NotContains(t, outcome.observer.watchedDirs, filepath.Join(outcome.root, "blocked", "nested"))
			assert.True(t, outcome.watcher.removedContains(filepath.Join(outcome.root, "blocked", "nested")))
		})
	}
}

// Validates: R-2.4.4
func TestReplayWatchCapture_MarkerCreatePublishesSingleScopeChange(t *testing.T) {
	t.Parallel()

	outcomes := replayWatchCaptureScenarioVariants(t, "marker_create", syncscope.Config{
		IgnoreMarker: ".odignore",
	})

	for _, outcome := range outcomes {
		t.Run(outcome.variant, func(t *testing.T) {
			require.Len(t, outcome.scopeChanges, 1)
			assert.Empty(t, outcome.scopeChanges[0].Old.MarkerDirs())
			assert.Equal(t, []string{"blocked"}, outcome.scopeChanges[0].New.MarkerDirs())
			assert.Equal(t, int64(2), outcome.observer.currentScopeGeneration())
			assert.Contains(t, outcome.observer.watchedDirs, filepath.Join(outcome.root, "blocked"))
			assert.NotContains(t, outcome.observer.watchedDirs, filepath.Join(outcome.root, "blocked", "nested"))
			assert.True(t, outcome.watcher.removedContains(filepath.Join(outcome.root, "blocked", "nested")))
		})
	}
}

// Validates: R-2.4.4
func TestReplayWatchCapture_MarkerDeleteRestoresDescendantWatches(t *testing.T) {
	t.Parallel()

	outcomes := replayWatchCaptureScenarioVariants(t, "marker_delete", syncscope.Config{
		IgnoreMarker: ".odignore",
	})

	for _, outcome := range outcomes {
		t.Run(outcome.variant, func(t *testing.T) {
			require.Len(t, outcome.scopeChanges, 1)
			assert.Empty(t, outcome.scopeChanges[0].New.MarkerDirs())
			assert.Equal(t, int64(2), outcome.observer.currentScopeGeneration())
			assert.Contains(t, outcome.observer.watchedDirs, filepath.Join(outcome.root, "blocked", "nested"))
		})
	}
}

// Validates: R-2.4.4
func TestReplayWatchCapture_MarkerParentRenamePublishesSingleScopeChange(t *testing.T) {
	t.Parallel()

	outcomes := replayWatchCaptureScenarioVariants(t, "marker_parent_rename", syncscope.Config{
		IgnoreMarker: ".odignore",
	})
	assertSingleMarkerScopeTransition(t, outcomes, []string{"parent/blocked"}, []string{"renamed/blocked"})
}

// Validates: R-2.4.4
func TestReplayWatchCapture_MarkerMoveBetweenDirsPublishesSingleScopeChange(t *testing.T) {
	t.Parallel()

	outcomes := replayWatchCaptureScenarioVariants(t, "marker_move_between_dirs", syncscope.Config{
		IgnoreMarker: ".odignore",
	})
	assertSingleMarkerScopeTransition(t, outcomes, []string{"left/blocked"}, []string{"right/blocked"})
}

// Validates: R-2.4.5
func TestReplayWatchCapture_DirMoveIntoScopeRemainsDataOnly(t *testing.T) {
	t.Parallel()

	outcomes := replayWatchCaptureScenarioVariants(t, "dir_move_into_scope", syncscope.Config{
		SyncPaths: []string{"/docs"},
	})

	for _, outcome := range outcomes {
		t.Run(outcome.variant, func(t *testing.T) {
			assert.Empty(t, outcome.scopeChanges)
			assert.Equal(t, int64(1), outcome.observer.currentScopeGeneration())
			require.NotEmpty(t, outcome.events)
			for _, event := range outcome.events {
				assert.NotContains(t, event.Path, "parking/album")
			}
		})
	}
}

// Validates: R-2.4.5
func TestReplayWatchCapture_DirMoveOutOfScopeRemainsDataOnly(t *testing.T) {
	t.Parallel()

	outcomes := replayWatchCaptureScenarioVariants(t, "dir_move_out_of_scope", syncscope.Config{
		SyncPaths: []string{"/docs"},
	})

	for _, outcome := range outcomes {
		t.Run(outcome.variant, func(t *testing.T) {
			assert.Empty(t, outcome.scopeChanges)
			assert.Equal(t, int64(1), outcome.observer.currentScopeGeneration())
			for _, event := range outcome.events {
				assert.NotContains(t, event.Path, "parking/album")
			}
		})
	}
}
