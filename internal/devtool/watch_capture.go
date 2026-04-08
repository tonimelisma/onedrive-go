package devtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

const (
	DefaultWatchCaptureSettle = 250 * time.Millisecond
	watchCaptureRecordReserve = 4
	watchCaptureDirPerm       = 0o700
	watchCaptureFilePerm      = 0o600
)

type WatchCaptureOptions struct {
	Scenario string
	JSON     bool
	Repeat   int
	Settle   time.Duration
	Stdout   io.Writer
}

type WatchCaptureRecord struct {
	Scenario         string   `json:"scenario"`
	Step             string   `json:"step"`
	Path             string   `json:"path"`
	OpBits           uint32   `json:"op_bits"`
	OpNames          []string `json:"op_names"`
	TimeOffsetMicros int64    `json:"time_offset_micros"`
}

type watchCaptureStep struct {
	Name string
	Run  func(root string) error
}

type WatchCaptureScenario struct {
	Name  string
	setup func(root string) error
	steps []watchCaptureStep
}

func (scenario WatchCaptureScenario) SetUp(root string) error {
	if scenario.setup == nil {
		return nil
	}

	return scenario.setup(root)
}

func (scenario WatchCaptureScenario) StepNames() []string {
	names := make([]string, 0, len(scenario.steps))
	for _, step := range scenario.steps {
		names = append(names, step.Name)
	}

	return names
}

func (scenario WatchCaptureScenario) RunStep(root, name string) error {
	for _, step := range scenario.steps {
		if step.Name == name {
			return step.Run(root)
		}
	}

	return fmt.Errorf("watch capture scenario %q: unknown step %q", scenario.Name, name)
}

func WatchCaptureScenarioNames() []string {
	names := []string{
		"dir_move_into_scope",
		"dir_move_out_of_scope",
		"marker_create",
		"marker_delete",
		"marker_parent_rename",
		"marker_rename",
	}
	sort.Strings(names)
	return names
}

func LookupWatchCaptureScenario(name string) (WatchCaptureScenario, error) {
	switch name {
	case "marker_create":
		return markerCreateScenario(), nil
	case "marker_delete":
		return markerDeleteScenario(), nil
	case "marker_rename":
		return markerRenameScenario(), nil
	case "marker_parent_rename":
		return markerParentRenameScenario(), nil
	case "dir_move_into_scope":
		return moveDirScenario("dir_move_into_scope", "parking/album", "docs/album", "move_into_scope", "move into scope"), nil
	case "dir_move_out_of_scope":
		return moveDirScenario("dir_move_out_of_scope", "docs/album", "parking/album", "move_out_of_scope", "move out of scope"), nil
	}

	return WatchCaptureScenario{}, fmt.Errorf(
		"watch capture: unknown scenario %q (known: %s)",
		name,
		strings.Join(WatchCaptureScenarioNames(), ", "),
	)
}

func RunWatchCapture(ctx context.Context, opts WatchCaptureOptions) error {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	if opts.Scenario == "" {
		return fmt.Errorf("watch capture: missing scenario")
	}

	repeat := opts.Repeat
	if repeat <= 0 {
		repeat = 1
	}

	settle := opts.Settle
	if settle <= 0 {
		settle = DefaultWatchCaptureSettle
	}

	scenario, err := LookupWatchCaptureScenario(opts.Scenario)
	if err != nil {
		return err
	}

	records, err := runWatchCaptureScenario(ctx, scenario, repeat, settle)
	if err != nil {
		return err
	}

	if opts.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(records); err != nil {
			return fmt.Errorf("watch capture: write json: %w", err)
		}
		return nil
	}

	for _, record := range records {
		if _, err := fmt.Fprintf(
			stdout,
			"%s %s %s %v %d\n",
			record.Scenario,
			record.Step,
			record.Path,
			record.OpNames,
			record.TimeOffsetMicros,
		); err != nil {
			return fmt.Errorf("watch capture: write text: %w", err)
		}
	}

	return nil
}

func runWatchCaptureScenario(
	ctx context.Context,
	scenario WatchCaptureScenario,
	repeat int,
	settle time.Duration,
) ([]WatchCaptureRecord, error) {
	records := make([]WatchCaptureRecord, 0, repeat*watchCaptureRecordReserve)

	for runIndex := 1; runIndex <= repeat; runIndex++ {
		runRecords, err := runWatchCaptureIteration(ctx, scenario, runIndex, repeat, settle)
		if err != nil {
			return nil, err
		}
		records = append(records, runRecords...)
	}

	return records, nil
}

func runWatchCaptureIteration(
	ctx context.Context,
	scenario WatchCaptureScenario,
	runIndex, repeat int,
	settle time.Duration,
) (records []WatchCaptureRecord, retErr error) {
	root, err := os.MkdirTemp("", "onedrive-go-watch-capture-*")
	if err != nil {
		return nil, fmt.Errorf("watch capture: create temp root: %w", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		cleanupErr := localpath.RemoveAll(root)
		if cleanupErr != nil {
			return nil, errors.Join(
				fmt.Errorf("watch capture: create watcher: %w", err),
				fmt.Errorf("watch capture: remove temp root: %w", cleanupErr),
			)
		}
		return nil, fmt.Errorf("watch capture: create watcher: %w", err)
	}

	defer func() {
		closeErr := watcher.Close()
		removeErr := localpath.RemoveAll(root)
		if closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("watch capture: close watcher: %w", closeErr))
		}
		if removeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("watch capture: remove temp root: %w", removeErr))
		}
	}()

	if setupErr := scenario.SetUp(root); setupErr != nil {
		return nil, fmt.Errorf("watch capture: setup scenario %q: %w", scenario.Name, setupErr)
	}

	watchedDirs := make(map[string]struct{})
	if err := addRecursiveWatchDirs(watcher, root, watchedDirs); err != nil {
		return nil, err
	}

	start := time.Now()
	records = make([]WatchCaptureRecord, 0, watchCaptureRecordReserve)
	for _, stepName := range scenario.StepNames() {
		if stepErr := scenario.RunStep(root, stepName); stepErr != nil {
			return nil, fmt.Errorf("watch capture: run %q step %q: %w", scenario.Name, stepName, stepErr)
		}

		recordStep := stepName
		if repeat > 1 {
			recordStep = fmt.Sprintf("repeat_%d:%s", runIndex, stepName)
		}

		stepRecords, err := collectWatchCaptureStep(ctx, watcher, root, scenario.Name, recordStep, settle, start)
		if err != nil {
			return nil, err
		}
		records = append(records, stepRecords...)

		if err := addRecursiveWatchDirs(watcher, root, watchedDirs); err != nil {
			return nil, err
		}
	}

	return records, nil
}

func addRecursiveWatchDirs(watcher *fsnotify.Watcher, root string, watched map[string]struct{}) error {
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("watch capture: walk %q: %w", path, err)
		}
		if !entry.IsDir() {
			return nil
		}

		cleanPath := filepath.Clean(path)
		if _, ok := watched[cleanPath]; ok {
			return nil
		}
		if err := watcher.Add(cleanPath); err != nil {
			return fmt.Errorf("watch capture: add watch %q: %w", cleanPath, err)
		}
		watched[cleanPath] = struct{}{}
		return nil
	}); err != nil {
		return fmt.Errorf("watch capture: sync watch dirs: %w", err)
	}

	return nil
}

func collectWatchCaptureStep(
	ctx context.Context,
	watcher *fsnotify.Watcher,
	root, scenario, step string,
	settle time.Duration,
	start time.Time,
) ([]WatchCaptureRecord, error) {
	timer := time.NewTimer(settle)
	defer timer.Stop()

	records := make([]WatchCaptureRecord, 0, watchCaptureRecordReserve)
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("watch capture: context canceled: %w", ctx.Err())
		case err := <-watcher.Errors:
			if err != nil {
				return nil, fmt.Errorf("watch capture: watcher error: %w", err)
			}
		case event := <-watcher.Events:
			record, err := newWatchCaptureRecord(root, scenario, step, start, event)
			if err != nil {
				return nil, err
			}
			records = append(records, record)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(settle)
		case <-timer.C:
			return records, nil
		}
	}
}

func newWatchCaptureRecord(
	root, scenario, step string,
	start time.Time,
	event fsnotify.Event,
) (WatchCaptureRecord, error) {
	relPath := "."
	if event.Name != "" {
		relative, err := filepath.Rel(root, event.Name)
		if err != nil {
			return WatchCaptureRecord{}, fmt.Errorf("watch capture: relativize %q: %w", event.Name, err)
		}
		relPath = filepath.ToSlash(relative)
	}

	return WatchCaptureRecord{
		Scenario:         scenario,
		Step:             step,
		Path:             relPath,
		OpBits:           uint32(event.Op),
		OpNames:          watchCaptureOpNames(event.Op),
		TimeOffsetMicros: time.Since(start).Microseconds(),
	}, nil
}

func watchCaptureOpNames(op fsnotify.Op) []string {
	type namedOp struct {
		flag fsnotify.Op
		name string
	}

	ordered := []namedOp{
		{flag: fsnotify.Create, name: "create"},
		{flag: fsnotify.Write, name: "write"},
		{flag: fsnotify.Remove, name: "remove"},
		{flag: fsnotify.Rename, name: "rename"},
		{flag: fsnotify.Chmod, name: "chmod"},
	}

	names := make([]string, 0, len(ordered))
	for _, candidate := range ordered {
		if op&candidate.flag != 0 {
			names = append(names, candidate.name)
		}
	}

	return names
}

func watchCaptureScenarioStep(name string, run func(root string) error) watchCaptureStep {
	return watchCaptureStep{Name: name, Run: run}
}

func writeScenarioFile(root, relPath, content string) error {
	fullPath := filepath.Join(root, filepath.FromSlash(relPath))
	if err := localpath.MkdirAll(filepath.Dir(fullPath), watchCaptureDirPerm); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(fullPath), err)
	}
	if err := localpath.WriteFile(fullPath, []byte(content), watchCaptureFilePerm); err != nil {
		return fmt.Errorf("write file %q: %w", fullPath, err)
	}

	return nil
}

func ensureScenarioDir(root, relPath string) error {
	if err := localpath.MkdirAll(filepath.Join(root, filepath.FromSlash(relPath)), watchCaptureDirPerm); err != nil {
		return fmt.Errorf("mkdir %q: %w", relPath, err)
	}

	return nil
}

func markerCreateScenario() WatchCaptureScenario {
	return WatchCaptureScenario{
		Name: "marker_create",
		setup: func(root string) error {
			return ensureScenarioDir(root, "blocked/nested")
		},
		steps: []watchCaptureStep{
			watchCaptureScenarioStep("create_marker", func(root string) error {
				return writeScenarioFile(root, "blocked/.odignore", "marker")
			}),
		},
	}
}

func markerDeleteScenario() WatchCaptureScenario {
	return WatchCaptureScenario{
		Name: "marker_delete",
		setup: func(root string) error {
			if err := ensureScenarioDir(root, "blocked/nested"); err != nil {
				return err
			}
			if err := writeScenarioFile(root, "blocked/.odignore", "marker"); err != nil {
				return err
			}
			return writeScenarioFile(root, "blocked/nested/keep.txt", "keep")
		},
		steps: []watchCaptureStep{
			watchCaptureScenarioStep("delete_marker", func(root string) error {
				if err := localpath.Remove(filepath.Join(root, "blocked", ".odignore")); err != nil {
					return fmt.Errorf("remove marker: %w", err)
				}
				return nil
			}),
		},
	}
}

func markerRenameScenario() WatchCaptureScenario {
	return WatchCaptureScenario{
		Name: "marker_rename",
		setup: func(root string) error {
			if err := ensureScenarioDir(root, "blocked/nested"); err != nil {
				return err
			}
			return writeScenarioFile(root, "blocked/pending.marker", "marker")
		},
		steps: []watchCaptureStep{
			watchCaptureScenarioStep("rename_to_marker", func(root string) error {
				if err := localpath.Rename(
					filepath.Join(root, "blocked", "pending.marker"),
					filepath.Join(root, "blocked", ".odignore"),
				); err != nil {
					return fmt.Errorf("rename to marker: %w", err)
				}
				return nil
			}),
		},
	}
}

func markerParentRenameScenario() WatchCaptureScenario {
	return WatchCaptureScenario{
		Name: "marker_parent_rename",
		setup: func(root string) error {
			if err := ensureScenarioDir(root, "parent/blocked/nested"); err != nil {
				return err
			}
			if err := writeScenarioFile(root, "parent/blocked/.odignore", "marker"); err != nil {
				return err
			}
			return writeScenarioFile(root, "parent/blocked/nested/keep.txt", "keep")
		},
		steps: []watchCaptureStep{
			watchCaptureScenarioStep("rename_parent", func(root string) error {
				if err := localpath.Rename(
					filepath.Join(root, "parent"),
					filepath.Join(root, "renamed"),
				); err != nil {
					return fmt.Errorf("rename parent: %w", err)
				}
				return nil
			}),
		},
	}
}

func moveDirScenario(
	name string,
	sourceDir string,
	destDir string,
	stepName string,
	moveLabel string,
) WatchCaptureScenario {
	sourceFile := filepath.ToSlash(filepath.Join(sourceDir, "photo.jpg"))
	return WatchCaptureScenario{
		Name: name,
		setup: func(root string) error {
			if err := ensureScenarioDir(root, sourceDir); err != nil {
				return err
			}
			if err := ensureScenarioDir(root, filepath.Dir(destDir)); err != nil {
				return err
			}
			return writeScenarioFile(root, sourceFile, "img")
		},
		steps: []watchCaptureStep{
			watchCaptureScenarioStep(stepName, func(root string) error {
				if err := localpath.Rename(
					filepath.Join(root, filepath.FromSlash(sourceDir)),
					filepath.Join(root, filepath.FromSlash(destDir)),
				); err != nil {
					return fmt.Errorf("%s: %w", moveLabel, err)
				}
				return nil
			}),
		},
	}
}
