package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	stdsync "sync"
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

// syncLockedBuffer is a bytes.Buffer safe for slog handlers written from
// several goroutines.
type syncLockedBuffer struct {
	mu  stdsync.Mutex // guards buf
	buf bytes.Buffer
}

func (b *syncLockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n, err := b.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("write log buffer: %w", err)
	}

	return n, nil
}

func (b *syncLockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func newCapturedLogger() (*slog.Logger, *syncLockedBuffer) {
	buf := &syncLockedBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func countLevel(output, level string) int {
	return strings.Count(output, "level="+level+" ")
}

// Validates: R-6.6.22
func TestEngineFlow_FatalAuth_ReturnedOnceNotLogged(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	flow := testEngineFlow(t, eng)

	decision := &resultDecision{
		Class:        errclass.ClassFatal,
		ConditionKey: ConditionAuthenticationRequired,
		TrialHint:    trialHintFatal,
	}
	for _, path := range []string{"first.txt", "second.txt", "third.txt"} {
		_, err := flow.applyNormalCompletionDecision(t.Context(), decision, nil, &actionCompletion{
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
	completion := &actionCompletion{Path: "a.txt", ActionType: ActionUpload}

	flow.logSuppressedShutdownCompletionError(completion, fmt.Errorf("commit outcome: %w", context.Canceled))
	flow.logSuppressedShutdownCompletionError(completion, fatalResultError(completion))
	assert.Zero(t, countLevel(logs.String(), "WARN"), "a clean shutdown produces no warnings")

	flow.logSuppressedShutdownCompletionError(completion, errors.New("disk I/O error"))
	output := logs.String()
	assert.Equal(t, 1, countLevel(output, "WARN"), "a real bookkeeping failure while stopping stays visible")
	assert.Contains(t, output, "Could not record the result for a.txt while stopping")
}

// Validates: R-6.6.29
func TestPollOutageLog_WarnsOncePerOutage(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	var outage pollOutageLog

	for range 5 {
		outage.failed(logger, "Can't check OneDrive for changes; retrying", errors.New("dial tcp: no route"), 0)
	}
	outage.recovered(logger, "Checking OneDrive for changes works again")
	outage.recovered(logger, "Checking OneDrive for changes works again")
	outage.failed(logger, "Can't check OneDrive for changes; retrying", errors.New("dial tcp: no route"), 0)

	output := logs.String()
	assert.Equal(t, 2, countLevel(output, "WARN"), "one warning per outage, not per attempt")
	assert.Equal(t, 4, countLevel(output, "DEBUG"), "repeats inside one outage stay at debug")
	assert.Equal(t, 1, countLevel(output, "INFO"), "recovery is reported once, only after an outage")
}

// Regression: every watch safety scan re-reports every persistent skipped
// file, and each report used to WARN again, so one badly named file warned
// every 5 minutes forever. Only a newly skipped file warns.
//
// Validates: R-6.6.29
func TestEngineFlow_SkippedFindings_WarnOnlyWhenNew(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	flow := testEngineFlow(t, eng)

	bad := skippedItem{Path: "docs/bad:name.txt", Reason: IssueInvalidFilename, Detail: "name contains ':'"}
	require.NoError(t, flow.reconcileSkippedObservationFindings(t.Context(), []skippedItem{bad}))
	require.NoError(t, flow.reconcileSkippedObservationFindings(t.Context(), []skippedItem{bad}))
	require.NoError(t, flow.reconcileSkippedObservationFindings(t.Context(), []skippedItem{bad}))

	output := logs.String()
	assert.Equal(t, 1, countLevel(output, "WARN"), "an unchanged skipped file warns once, not on every scan")
	assert.Contains(t, output, "Not syncing docs/bad:name.txt: name contains ':'")

	other := skippedItem{Path: "docs/other?.txt", Reason: IssueInvalidFilename, Detail: "name contains '?'"}
	require.NoError(t, flow.reconcileSkippedObservationFindings(t.Context(), []skippedItem{bad, other}))
	output = logs.String()
	assert.Equal(t, 2, countLevel(output, "WARN"), "a newly skipped file still warns")
	assert.Contains(t, output, "Not syncing docs/other?.txt")
}

// A file that stats but cannot be opened used to warn "hash computation
// failed" on every safety scan forever. It is now a skipped local_read_denied
// item (reported once, listed by status) and the scan logs nothing at WARN.
//
// Validates: R-6.6.29
func TestFullScan_UnreadableFileIsSkippedItemNotWarning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "locked.txt", "secret")

	logger, logs := newCapturedLogger()
	obs := newLocalObserver(emptyBaseline(), logger, 0)
	obs.hashFunc = func(string) (string, error) {
		return "", fmt.Errorf("open locked.txt: %w", os.ErrPermission)
	}

	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err)

	require.Len(t, result.Skipped, 1)
	assert.Equal(t, "locked.txt", result.Skipped[0].Path)
	assert.Equal(t, issueLocalReadDenied, result.Skipped[0].Reason)
	assert.Zero(t, countLevel(logs.String(), "WARN"))
}

// Socket.io is a best-effort wake source; watch keeps polling. A long outage
// used to print one WARN per backoff step (about once a minute forever).
//
// Validates: R-6.6.29
func TestSocketIOWakeSource_RetriesNeverWarn(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	logger, logs := newCapturedLogger()
	sleeps := 0
	source := newSocketIOWakeSourceWithOptions(&mockSocketIOEndpointFetcher{}, driveid.New(synctest.TestDriveID), socketIOWakeSourceOptions{
		Logger: logger,
		SleepFunc: func(context.Context, time.Duration) error {
			sleeps++
			if sleeps >= 5 {
				cancel()
				return context.Canceled
			}
			return nil
		},
	})

	require.NoError(t, source.Run(ctx, make(chan struct{}, 1)))

	output := logs.String()
	assert.Zero(t, countLevel(output, "WARN"))
	assert.Equal(t, 1, strings.Count(output, "Instant change notifications from OneDrive are unavailable"),
		"the outage is announced once, not per retry")
}

// Every in-flight transfer that hits the same 429 or full drive re-applies the
// same block scope; each used to print "block scope active" at WARN. The
// scope is now reported once when it starts, throttling at Info.
//
// Validates: R-6.6.29
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
		require.NoError(t, flow.applyBlockScope(t.Context(), scopeUpdateResult{
			Block: true, ScopeKey: throttle, ConditionType: issueRateLimited,
		}))
	}
	output := logs.String()
	assert.Zero(t, countLevel(output, "WARN"), "rate limiting is handled automatically")
	assert.Equal(t, 1, strings.Count(output, "OneDrive is limiting requests"))

	seedBlockedWork(SKQuotaOwn())
	for range 3 {
		require.NoError(t, flow.applyBlockScope(t.Context(), scopeUpdateResult{
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

	rt.logObserverError(observerExitError(observerNameRemote, fmt.Errorf("poll: %w", context.Canceled)))
	assert.Zero(t, countLevel(logs.String(), "WARN"), "cancellation is shutdown, not news")

	cause := errors.New("inotify: too many open files")
	rt.logObserverError(observerExitError(observerNameLocal, cause))
	require.NoError(t, rt.handleObserverExit(&watchPipeline{}, false))
	output := logs.String()
	assert.Equal(t, 1, countLevel(output, "WARN"))
	assert.Contains(t, output, "Stopped watching local files for changes")

	err := rt.handleObserverExit(&watchPipeline{}, false)
	require.ErrorIs(t, err, cause, "the returned error names why watching stopped")
	assert.Zero(t, countLevel(logs.String(), "ERROR"), "the returned error is the only report")
}
