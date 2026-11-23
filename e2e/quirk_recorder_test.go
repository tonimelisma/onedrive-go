package e2e

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/testutil"
)

// The quirk recorder takes the roles it redacts with, and its recording
// methods take the recorder, so its tests run without an account, in the
// quick tier; the live suite records through suiteQuirkRecorder (TestMain).

const (
	quirkEventsFileName  = "quirk-events.jsonl"
	quirkSummaryFileName = "quirk-summary.json"

	quirkPhaseCLICommand    = "cli_command"
	quirkPhaseAuthPreflight = "auth_preflight"
	quirkPhaseFixtureSeed   = "fixture_seed"

	quirkSourceCLIDebugStderr    = "cli_debug_stderr"
	quirkSourceAuthPreflight     = "auth_preflight_retry"
	quirkSourceFixtureRecurrence = "fixture_recurrence"
	quirkSourceStandingCondition = "standing_sync_condition"

	quirkOutcomeLogged    = "logged"
	quirkOutcomeFailed    = "failed"
	quirkOutcomeRecovered = "recovered"
	quirkOutcomeRetried   = "retried"
	quirkOutcomeSoftened  = "softened"
)

type e2eQuirkEvent struct {
	Phase         string   `json:"phase"`
	TestName      string   `json:"test_name,omitempty"`
	Operation     string   `json:"operation,omitempty"`
	Source        string   `json:"source"`
	QuirkOrReason string   `json:"quirk_or_reason"`
	AttemptCount  int      `json:"attempt_count,omitempty"`
	RequestIDs    []string `json:"request_ids,omitempty"`
	StatusCodes   []int    `json:"status_codes,omitempty"`
	GraphCodes    []string `json:"graph_codes,omitempty"`
	Outcome       string   `json:"outcome"`
	FinalError    string   `json:"final_error,omitempty"`
}

type e2eQuirkSummary struct {
	Events []e2eQuirkEvent `json:"events"`
}

// e2eQuirkRecorder keeps the suite's quirk events and writes them and their
// summary through roles: what it writes is a CI artifact of a public
// repository. A nil recorder records nothing.
type e2eQuirkRecorder struct {
	mu     sync.Mutex
	logDir string
	roles  testutil.DriveRoles
	events []e2eQuirkEvent
}

type cliQuirkLogLine struct {
	GraphQuirk             string                    `json:"graph_quirk"`
	GraphQuirkAttemptCount int                       `json:"graph_quirk_attempt_count"`
	GraphQuirkAttempts     []graph.QuirkRetryAttempt `json:"graph_quirk_attempts"`
	Error                  string                    `json:"error"`
}

func newE2EQuirkRecorder(logDir string, roles testutil.DriveRoles) (*e2eQuirkRecorder, error) {
	if logDir == "" {
		return nil, errors.New("quirk recorder log dir is required")
	}

	events, err := loadQuirkEvents(filepath.Join(logDir, quirkEventsFileName))
	if err != nil {
		return nil, err
	}

	recorder := &e2eQuirkRecorder{
		logDir: logDir,
		roles:  roles,
		events: events,
	}
	if err := recorder.writeSummaryLocked(); err != nil {
		return nil, err
	}

	return recorder, nil
}

// record keeps one event, with every configured drive named by role
// (redactQuirkEvent).
func (r *e2eQuirkRecorder) record(event *e2eQuirkEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	redacted := redactQuirkEvent(r.roles, event)
	r.events = append(r.events, redacted)
	if err := appendQuirkEvent(r.eventsPath(), &redacted); err != nil {
		return err
	}

	return r.writeSummaryLocked()
}

// redactQuirkEvent names every configured drive in a quirk event by its role
// in roles: a CLI command line carries `--drive <id>`, an auth preflight
// subtest is named after its drive, and the summary is uploaded as a CI
// artifact of a public repository.
func redactQuirkEvent(roles testutil.DriveRoles, event *e2eQuirkEvent) e2eQuirkEvent {
	redacted := *event
	redacted.TestName = roles.Redact(event.TestName)
	redacted.Operation = roles.Redact(event.Operation)
	redacted.QuirkOrReason = roles.Redact(event.QuirkOrReason)
	redacted.FinalError = roles.Redact(event.FinalError)

	return redacted
}

func (r *e2eQuirkRecorder) writeSummaryLocked() error {
	events := append([]e2eQuirkEvent{}, r.events...)
	summaryData, err := json.MarshalIndent(e2eQuirkSummary{Events: events}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal quirk summary: %w", err)
	}
	summaryData = append(summaryData, '\n')
	if err := localpath.AtomicWrite(r.summaryPath(), summaryData, 0o644, 0o755, ".quirk-summary-*.tmp"); err != nil {
		return fmt.Errorf("write quirk summary: %w", err)
	}

	return nil
}

func (r *e2eQuirkRecorder) eventsPath() string {
	return filepath.Join(r.logDir, quirkEventsFileName)
}

func (r *e2eQuirkRecorder) summaryPath() string {
	return filepath.Join(r.logDir, quirkSummaryFileName)
}

func appendQuirkEvent(path string, event *e2eQuirkEvent) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // a recorder file under the suite's own E2E log dir
	if err != nil {
		return fmt.Errorf("open quirk events %s: %w", path, err)
	}
	defer func() {
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}()

	if err := json.NewEncoder(f).Encode(event); err != nil {
		return fmt.Errorf("encode quirk event: %w", err)
	}

	return nil
}

func loadQuirkEvents(path string) ([]e2eQuirkEvent, error) {
	return loadNDJSONEvents[e2eQuirkEvent](path, "quirk")
}

// recordFor keeps event for t, which a failure to record warns about but does
// not fail.
func (r *e2eQuirkRecorder) recordFor(t *testing.T, event *e2eQuirkEvent) {
	t.Helper()

	if r == nil {
		return
	}

	if err := r.record(event); err != nil {
		t.Logf("warning: cannot record quirk event: %v", err)
	}
}

// recordCLIEvents keeps an event for each Graph quirk a CLI run's --debug
// stderr logs.
func (r *e2eQuirkRecorder) recordCLIEvents(t *testing.T, args []string, stderr string, cmdErr error) {
	t.Helper()

	if r == nil {
		return
	}

	scanner := bufio.NewScanner(strings.NewReader(stderr))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}

		var record cliQuirkLogLine
		if err := json.Unmarshal([]byte(line), &record); err != nil || record.GraphQuirk == "" {
			continue
		}

		finalError := record.Error
		outcome := quirkOutcomeLogged
		if cmdErr != nil {
			outcome = quirkOutcomeFailed
			if finalError == "" {
				finalError = cmdErr.Error()
			}
		}

		r.recordFor(t, &e2eQuirkEvent{
			Phase:         quirkPhaseCLICommand,
			TestName:      t.Name(),
			Operation:     strings.Join(args, " "),
			Source:        quirkSourceCLIDebugStderr,
			QuirkOrReason: record.GraphQuirk,
			AttemptCount:  max(record.GraphQuirkAttemptCount, len(record.GraphQuirkAttempts)),
			RequestIDs:    quirkRequestIDs(record.GraphQuirkAttempts),
			StatusCodes:   quirkStatusCodes(record.GraphQuirkAttempts),
			GraphCodes:    quirkGraphCodes(record.GraphQuirkAttempts),
			Outcome:       outcome,
			FinalError:    finalError,
		})
	}
	if err := scanner.Err(); err != nil {
		t.Logf("warning: cannot scan CLI stderr for quirk evidence: %v", err)
	}
}

// recordAuthPreflightDecision keeps an event for an auth preflight whose
// attempts were retried for a reason, naming the drive by its role.
func (r *e2eQuirkRecorder) recordAuthPreflightDecision(
	t *testing.T,
	driveID string,
	endpoint string,
	attempts []authPreflightAttempt,
	finalErr error,
) {
	t.Helper()

	if r == nil || len(attempts) == 0 {
		return
	}

	reasons := make([]string, 0, len(attempts))
	requestIDs := make([]string, 0, len(attempts))
	statusCodes := make([]int, 0, len(attempts))
	graphCodes := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.RequestID != "" {
			requestIDs = append(requestIDs, attempt.RequestID)
		}
		if attempt.StatusCode != 0 {
			statusCodes = append(statusCodes, attempt.StatusCode)
		}
		if attempt.Code != "" {
			graphCodes = append(graphCodes, attempt.Code)
		}
		if attempt.succeeded() {
			continue
		}

		decision := classifyAuthPreflightAttempt(endpoint, attempt)
		if decision.Reason != "" && decision.Reason != "success" {
			reasons = append(reasons, decision.Reason)
		}
	}

	if len(reasons) == 0 {
		return
	}

	outcome := quirkOutcomeRecovered
	finalError := ""
	if finalErr != nil {
		outcome = quirkOutcomeFailed
		finalError = finalErr.Error()
	}

	r.recordFor(t, &e2eQuirkEvent{
		Phase:         quirkPhaseAuthPreflight,
		TestName:      t.Name(),
		Operation:     fmt.Sprintf("GET %s drive=%s", endpoint, r.roles.Role(driveID)),
		Source:        quirkSourceAuthPreflight,
		QuirkOrReason: summarizeReasons(reasons),
		AttemptCount:  len(attempts),
		RequestIDs:    uniqueStrings(requestIDs),
		StatusCodes:   uniqueInts(statusCodes),
		GraphCodes:    uniqueStrings(graphCodes),
		Outcome:       outcome,
		FinalError:    finalError,
	})
}

// recordLiveProviderRecurrence keeps an event for a known recurrence of a
// live provider condition; an unknown one is not kept.
func (r *e2eQuirkRecorder) recordLiveProviderRecurrence(
	t *testing.T,
	operation string,
	decision liveProviderRecurrenceDecision,
	outcome string,
	finalError string,
) {
	t.Helper()

	if decision.Reason == liveProviderRecurrenceUnknown {
		return
	}

	r.recordFor(t, &e2eQuirkEvent{
		Phase:         quirkPhaseFixtureSeed,
		TestName:      t.Name(),
		Operation:     operation,
		Source:        quirkSourceFixtureRecurrence,
		QuirkOrReason: string(decision.Reason),
		AttemptCount:  1,
		Outcome:       outcome,
		FinalError:    finalError,
	})
}

func summarizeReasons(reasons []string) string {
	unique := uniqueStrings(reasons)
	if len(unique) == 1 {
		return unique[0]
	}

	return "mixed:" + strings.Join(unique, ",")
}

func quirkRequestIDs(attempts []graph.QuirkRetryAttempt) []string {
	values := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.RequestID != "" {
			values = append(values, attempt.RequestID)
		}
	}

	return uniqueStrings(values)
}

func quirkStatusCodes(attempts []graph.QuirkRetryAttempt) []int {
	values := make([]int, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.StatusCode != 0 {
			values = append(values, attempt.StatusCode)
		}
	}

	return uniqueInts(values)
}

func quirkGraphCodes(attempts []graph.QuirkRetryAttempt) []string {
	values := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.GraphCode != "" {
			values = append(values, attempt.GraphCode)
		}
	}

	return uniqueStrings(values)
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}

	return result
}

func uniqueInts(values []int) []int {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[int]struct{}, len(values))
	result := make([]int, 0, len(values))
	for _, value := range values {
		if value == 0 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}

	return result
}

func readQuirkSummaryFile(t *testing.T, path string) e2eQuirkSummary {
	t.Helper()

	data := mustReadFile(t, path)

	var summary e2eQuirkSummary
	require.NoError(t, json.Unmarshal(data, &summary))
	return summary
}

func TestE2EQuirkRecorderWritesSummaryAndLoadsExistingEventsAcrossProcesses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first, err := newE2EQuirkRecorder(dir, plantedRecorderRoles())
	require.NoError(t, err)
	require.NoError(t, first.record(&e2eQuirkEvent{
		Phase:         quirkPhaseCLICommand,
		TestName:      "TestOne",
		Operation:     "drive list",
		Source:        quirkSourceCLIDebugStderr,
		QuirkOrReason: "drive-discovery-forbidden",
		AttemptCount:  2,
		RequestIDs:    []string{"req-1", "req-2"},
		StatusCodes:   []int{403},
		GraphCodes:    []string{"accessDenied"},
		Outcome:       quirkOutcomeLogged,
	}))

	second, err := newE2EQuirkRecorder(dir, plantedRecorderRoles())
	require.NoError(t, err)
	require.Len(t, second.events, 1)
	require.NoError(t, second.record(&e2eQuirkEvent{
		Phase:         quirkPhaseAuthPreflight,
		TestName:      "TestTwo",
		Operation:     "GET /me/drives drive=personal:test@example.com",
		Source:        quirkSourceAuthPreflight,
		QuirkOrReason: "drive_catalog_projection_lag",
		AttemptCount:  3,
		RequestIDs:    []string{"req-3"},
		StatusCodes:   []int{403},
		GraphCodes:    []string{"accessDenied"},
		Outcome:       quirkOutcomeRecovered,
	}))

	lines, err := loadQuirkEvents(filepath.Join(dir, quirkEventsFileName))
	require.NoError(t, err)
	require.Len(t, lines, 2)

	summary := readQuirkSummaryFile(t, filepath.Join(dir, quirkSummaryFileName))
	require.Len(t, summary.Events, 2)
	assert.Equal(t, "drive-discovery-forbidden", summary.Events[0].QuirkOrReason)
	assert.Equal(t, quirkOutcomeRecovered, summary.Events[1].Outcome)
}

func TestRecordCLIQuirkEventsParsesStructuredJSONLogs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	recorder, err := newE2EQuirkRecorder(dir, plantedRecorderRoles())
	require.NoError(t, err)

	stderr := `{"time":"2026-04-10T23:56:11.987283-07:00","level":"WARN","msg":"graph quirk retry exhausted","graph_quirk":"download-metadata-transient-404","graph_quirk_attempt_count":4,"graph_quirk_attempts":[{"attempt":1,"statusCode":404,"graphCode":"itemNotFound","requestId":"req-1"},{"attempt":2,"statusCode":404,"graphCode":"itemNotFound","requestId":"req-2"}],"error":"graph: HTTP 404"}` + "\n" +
		`plain stderr`

	recorder.recordCLIEvents(t, []string{"--debug", "sync"}, stderr, assert.AnError)

	summary := readQuirkSummaryFile(t, filepath.Join(dir, quirkSummaryFileName))
	require.Len(t, summary.Events, 1)
	assert.Equal(t, quirkPhaseCLICommand, summary.Events[0].Phase)
	assert.Equal(t, quirkSourceCLIDebugStderr, summary.Events[0].Source)
	assert.Equal(t, "download-metadata-transient-404", summary.Events[0].QuirkOrReason)
	assert.Equal(t, 4, summary.Events[0].AttemptCount)
	assert.Equal(t, []string{"req-1", "req-2"}, summary.Events[0].RequestIDs)
	assert.Equal(t, []int{404}, summary.Events[0].StatusCodes)
	assert.Equal(t, []string{"itemNotFound"}, summary.Events[0].GraphCodes)
	assert.Equal(t, quirkOutcomeFailed, summary.Events[0].Outcome)
}

func TestRecordAuthPreflightDecisionEventSummarizesRetryReasons(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	recorder, err := newE2EQuirkRecorder(dir, plantedRecorderRoles())
	require.NoError(t, err)

	recorder.recordAuthPreflightDecision(t, "personal:test@example.com", "/me/drives", []authPreflightAttempt{
		{StatusCode: 403, Code: "accessDenied", RequestID: "req-1", Err: "forbidden"},
		{StatusCode: 200},
	}, nil)

	summary := readQuirkSummaryFile(t, filepath.Join(dir, quirkSummaryFileName))
	require.Len(t, summary.Events, 1)
	assert.Equal(t, quirkPhaseAuthPreflight, summary.Events[0].Phase)
	assert.Equal(t, "drive_catalog_projection_lag", summary.Events[0].QuirkOrReason)
	assert.Equal(t, quirkOutcomeRecovered, summary.Events[0].Outcome)
	assert.Equal(t, []string{"req-1"}, summary.Events[0].RequestIDs)
	assert.Equal(t, []int{403, 200}, summary.Events[0].StatusCodes)
	assert.Equal(t, []string{"accessDenied"}, summary.Events[0].GraphCodes)
}

func TestRecordLiveProviderRecurrenceEventIgnoresUnknownReasons(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	recorder, err := newE2EQuirkRecorder(dir, plantedRecorderRoles())
	require.NoError(t, err)

	recorder.recordLiveProviderRecurrence(t, "put /file.txt", liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrenceUnknown,
		Retry:  false,
	}, quirkOutcomeRetried, "")

	summary := readQuirkSummaryFile(t, filepath.Join(dir, quirkSummaryFileName))
	assert.Empty(t, summary.Events)
}

func TestRecordLiveProviderRecurrenceEventWritesKnownReasons(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	recorder, err := newE2EQuirkRecorder(dir, plantedRecorderRoles())
	require.NoError(t, err)

	recorder.recordLiveProviderRecurrence(t, "fixture visibility /file.txt", liveProviderRecurrenceDecision{
		Reason: liveProviderRecurrencePostMutationDestinationPathLag,
		Retry:  false,
	}, quirkOutcomeSoftened, "stat still 404")

	summary := readQuirkSummaryFile(t, filepath.Join(dir, quirkSummaryFileName))
	require.Len(t, summary.Events, 1)
	assert.Equal(t, quirkPhaseFixtureSeed, summary.Events[0].Phase)
	assert.Equal(t, quirkSourceFixtureRecurrence, summary.Events[0].Source)
	assert.Equal(t, string(liveProviderRecurrencePostMutationDestinationPathLag), summary.Events[0].QuirkOrReason)
	assert.Equal(t, quirkOutcomeSoftened, summary.Events[0].Outcome)
	assert.Equal(t, "stat still 404", summary.Events[0].FinalError)
}
