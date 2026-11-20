package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Tests in this file pin the console-noise contract of the sync engine: what
// reaches WARN/ERROR (shown to every user at default verbosity) and what
// stays below it because it repeats, heals itself, is shutdown noise, or is
// already reported elsewhere.

// Validates: R-6.6.22
func TestEngineFlow_FatalAuth_ReturnedOnceNotLogged(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	flow := testEngineFlow(t, eng)

	decision := &ResultDecision{
		Class:        errclass.ClassFatal,
		ConditionKey: ConditionAuthenticationRequired,
		TrialHint:    TrialHintFatal,
	}
	for _, path := range []string{"first.txt", "second.txt", "third.txt"} {
		_, err := flow.applyNormalCompletionDecision(t.Context(), decision, nil, &ActionCompletion{
			Path:       path,
			ActionType: ActionUpload,
		}, nil)

		var fatalErr *fatalCompletionError
		require.ErrorAs(t, err, &fatalErr, "the sign-in failure is returned for the CLI to print")
	}

	output := logs.String()
	assert.Zero(t, countLevel(output, "ERROR"), "the returned error is the report; logging it too prints it twice")
	assert.Zero(t, countLevel(output, "WARN"))
	assert.Equal(t, 1, strings.Count(output, "authentication required: sync stopping"),
		"later in-flight 401s add nothing new")
}

// Validates: R-6.6.21
func TestEngineFlow_SuppressedShutdownCompletionError_QuietForCancellationAndRepeatFatal(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	flow := testEngineFlow(t, eng)
	completion := &ActionCompletion{Path: "a.txt", ActionType: ActionUpload}

	flow.logSuppressedShutdownCompletionError(completion, fmt.Errorf("commit outcome: %w", context.Canceled))
	flow.logSuppressedShutdownCompletionError(completion, fatalResultError(completion))
	assert.Zero(t, countLevel(logs.String(), "WARN"), "a clean shutdown produces no warnings")

	flow.logSuppressedShutdownCompletionError(completion, errors.New("disk I/O error"))
	output := logs.String()
	assert.Equal(t, 1, countLevel(output, "WARN"), "a real bookkeeping failure while stopping stays visible")
	assert.Contains(t, output, "Could not record the result for a.txt while stopping")
}

// Validates: R-6.6.30
func TestPollOutageLog_WarnsOncePerOutage(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	var outage PollOutageLog

	for range 5 {
		outage.Failed(logger, "Can't check OneDrive for changes; retrying", errors.New("dial tcp: no route"), 0)
	}
	outage.Recovered(t.Context(), logger, "Checking OneDrive for changes works again")
	outage.Recovered(t.Context(), logger, "Checking OneDrive for changes works again")
	outage.Failed(logger, "Can't check OneDrive for changes; retrying", errors.New("dial tcp: no route"), 0)

	output := logs.String()
	assert.Equal(t, 2, countLevel(output, "WARN"), "one warning per outage, not per attempt")
	assert.Equal(t, 4, countLevel(output, "DEBUG"), "repeats inside one outage stay at debug")
	assert.Equal(t, 1, countLevel(output, "INFO+2"), "recovery is reported once, at NOTICE, only after an outage")
}

// Regression: every watch safety scan re-reports every persistent skipped
// file, and each report used to WARN again, so one badly named file warned
// every 5 minutes forever. Only a newly skipped file warns.
//
// Validates: R-6.6.30, R-6.6.7
func TestEngineFlow_SkippedFindings_WarnOnlyWhenNew(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	flow := testEngineFlow(t, eng)

	bad := SkippedItem{Path: "docs/bad:name.txt", Reason: IssueInvalidFilename, Detail: "name contains ':'"}
	requireReconcileSkipped(t, flow, []SkippedItem{bad})
	requireReconcileSkipped(t, flow, []SkippedItem{bad})
	requireReconcileSkipped(t, flow, []SkippedItem{bad})

	output := logs.String()
	assert.Equal(t, 1, countLevel(output, "WARN"), "an unchanged skipped file warns once, not on every scan")
	assert.Contains(t, output, "Not syncing docs/bad:name.txt: name contains ':'")

	other := SkippedItem{Path: "docs/other?.txt", Reason: IssueInvalidFilename, Detail: "name contains '?'"}
	requireReconcileSkipped(t, flow, []SkippedItem{bad, other})
	output = logs.String()
	assert.Equal(t, 2, countLevel(output, "WARN"), "a newly skipped file still warns")
	assert.Contains(t, output, "Not syncing docs/other?.txt")
	assert.NotContains(t, output, "action=", "each line is complete; the watch summary names status once")
}

// Regression: eleven invalid names with eleven different problems printed as
// "Not syncing 11 files: the name starts with a space (examples=...)", the
// first file's detail standing in for all of them.
//
// Validates: R-6.6.30, R-6.6.7
func TestEngineFlow_SkippedFindings_GroupedWarningNamesTheWholeGroup(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	flow := testEngineFlow(t, eng)

	var mixed, same []SkippedItem
	for i := range 11 {
		detail := "detail B"
		if i%2 == 0 {
			detail = "detail A"
		}
		mixed = append(mixed, SkippedItem{Path: fmt.Sprintf("mixed/%02d", i), Reason: IssueInvalidFilename, Detail: detail})
		same = append(same, SkippedItem{Path: fmt.Sprintf("same/%02d", i), Reason: IssueInvalidFilename, Detail: "the name ends with a period"})
	}
	requireReconcileSkipped(t, flow, mixed)
	requireReconcileSkipped(t, flow, same)

	output := logs.String()
	assert.Contains(t, output, "Not syncing 11 files with names OneDrive does not allow, such as mixed/00, mixed/01, mixed/02")
	assert.Contains(t, output, "Not syncing 11 files: the name ends with a period, such as same/00, same/01, same/02")
	assert.NotContains(t, output, "examples=")
}

// Regression: the watch condition summary printed its breakdown twice and a
// raw action attribute on the console: "Some files are not syncing: 2 files
// ... (total=2, breakdown=\"2 files ...\", action=\"run onedrive-go status
// for details\")". The message now carries the breakdown and the next step.
//
// Validates: R-6.6.30
func TestWatchConditionSummary_StatesBreakdownAndHintOnce(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	seedObservationIssueRowForTest(t, rt.deps.store, &ObservationIssue{
		Path:      "docs/CON",
		DriveID:   driveid.New(engineTestDriveID),
		IssueType: IssueInvalidFilename,
	})
	rt.logWatchSummary(t.Context())

	output := logs.String()
	assert.Equal(t, 1, countLevel(output, "WARN"))
	assert.Contains(t, output,
		`msg="Some files are not syncing: 1 file with names OneDrive does not allow. Run 'onedrive-go status' for details."`)
	assert.Contains(t, output, "condition_total=1", "the count stays in the log file")
	assert.NotContains(t, output, "breakdown=")
	assert.NotContains(t, output, "action=")
}

// The 10 s maintenance tick writes its debug line "sync conditions changed"
// only when the breakdown differs from the one last written: ticks while the
// same conditions stay write nothing.
//
// Validates: R-6.6.30
func TestWatchConditionSummary_DebugLineOnlyWhenTheBreakdownChanges(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	seedBadName := func(path string) {
		seedObservationIssueRowForTest(t, rt.deps.store, &ObservationIssue{
			Path:      path,
			DriveID:   driveid.New(engineTestDriveID),
			IssueType: IssueInvalidFilename,
		})
	}

	seedBadName("docs/CON")
	for range 3 {
		rt.logWatchSummary(t.Context())
	}
	output := logs.String()
	assert.Equal(t, 1, countLevel(output, "WARN"), "the summary is said when the condition appears")
	assert.Zero(t, strings.Count(output, "sync conditions changed"), "ticks with the same conditions write nothing")

	seedBadName("docs/PRN")
	for range 3 {
		rt.logWatchSummary(t.Context())
	}
	output = logs.String()
	assert.Equal(t, 1, strings.Count(output, "sync conditions changed"), "a tick whose counts moved writes one line")
	assert.Contains(t, output, `breakdown="2 files with names OneDrive does not allow"`)
	assert.Equal(t, 1, countLevel(output, "WARN"), "a count change is not a new kind of condition")
}

// A failed read of the conditions is a housekeeping step that keeps failing:
// it warns once per failure streak, logs its repeats at Debug, and notes its
// recovery once at Info.
//
// Validates: R-6.6.30
func TestWatchConditionSummary_AFailedReadIsLoggedOnce(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	// An issue type no member has: the snapshot read refuses the row.
	db := requireStoreTestRawDB(t, eng.baseline)
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO observation_issues (path, issue_type, scope_key) VALUES ('x.txt', 'not_an_issue_type', '')`)
	require.NoError(t, err)

	for range 3 {
		rt.logWatchSummary(t.Context())
	}
	const failed = `msg="could not read the sync conditions for the watch summary"`
	assert.Equal(t, 1, strings.Count(logs.String(), "level=WARN "+failed), logs.String())
	assert.Equal(t, 2, strings.Count(logs.String(), "level=DEBUG "+failed), logs.String())

	_, err = db.ExecContext(t.Context(), `DELETE FROM observation_issues WHERE path = 'x.txt'`)
	require.NoError(t, err)
	for range 2 {
		rt.logWatchSummary(t.Context())
	}
	assert.Equal(t, 1, strings.Count(logs.String(), `level=INFO msg="reading the sync conditions for the watch summary works again"`),
		logs.String())
	assert.Equal(t, 1, countLevel(logs.String(), "WARN"), "the recovery is not a new failure")
}

// Socket.io is a best-effort wake source; watch keeps polling. A long outage
// used to print one WARN per backoff step (about once a minute forever).
//
// Validates: R-6.6.30
func TestSocketIOWakeSource_RetriesNeverWarn(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	logger, logs := newCapturedLogger()
	sleeps := 0
	source := NewSocketIOWakeSourceWithOptions(&mockSocketIOEndpointFetcher{}, driveid.New(synctest.TestDriveID), SocketIOWakeSourceOptions{
		Logger: logger,
		Clock: &scriptedSleepClock{SyncClock: RealClock{}, sleep: func(context.Context, time.Duration) error {
			sleeps++
			if sleeps >= 5 {
				cancel()
				return context.Canceled
			}
			return nil
		}},
	})

	require.NoError(t, runWithin(t, ctx, "the fifth retry sleep to stop the wake source", func(runCtx context.Context) error {
		return source.Run(runCtx, make(chan struct{}, 1))
	}))

	output := logs.String()
	assert.Zero(t, countLevel(output, "WARN"))
	assert.Equal(t, 1, strings.Count(output, "Instant change notifications from OneDrive are unavailable"),
		"the outage is announced once, not per retry")
}

// Every in-flight transfer that hits the same 429 or full drive re-applies the
// same block scope; each used to print "block scope active" at WARN. The
// scope is now reported once when it starts, throttling at Info.
//
// Validates: R-6.6.30
func TestEngineFlow_BlockScope_ReportedOnceWhenItStarts(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	setupWatchEngine(t, eng)
	flow := testEngineFlow(t, eng)

	// A persisted block scope must own blocked retry work (invariant).
	seedBlockedWork := func(key ScopeKey) {
		require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
			Path:         "blocked-" + key.String() + ".txt",
			ActionType:   ActionUpload,
			ScopeKey:     key,
			Blocked:      true,
			AttemptCount: 1,
		}))
	}

	throttle := SKThrottleDrive(eng.driveID)
	seedBlockedWork(throttle)
	for range 4 {
		require.NoError(t, flow.applyBlockScope(t.Context(), ScopeUpdateResult{
			Block: true, ScopeKey: throttle, ConditionType: IssueRateLimited,
		}))
	}
	output := logs.String()
	assert.Zero(t, countLevel(output, "WARN"), "rate limiting is handled automatically")
	assert.Equal(t, 1, strings.Count(output, "OneDrive is limiting requests"))

	seedBlockedWork(SKQuotaOwn())
	for range 3 {
		require.NoError(t, flow.applyBlockScope(t.Context(), ScopeUpdateResult{
			Block: true, ScopeKey: SKQuotaOwn(), ConditionType: IssueQuotaExceeded,
		}))
	}
	output = logs.String()
	assert.Equal(t, 1, countLevel(output, "WARN"), "a full drive is warned once, in plain words")
	assert.Contains(t, output, "OneDrive storage is full; uploads are paused")
}

// "all observers have exited" used to be logged at ERROR and returned too,
// and the multisync runner logged the returned error again. It is now
// returned once, carrying the cause; cancellation is never reported.
//
// Validates: R-6.6.21, R-6.6.22
func TestWatchRuntime_ObserverExit_ReturnedOnceWithCause(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	rt := newWatchRuntime(eng.Engine)
	rt.resources.activeObservers = 2

	stopped, stop := context.WithCancel(t.Context())
	stop()
	rt.logObserverError(stopped, observerExitError(observerNameRemote, fmt.Errorf("poll: %w", context.Canceled)))
	assert.Zero(t, countLevel(logs.String(), "WARN"), "cancellation is shutdown, not news")

	cause := errors.New("inotify: too many open files")
	rt.logObserverError(t.Context(), observerExitError(observerNameLocal, cause))
	require.NoError(t, rt.handleObserverExit(&watchPipeline{}, false))
	output := logs.String()
	assert.Equal(t, 1, countLevel(output, "WARN"))
	assert.Contains(t, output, "Stopped watching local files for changes")

	err := rt.handleObserverExit(&watchPipeline{}, false)
	require.ErrorIs(t, err, cause, "the returned error names why watching stopped")
	assert.Zero(t, countLevel(logs.String(), "ERROR"), "the returned error is the only report")
}

// The watch condition summary used to fingerprint counts, so it re-warned on
// every count change at the 10 s maintenance tick, including for throttling
// the engine handles itself.
//
// Validates: R-6.6.30
func TestWatchConditionSummary_WarnsOnlyForNewActionableKinds(t *testing.T) {
	t.Parallel()

	throttled := watchConditionSummary{ConditionTotal: 3, Counts: []watchConditionCount{
		{Key: ConditionRateLimited, Count: 3},
	}}
	moreThrottled := watchConditionSummary{ConditionTotal: 9, Counts: []watchConditionCount{
		{Key: ConditionRateLimited, Count: 9},
	}}
	withBadName := watchConditionSummary{ConditionTotal: 10, Counts: []watchConditionCount{
		{Key: ConditionInvalidFilename, Count: 1},
		{Key: ConditionRateLimited, Count: 9},
	}}

	assert.False(t, watchConditionSummaryAddsActionable(throttled, ""), "throttling is never a warning")
	assert.Equal(t, watchConditionSummaryFingerprint(throttled), watchConditionSummaryFingerprint(moreThrottled),
		"a count change alone is not a new report")
	assert.True(t, watchConditionSummaryAddsActionable(withBadName, watchConditionSummaryFingerprint(throttled)))
	assert.False(t, watchConditionSummaryAddsActionable(withBadName, watchConditionSummaryFingerprint(withBadName)))
	assert.Equal(t, "1 file with names OneDrive does not allow, 9 files waiting while OneDrive limits requests",
		formatWatchConditionBreakdown(withBadName))
}

// requireReconcileSkipped commits a scan of no rows with the skipped
// findings, failing the test on error; the change report is not what these
// tests observe.
func requireReconcileSkipped(t *testing.T, flow *engineFlow, skipped []SkippedItem) {
	t.Helper()

	_, err := flow.commitLocalScan(t.Context(), nil, skipped, nil)
	require.NoError(t, err)
}
