//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

const (
	e2eSkipSuiteScrubEnvVar = "ONEDRIVE_E2E_SKIP_SUITE_SCRUB"

	timingEventsFileName  = "timing-events.jsonl"
	timingSummaryFileName = "timing-summary.json"

	timingKindRemoteWriteVisibility     = "remote_write_visibility"
	timingKindRemoteDeleteDisappearance = "remote_delete_disappearance"
	timingKindRemoteScopeTransition     = "remote_scope_transition"
	timingKindSyncConvergence           = "sync_convergence"

	timingOutcomeSuccess = "success"
	timingOutcomeTimeout = "timeout"

	timingSlowestEventLimit = 5
)

type e2eTimingEvent struct {
	TestName    string `json:"test_name"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	DriveID     string `json:"drive_id,omitempty"`
	Command     string `json:"command"`
	TimeoutMS   int64  `json:"timeout_ms"`
	DurationMS  int64  `json:"duration_ms"`
	Attempts    int    `json:"attempts"`
	Outcome     string `json:"outcome"`
}

type e2eTimingKindSummary struct {
	Kind          string           `json:"kind"`
	Count         int              `json:"count"`
	SuccessCount  int              `json:"success_count"`
	TimeoutCount  int              `json:"timeout_count"`
	MinMS         int64            `json:"min_ms"`
	P50MS         int64            `json:"p50_ms"`
	P95MS         int64            `json:"p95_ms"`
	MaxMS         int64            `json:"max_ms"`
	SlowestEvents []e2eTimingEvent `json:"slowest_events"`
}

type e2eTimingSummary struct {
	Kinds []e2eTimingKindSummary `json:"kinds"`
}

type e2eTimingRecorder struct {
	mu     sync.Mutex
	logDir string
	events []e2eTimingEvent
}

func newE2ETimingRecorder(logDir string) (*e2eTimingRecorder, error) {
	if logDir == "" {
		return nil, fmt.Errorf("timing recorder log dir is required")
	}

	events, err := loadTimingEvents(filepath.Join(logDir, timingEventsFileName))
	if err != nil {
		return nil, err
	}

	return &e2eTimingRecorder{
		logDir: logDir,
		events: events,
	}, nil
}

func recordTimingEvent(
	t *testing.T,
	kind string,
	description string,
	driveID string,
	args []string,
	timeout time.Duration,
	duration time.Duration,
	attempts int,
	outcome string,
) {
	t.Helper()

	if suiteTimingRecorder == nil {
		return
	}

	if err := suiteTimingRecorder.record(e2eTimingEvent{
		TestName:    t.Name(),
		Kind:        kind,
		Description: description,
		DriveID:     driveID,
		Command:     strings.Join(args, " "),
		TimeoutMS:   timeout.Milliseconds(),
		DurationMS:  duration.Milliseconds(),
		Attempts:    attempts,
		Outcome:     outcome,
	}); err != nil {
		t.Logf("warning: cannot record timing event: %v", err)
	}
}

func (r *e2eTimingRecorder) record(event e2eTimingEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event)
	if err := appendTimingEvent(r.eventsPath(), event); err != nil {
		return err
	}

	summaryData, err := json.MarshalIndent(summarizeTimingEvents(r.events), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal timing summary: %w", err)
	}
	summaryData = append(summaryData, '\n')
	if err := localpath.AtomicWrite(r.summaryPath(), summaryData, 0o644, 0o755, ".timing-summary-*.tmp"); err != nil {
		return fmt.Errorf("write timing summary: %w", err)
	}

	if event.TestName != "" {
		if err := appendDebugLogChunk(
			filepath.Join(r.logDir, sanitizeTestName(event.TestName)+".log"),
			formatTimingDebugLine(event),
		); err != nil {
			return fmt.Errorf("append timing debug log: %w", err)
		}
	}

	return nil
}

func (r *e2eTimingRecorder) eventsPath() string {
	return filepath.Join(r.logDir, timingEventsFileName)
}

func (r *e2eTimingRecorder) summaryPath() string {
	return filepath.Join(r.logDir, timingSummaryFileName)
}

func appendTimingEvent(path string, event e2eTimingEvent) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open timing events %s: %w", path, err)
	}
	defer func() {
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}()

	if err := json.NewEncoder(f).Encode(event); err != nil {
		return fmt.Errorf("encode timing event: %w", err)
	}

	return nil
}

func loadTimingEvents(path string) ([]e2eTimingEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("open timing events %s: %w", path, err)
	}
	defer f.Close()

	events := make([]e2eTimingEvent, 0)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var event e2eTimingEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("decode timing event: %w", err)
		}

		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan timing events: %w", err)
	}

	return events, nil
}

func summarizeTimingEvents(events []e2eTimingEvent) e2eTimingSummary {
	byKind := make(map[string][]e2eTimingEvent)
	for _, event := range events {
		byKind[event.Kind] = append(byKind[event.Kind], event)
	}

	kinds := make([]string, 0, len(byKind))
	for kind := range byKind {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)

	summary := e2eTimingSummary{
		Kinds: make([]e2eTimingKindSummary, 0, len(kinds)),
	}
	for _, kind := range kinds {
		summary.Kinds = append(summary.Kinds, summarizeTimingKind(kind, byKind[kind]))
	}

	return summary
}

func summarizeTimingKind(kind string, events []e2eTimingEvent) e2eTimingKindSummary {
	summary := e2eTimingKindSummary{
		Kind:  kind,
		Count: len(events),
	}

	durations := make([]int64, 0, len(events))
	slowest := append([]e2eTimingEvent(nil), events...)
	for _, event := range events {
		durations = append(durations, event.DurationMS)
		switch event.Outcome {
		case timingOutcomeSuccess:
			summary.SuccessCount++
		case timingOutcomeTimeout:
			summary.TimeoutCount++
		}
	}

	sort.Slice(durations, func(i, j int) bool {
		return durations[i] < durations[j]
	})
	summary.MinMS = durations[0]
	summary.P50MS = percentileDurationMS(durations, 50, 100)
	summary.P95MS = percentileDurationMS(durations, 95, 100)
	summary.MaxMS = durations[len(durations)-1]

	sort.Slice(slowest, func(i, j int) bool {
		if slowest[i].DurationMS == slowest[j].DurationMS {
			return slowest[i].TestName < slowest[j].TestName
		}
		return slowest[i].DurationMS > slowest[j].DurationMS
	})
	if len(slowest) > timingSlowestEventLimit {
		slowest = slowest[:timingSlowestEventLimit]
	}
	summary.SlowestEvents = slowest

	return summary
}

func percentileDurationMS(durations []int64, numerator int, denominator int) int64 {
	if len(durations) == 0 {
		return 0
	}

	index := (len(durations)*numerator + denominator - 1) / denominator
	if index <= 0 {
		index = 1
	}
	if index > len(durations) {
		index = len(durations)
	}

	return durations[index-1]
}

func formatTimingDebugLine(event e2eTimingEvent) string {
	return fmt.Sprintf(
		"TIMING kind=%s outcome=%s attempts=%d duration=%s timeout=%s drive=%s cmd=%s desc=%s\n",
		event.Kind,
		event.Outcome,
		event.Attempts,
		time.Duration(event.DurationMS)*time.Millisecond,
		time.Duration(event.TimeoutMS)*time.Millisecond,
		event.DriveID,
		event.Command,
		event.Description,
	)
}

func readTimingSummaryFile(t *testing.T, path string) e2eTimingSummary {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var summary e2eTimingSummary
	require.NoError(t, json.Unmarshal(data, &summary))
	return summary
}

func findTimingKindSummary(t *testing.T, summary e2eTimingSummary, kind string) e2eTimingKindSummary {
	t.Helper()

	for _, candidate := range summary.Kinds {
		if candidate.Kind == kind {
			return candidate
		}
	}

	require.Failf(t, "missing timing kind", "kind %q not found", kind)
	return e2eTimingKindSummary{}
}

func TestE2ETimingRecorderWritesEventsAndAggregatedSummary(t *testing.T) {
	dir := t.TempDir()
	recorder, err := newE2ETimingRecorder(dir)
	require.NoError(t, err)

	events := []e2eTimingEvent{
		{TestName: "TestWrite", Kind: timingKindRemoteWriteVisibility, Description: "write", DriveID: "drive-a", Command: "stat /a", TimeoutMS: 120000, DurationMS: 1200, Attempts: 2, Outcome: timingOutcomeSuccess},
		{TestName: "TestDelete", Kind: timingKindRemoteDeleteDisappearance, Description: "delete", DriveID: "drive-a", Command: "ls /a", TimeoutMS: 120000, DurationMS: 2400, Attempts: 3, Outcome: timingOutcomeTimeout},
		{TestName: "TestScope", Kind: timingKindRemoteScopeTransition, Description: "scope", DriveID: "drive-b", Command: "ls /b", TimeoutMS: 180000, DurationMS: 1800, Attempts: 2, Outcome: timingOutcomeSuccess},
		{TestName: "TestSync", Kind: timingKindSyncConvergence, Description: "sync", Command: "sync --download-only", TimeoutMS: 180000, DurationMS: 3600, Attempts: 4, Outcome: timingOutcomeSuccess},
	}
	for _, event := range events {
		require.NoError(t, recorder.record(event))
	}

	lines, err := loadTimingEvents(filepath.Join(dir, timingEventsFileName))
	require.NoError(t, err)
	require.Len(t, lines, len(events))

	summary := readTimingSummaryFile(t, filepath.Join(dir, timingSummaryFileName))
	require.Len(t, summary.Kinds, 4)
	assert.Equal(t, 1, findTimingKindSummary(t, summary, timingKindRemoteWriteVisibility).SuccessCount)
	assert.Equal(t, 1, findTimingKindSummary(t, summary, timingKindRemoteDeleteDisappearance).TimeoutCount)
	assert.Equal(t, int64(3600), findTimingKindSummary(t, summary, timingKindSyncConvergence).MaxMS)
	assert.NotEmpty(t, findTimingKindSummary(t, summary, timingKindRemoteScopeTransition).SlowestEvents)
}

func TestE2ETimingRecorderLoadsExistingEventsAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	first, err := newE2ETimingRecorder(dir)
	require.NoError(t, err)
	require.NoError(t, first.record(e2eTimingEvent{
		TestName:    "TestOne",
		Kind:        timingKindSyncConvergence,
		Description: "first",
		Command:     "sync --force",
		TimeoutMS:   1000,
		DurationMS:  100,
		Attempts:    1,
		Outcome:     timingOutcomeSuccess,
	}))

	second, err := newE2ETimingRecorder(dir)
	require.NoError(t, err)
	require.Len(t, second.events, 1)
	require.NoError(t, second.record(e2eTimingEvent{
		TestName:    "TestTwo",
		Kind:        timingKindSyncConvergence,
		Description: "second",
		Command:     "sync --force",
		TimeoutMS:   1000,
		DurationMS:  200,
		Attempts:    2,
		Outcome:     timingOutcomeTimeout,
	}))

	summary := readTimingSummaryFile(t, filepath.Join(dir, timingSummaryFileName))
	syncSummary := findTimingKindSummary(t, summary, timingKindSyncConvergence)
	assert.Equal(t, 2, syncSummary.Count)
	assert.Equal(t, 1, syncSummary.SuccessCount)
	assert.Equal(t, 1, syncSummary.TimeoutCount)
	assert.Equal(t, int64(200), syncSummary.MaxMS)
}

func TestE2ETimingRecorderAppendsDebugLogLine(t *testing.T) {
	dir := t.TempDir()
	recorder, err := newE2ETimingRecorder(dir)
	require.NoError(t, err)

	require.NoError(t, recorder.record(e2eTimingEvent{
		TestName:    "TestTimingDebug",
		Kind:        timingKindRemoteWriteVisibility,
		Description: "debug line",
		DriveID:     "drive-a",
		Command:     "stat /timing.txt",
		TimeoutMS:   120000,
		DurationMS:  1500,
		Attempts:    3,
		Outcome:     timingOutcomeSuccess,
	}))

	data, err := os.ReadFile(filepath.Join(dir, sanitizeTestName("TestTimingDebug")+".log"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "TIMING kind=remote_write_visibility")
	assert.Contains(t, string(data), "cmd=stat /timing.txt")
}
