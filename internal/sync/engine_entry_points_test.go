package sync

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// newCollectingTestEngine is a test engine built with collector, as a sync
// owner builds a mount's engine.
func newCollectingTestEngine(t *testing.T, mock *syncfixture.EngineMockClient, collector *perf.Collector) *testEngine {
	t.Helper()

	eng, _ := newTestEngineInDirWith(t, t.Context(), mock, t.TempDir(), func(inputs *engineInputs) {
		inputs.PerfCollector = collector
	})

	return eng
}

// A one-shot pass called with a context that carries no collector still
// counts its store transactions on the collector the engine was built with:
// the engine does not depend on its owner for its own counts.
//
// Validates: R-6.6.15
func TestRunOnce_CountsOnTheEngineCollectorWithoutAnOwnerWrap(t *testing.T) {
	t.Parallel()

	const content = "alpha!!"

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := newDownloadDeltaMock(
		driveID,
		&graph.Item{
			ID: "item-a", Name: "alpha.txt", ParentID: "root",
			DriveID: driveID, Size: int64(len(content)), QuickXorHash: syncfixture.TestQuickXor(content),
		},
		"token-perf",
		[]byte(content),
	)
	mountCollector := perf.NewCollector(nil)
	eng := newCollectingTestEngine(t, mock, mountCollector)

	report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err, "RunOnce")
	require.Equal(t, 1, report.Downloads, "the pass downloaded the file and committed it to the store")

	snapshot := mountCollector.Snapshot()
	assert.Positive(t, snapshot.DBTransactionCount, "store transactions of the pass")
}

// A watch called with a context that carries no collector counts its store
// transactions on the engine's collector.
//
// Validates: R-6.6.15
func TestRunWatch_CountsOnTheEngineCollectorWithoutAnOwnerWrap(t *testing.T) {
	t.Parallel()

	mountCollector := perf.NewCollector(nil)
	eng := newCollectingTestEngine(t, &syncfixture.EngineMockClient{}, mountCollector)
	recorder := attachDebugEventRecorder(eng)

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncBidirectional, quietWatchOptions())
	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	syncfixture.StopWatch(t, cancel, done)

	snapshot := mountCollector.Snapshot()
	assert.Positive(t, snapshot.DBTransactionCount, "store transactions of the bootstrap")
}

// withOwnCollector, which RunOnce, RunWatch and Close call first, puts the
// engine's collector on a context that carries none or another one, leaves a
// context that already carries it as it is (what is recorded there counts
// once, at every level of the collector's chain), and leaves every context
// alone for an engine without a collector.
//
// Validates: R-6.6.15
func TestEngine_WithOwnCollectorWrapsOnceAndReplacesAnother(t *testing.T) {
	t.Parallel()

	parent := perf.NewCollector(nil)
	own := perf.NewCollector(parent)
	eng := &Engine{perfCollector: own}

	plain := t.Context()
	assert.Same(t, own, perf.FromContext(eng.withOwnCollector(plain)), "a plain context gets the engine's collector")

	other := perf.WithCollector(plain, perf.NewCollector(nil))
	assert.Same(t, own, perf.FromContext(eng.withOwnCollector(other)), "another collector is replaced")

	wrapped := perf.WithCollector(plain, own)
	again := eng.withOwnCollector(wrapped)
	assert.Equal(t, wrapped, again, "a context that carries the engine's collector is returned as it is")
	perf.FromContext(again).RecordDBTransaction(1)
	ownSnapshot, parentSnapshot := own.Snapshot(), parent.Snapshot()
	assert.Equal(t, 1, ownSnapshot.DBTransactionCount, "counted once on the engine's collector")
	assert.Equal(t, 1, parentSnapshot.DBTransactionCount, "and once on its parent")

	none := &Engine{}
	assert.Equal(t, other, none.withOwnCollector(other), "an engine without a collector leaves the context alone")
}

// A startup proof that passes revalidates a stored sign-in requirement, but
// another onedrive-go process may hold the catalog. The engine then logs one
// Warn naming what it could not clear, keeps the stored requirement and
// starts; the next start clears it.
//
// Validates: R-2.10.46
func TestEngineStartup_LockedCatalogWarnsAndStarts(t *testing.T) {
	t.Parallel()

	logger, logs := syncfixture.NewCapturedLogger()
	mock := &syncfixture.EngineMockClient{}
	eng, _ := newTestEngineInDirWith(t, t.Context(), mock, t.TempDir(), func(inputs *engineInputs) {
		inputs.DriveVerifier = mock
		inputs.Logger = logger
	})
	account := testEngineAccountCanonicalID()
	require.NoError(t, config.MarkAccountAuthRequired(t.Context(), eng.dataDir, account, authstate.ReasonSyncAuthRejected))
	eng.hooks.clearAccountAuthRequirement = func(context.Context, string, driveid.CanonicalID) error {
		return fmt.Errorf("updating catalog: %w", config.ErrConfigLocked)
	}

	_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err, "the sync starts although the catalog is locked")
	assert.Equal(t, authstate.ReasonSyncAuthRejected, signInRequirementOf(t, eng, account), "the stored requirement stands")
	assert.Equal(t, 1, syncfixture.CountLevel(logs.String(), "WARN"), "one Warn: %s", logs.String())
	assert.Contains(t, logs.String(), "Could not clear the stored sign-in requirement; the next start clears it")
	assert.Contains(t, logs.String(), account.String(), "the Warn names the account")

	eng.hooks.clearAccountAuthRequirement = nil
	_, err = eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Empty(t, signInRequirementOf(t, eng, account), "the next start clears it")
}

// faultComparisonQuery is a fragment of the planner's comparison query alone.
const faultComparisonQuery = "FROM comparison_flags\nORDER BY path"

// A planning-state load error reads the same in a one-shot pass, the watch's
// bootstrap and a watch replan, keeps its cause, and names the failed query
// once.
//
// Validates: R-6.2.16
func TestPlanningLoadError_SameTextInOneShotAndWatch(t *testing.T) {
	t.Parallel()

	eng, fault := newTestEngineOnFaultStore(t, &syncfixture.EngineMockClient{}, nil)
	ctx := t.Context()
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	fault.Arm(faultComparisonQuery, syncfixture.ErrInjectedFault)
	_, oneShotErr := newOneShotRunner(eng.Engine).runLiveCurrentPlan(ctx, bl, syncdomain.SyncBidirectional, RunOptions{})
	require.ErrorIs(t, oneShotErr, syncfixture.ErrInjectedFault, "one-shot")

	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	fault.Arm(faultComparisonQuery, syncfixture.ErrInjectedFault)
	_, bootstrapErr := rt.runBootstrapCurrentPlan(ctx, bl, syncdomain.SyncBidirectional)
	require.ErrorIs(t, bootstrapErr, syncfixture.ErrInjectedFault, "watch bootstrap")
	fault.Arm(faultComparisonQuery, syncfixture.ErrInjectedFault)
	_, replanErr := rt.runSteadyStateCurrentPlan(ctx, syncdomain.SyncBidirectional)
	require.ErrorIs(t, replanErr, syncfixture.ErrInjectedFault, "watch replan")

	assert.Equal(t, oneShotErr.Error(), bootstrapErr.Error(), "one-shot and watch bootstrap")
	assert.Equal(t, oneShotErr.Error(), replanErr.Error(), "one-shot and watch replan")
	assert.Equal(t, 1, strings.Count(oneShotErr.Error(), "querying comparison state"), oneShotErr.Error())
	assert.Equal(t, 3, fault.Fired(), "each path read the planning state")
}
