package sync

// test_helpers_test.go provides shared test helper functions for the merged
// sync package's engine, observer, planner, and executor tests.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	stdsync "sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

//nolint:gochecknoglobals // sync tests share one schema template cache to amortize repeated store initialization cost.
var (
	testStoreTemplateOnce stdsync.Once
	testStoreTemplateData []byte
	errTestStoreTemplate  error
)

// testDriveID is the canonical drive ID used by engine tests.
const testDriveID = synctest.TestDriveID

// eventWait is how long a test waits for an event that must come: a debug
// event, an observer's change, a pass's result, a goroutine's return. A wait
// returns the moment its event arrives, so a passing test never spends it; it
// only bounds how long a broken one hangs. It is long because events that take
// milliseconds alone took over ten seconds on a loaded two-core host under the
// race detector, and a ten-second wait failed tests there that were correct.
// A wait that proves something does NOT happen is the opposite case: its
// expiry is the passing path, so it stays short, at its use.
const eventWait = time.Minute

// emptyBaseline returns a Baseline with initialized but empty maps.
func emptyBaseline() *Baseline {
	return NewBaselineForTest(nil)
}

// baselineWith creates a Baseline pre-populated with the given entries.
func baselineWith(entries ...*BaselineEntry) *Baseline {
	return NewBaselineForTest(entries)
}

// newBaselineForTest seeds a baseline using the store-owned test helper so
// sync tests stay aligned with the current baseline owner.
func newBaselineForTest(entries []*BaselineEntry) *Baseline {
	return NewBaselineForTest(entries)
}

// testLogger returns a *slog.Logger wired to t.Log for clean test output.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return synctest.TestLogger(t)
}

func newTestLogger(tb testing.TB) *slog.Logger {
	tb.Helper()
	return synctest.TestLogger(tb)
}

func newTestStore(tb testing.TB) *SyncStore {
	tb.Helper()

	dbPath := filepath.Join(tb.TempDir(), "test.db")
	ctx := synctest.TestContext(tb)
	require.NoError(tb, os.WriteFile(dbPath, loadTestStoreTemplate(tb), 0o600), "WriteFile(%q)", dbPath)

	mgr, err := openSyncStore(ctx, dbPath, synctest.TestLogger(tb), false)
	require.NoError(tb, err, "NewSyncStore(%q)", dbPath)

	tb.Cleanup(func() {
		assert.NoError(tb, mgr.Close(context.Background()), "Close()")
	})

	return mgr
}

func setTestDirPermissions(t *testing.T, path string, perms os.FileMode) {
	t.Helper()

	require.NoError(t, os.Chmod(path, perms))
}

// makeReadOnlyForTest takes the owner's write access from dir, so nothing in
// it can be removed, and gives it back when the test ends. The folder stays
// readable and searchable.
func makeReadOnlyForTest(t *testing.T, dir string) {
	t.Helper()

	setTestDirPermissions(t, dir, 0o500)
	t.Cleanup(func() { setTestDirPermissions(t, dir, 0o700) })
}

func mustOpenSyncTree(t *testing.T, path string) *synctree.Root {
	t.Helper()

	tree, err := synctree.Open(path)
	if err != nil {
		panic(fmt.Sprintf("open sync tree %s: %v", path, err))
	}

	return tree
}

// newTestManager creates a SyncStore backed by a temp directory for use in
// engine tests that need database access (shared storage, etc.).
func newTestManager(t *testing.T) *SyncStore {
	t.Helper()

	ctx := synctest.TestContext(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	require.NoError(t, os.WriteFile(dbPath, loadTestStoreTemplate(t), 0o600), "WriteFile(%q)", dbPath)

	mgr, err := openSyncStore(ctx, dbPath, synctest.TestLogger(t), false)
	require.NoError(t, err, "NewSyncStore(%q)", dbPath)

	t.Cleanup(func() {
		assert.NoError(t, mgr.Close(context.Background()), "Close()")
	})

	return mgr
}

func loadTestStoreTemplate(tb testing.TB) []byte {
	tb.Helper()

	testStoreTemplateOnce.Do(func() {
		templateDir, err := os.MkdirTemp("", "sync-store-template-")
		if err != nil {
			errTestStoreTemplate = fmt.Errorf("create sync store template dir: %w", err)
			return
		}
		defer os.RemoveAll(templateDir)

		templatePath := filepath.Join(templateDir, "template.db")
		store, err := NewSyncStore(context.Background(), templatePath, slog.New(slog.DiscardHandler))
		if err != nil {
			errTestStoreTemplate = fmt.Errorf("create sync store template: %w", err)
			return
		}
		if closeErr := store.Close(context.Background()); closeErr != nil {
			errTestStoreTemplate = fmt.Errorf("close sync store template: %w", closeErr)
			return
		}

		//nolint:gosec // templatePath is created by this helper inside a private temp dir.
		testStoreTemplateData, err = os.ReadFile(templatePath)
		if err != nil {
			errTestStoreTemplate = fmt.Errorf("read sync store template: %w", err)
		}
	})

	require.NoError(tb, errTestStoreTemplate)

	return append([]byte(nil), testStoreTemplateData...)
}

func schemaFingerprint(tb testing.TB, db *sql.DB) []string {
	tb.Helper()

	rows, err := db.QueryContext(tb.Context(), `
		SELECT type, name, IFNULL(sql, '')
		FROM sqlite_schema
		WHERE type IN ('table', 'index', 'trigger', 'view')
		ORDER BY type, name
	`)
	require.NoError(tb, err)
	defer func() {
		require.NoError(tb, rows.Close())
	}()

	var result []string
	for rows.Next() {
		var itemType string
		var name string
		var ddl string
		require.NoError(tb, rows.Scan(&itemType, &name, &ddl))
		result = append(result, fmt.Sprintf("%s|%s|%s", itemType, name, ddl))
	}
	require.NoError(tb, rows.Err())

	return result
}

func journalMode(tb testing.TB, db *sql.DB) string {
	tb.Helper()

	var mode string
	require.NoError(tb, db.QueryRowContext(tb.Context(), "PRAGMA journal_mode").Scan(&mode))

	return mode
}

func TestNewTestStore_TemplateMatchesFreshStore(t *testing.T) {
	t.Parallel()

	freshPath := filepath.Join(t.TempDir(), "fresh.db")
	fresh, err := NewSyncStore(t.Context(), freshPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, fresh.Close(context.Background()))
	})

	templateBacked := newTestStore(t)

	assert.Equal(t, schemaFingerprint(t, fresh.rawDB()), schemaFingerprint(t, templateBacked.rawDB()))
	assert.Equal(t, journalMode(t, fresh.rawDB()), journalMode(t, templateBacked.rawDB()))
}

// discardLogger returns a logger that writes to nowhere, suitable for tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// controllableClock returns a deterministic clock and an advance hook for tests
// that need to move time forward explicitly.
func controllableClock() (nowFunc func() time.Time, advance func(d time.Duration)) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return now }, func(d time.Duration) { now = now.Add(d) }
}

// enospcWatcher returns ENOSPC after N successful Add calls.
// Used by engine tests that verify the ENOSPC fallback-to-polling path.
type enospcWatcher struct {
	events       chan fsnotify.Event
	errs         chan error
	addCount     int
	failAfter    int
	closeOne     stdsync.Once
	failOne      stdsync.Once
	failCh       chan struct{}
	addedPaths   []string
	failedPaths  []string
	removedPaths []string
}

func newEnospcWatcher(failAfter int) *enospcWatcher {
	return &enospcWatcher{
		events:    make(chan fsnotify.Event, 10),
		errs:      make(chan error, 10),
		failAfter: failAfter,
		failCh:    make(chan struct{}),
	}
}

func (w *enospcWatcher) Add(name string) error {
	w.addCount++
	if w.addCount > w.failAfter {
		w.failedPaths = append(w.failedPaths, name)
		w.failOne.Do(func() { close(w.failCh) })
		return syscall.ENOSPC
	}

	w.addedPaths = append(w.addedPaths, name)

	return nil
}

type signalingWatcher struct {
	events   chan fsnotify.Event
	errs     chan error
	addOne   stdsync.Once
	addCh    chan struct{}
	closeOne stdsync.Once
}

func newSignalingWatcher() *signalingWatcher {
	return &signalingWatcher{
		events: make(chan fsnotify.Event, 10),
		errs:   make(chan error, 10),
		addCh:  make(chan struct{}),
	}
}

func (w *signalingWatcher) Add(string) error {
	w.addOne.Do(func() { close(w.addCh) })
	return nil
}

func (w *signalingWatcher) Remove(string) error           { return nil }
func (w *signalingWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *signalingWatcher) Errors() <-chan error          { return w.errs }
func (w *signalingWatcher) Added() <-chan struct{}        { return w.addCh }

func (w *signalingWatcher) Close() error {
	w.closeOne.Do(func() { close(w.events); close(w.errs) })
	return nil
}

func (w *enospcWatcher) Remove(name string) error {
	w.removedPaths = append(w.removedPaths, name)
	return nil
}

func (w *enospcWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *enospcWatcher) Errors() <-chan error          { return w.errs }
func (w *enospcWatcher) Failures() <-chan struct{}     { return w.failCh }

func (w *enospcWatcher) Close() error {
	w.closeOne.Do(func() { close(w.events); close(w.errs) })

	return nil
}

// observerTestClock is the observer-side counterpart to manualClock: one clock
// value supplying whatever a test needs to control, with the real clock behind
// it for everything else. Tests set only the hooks they care about, so a test
// that controls the safety-scan tick still gets coherent timers and sleeps
// rather than a mix of controlled and wall-clock time.
type observerTestClock struct {
	syncClock

	// tickCh, when non-nil, backs every ticker this clock hands out.
	tickCh <-chan time.Time
	// sleep, when non-nil, replaces the real sleep.
	sleep func(context.Context, time.Duration) error
	// afterFunc, when non-nil, replaces the real delayed callback.
	afterFunc func(time.Duration, func()) syncTimer
}

func newObserverTestClock() *observerTestClock {
	return &observerTestClock{syncClock: realClock{}}
}

// fixedTicker delivers on a channel the test owns and ignores its interval.
type fixedTicker struct {
	ch <-chan time.Time
}

func (t *fixedTicker) Chan() <-chan time.Time { return t.ch }
func (t *fixedTicker) Stop()                  {}

func (c *observerTestClock) NewTicker(interval time.Duration) syncTicker {
	if c.tickCh != nil {
		return &fixedTicker{ch: c.tickCh}
	}

	return c.syncClock.NewTicker(interval)
}

func (c *observerTestClock) Sleep(ctx context.Context, delay time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, delay)
	}

	//nolint:wrapcheck // decorator delegates to the wrapped clock; adding context here would attribute the failure to the test seam rather than the clock that produced it
	return c.syncClock.Sleep(ctx, delay)
}

func (c *observerTestClock) AfterFunc(delay time.Duration, fn func()) syncTimer {
	if c.afterFunc != nil {
		return c.afterFunc(delay, fn)
	}

	return c.syncClock.AfterFunc(delay, fn)
}

// skipInShort marks an exhaustive test: a breadth, replay, randomized or
// many-pass variant of behavior that targeted tests in this package already
// cover. `go test -short` (the `verify quick` and `verify platform` profiles,
// which are the per-PR CI lane) skips it; the full profiles (`verify default`
// and `verify public`, the local gate and nightly CI) run it. Call it before
// t.Parallel so the skip costs nothing.
func skipInShort(tb testing.TB) {
	tb.Helper()

	if testing.Short() {
		tb.Skip("exhaustive: runs in the full verify profiles and nightly CI")
	}
}

// seedVars names the environment variables of one seeded model family: one
// seed, which wins over the other two, or a run of count consecutive seeds
// from start.
type seedVars struct {
	single string
	count  string
	start  string
}

// setIn reports whether getenv holds any of the family's variables.
func (v seedVars) setIn(getenv func(string) string) bool {
	return getenv(v.single) != "" || getenv(v.count) != "" || getenv(v.start) != ""
}

// seedsFromEnv returns the seeds of one run of a seeded model family: the one
// seed its single variable names, else count consecutive seeds from start,
// defaultCount and 1 where the variables are unset. A count below 1, or a
// value that does not parse, fails the test with one sentence that names the
// variable and its value.
func seedsFromEnv(tb testing.TB, vars seedVars, defaultCount int) []uint64 {
	tb.Helper()

	seeds, err := seedRun(os.Getenv, vars, defaultCount)
	require.NoError(tb, err)

	return seeds
}

// seedRun is the rule of seedsFromEnv over getenv.
func seedRun(getenv func(string) string, vars seedVars, defaultCount int) ([]uint64, error) {
	if raw := getenv(vars.single); raw != "" {
		seed, err := parseSeedVar(vars.single, raw)
		if err != nil {
			return nil, err
		}

		return []uint64{seed}, nil
	}
	count := defaultCount
	if raw := getenv(vars.count); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%s=%q is not a whole number of seeds", vars.count, raw)
		}
		count = parsed
	}
	if count < 1 {
		return nil, fmt.Errorf("%s=%d is below 1, and a seeded run takes at least one seed", vars.count, count)
	}
	start := uint64(1)
	if raw := getenv(vars.start); raw != "" {
		parsed, err := parseSeedVar(vars.start, raw)
		if err != nil {
			return nil, err
		}
		start = parsed
	}
	if start > math.MaxUint64-uint64(count-1) {
		return nil, fmt.Errorf("%s=%d with %d seeds runs past the largest seed", vars.start, start, count)
	}
	seeds := make([]uint64, count)
	for i := range seeds {
		seeds[i] = start + uint64(i)
	}

	return seeds, nil
}

func parseSeedVar(name string, raw string) (uint64, error) {
	seed, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a seed, which is a whole number from 0", name, raw)
	}

	return seed, nil
}

// seedReplayPrefix starts the line a failed seed logs. devtool reads the
// command after it from a go test log, raw or -json, for the nightly report's
// issue, the nightly check's key and `verify failed-test`.
const seedReplayPrefix = "seed-replay: "

// seedReplayPackage is the package a replay runs, from the module root.
const seedReplayPackage = "./internal/sync"

// seedReplayTB is what logSeedReplayOnFailure needs of a test.
type seedReplayTB interface {
	Helper()
	Name() string
	Failed() bool
	Cleanup(cleanup func())
	Logf(format string, args ...any)
}

// seedAssignment is one environment assignment in front of a replay command.
type seedAssignment struct {
	name  string
	value string
}

// logSeedReplayOnFailure registers a cleanup that, when the seed's test
// failed, logs the one command that reruns it:
//
//	seed-replay: <ENV>=<seed> [ONEDRIVE_GO_DISPATCH_ORDER=<order>] go test [-race] -run '^<TopLevelTest>$' ./internal/sync
//
// with -race when this test binary was built with the race detector, under
// which some families do other work for one seed, and the dispatch order when
// the run had one, which replaces the seed's own shuffle (seedReplayInputs).
// Call it first in the seed's subtest: cleanups run last-registered first, so
// the line ends what the subtest logs. A test that go test's deadline ends
// runs no cleanup and prints no line; devtool then builds the replay from the
// subtest's name, seed=<seed>.
func logSeedReplayOnFailure(t seedReplayTB, envName string, seed uint64) {
	t.Helper()

	assignments := seedReplayInputs(os.Getenv, envName, seed)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		test, _, _ := strings.Cut(t.Name(), "/")
		t.Logf("%s", seedReplayLine(test, raceDetectorEnabled, assignments))
	})
}

// seedReplayInputs are the assignments of a seed's replay line, read through
// getenv: the seed in envName, then every other input that changes what the
// seed does and was set, which is the dispatch order alone.
func seedReplayInputs(getenv func(string) string, envName string, seed uint64) []seedAssignment {
	assignments := []seedAssignment{{name: envName, value: strconv.FormatUint(seed, 10)}}
	if order := getenv(dispatchOrderEnv); order != "" {
		assignments = append(assignments, seedAssignment{name: dispatchOrderEnv, value: order})
	}

	return assignments
}

// seedReplayLine is the replay line of a seed of test: the assignments, then
// go test of exactly that top-level test in this package.
func seedReplayLine(test string, race bool, assignments []seedAssignment) string {
	var b strings.Builder
	b.WriteString(seedReplayPrefix)
	for _, assignment := range assignments {
		b.WriteString(assignment.name + "=" + assignment.value + " ")
	}
	b.WriteString("go test ")
	if race {
		b.WriteString("-race ")
	}
	b.WriteString("-run '^" + test + "$' " + seedReplayPackage)

	return b.String()
}
