package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.1.7, R-2.17.7
//
// OneDrive renamed a folder while the user edited a file inside it (the
// two-writer live test). The rename is applied locally first and the edit is
// planned only once the move has been made, so one `sync` used to apply the
// rename, report success and leave the edit unuploaded until the next run.
// One RunOnce now finishes it.
func TestRunOnce_OneShotUploadsLocalEditInsideFolderRenamedOnline(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-doc", "root", "doc")
	remote.addFile(t, "item-plan", "item-doc", "plan.txt", "plan v1\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	h.beginRound(t)
	remote.moveAt(t, "doc", "docs-renamed")
	writeLocalFile(t, h.syncRoot, "doc/plan.txt", "plan v2 locally\n")

	report := runScenarioPass(t, h.eng)

	assert.Equal(t, map[string]string{"docs-renamed": "/", "docs-renamed/plan.txt": "plan v2 locally\n"}, remote.tree(),
		"one sync uploads the edit as the same item at its new path")
	assert.Equal(t, "item-plan", requireScenarioRemoteID(t, remote, "docs-renamed/plan.txt"))
	requireScenarioLocalFile(t, h.syncRoot, "docs-renamed/plan.txt", "plan v2 locally\n")
	assert.NoDirExists(t, filepath.Join(h.syncRoot, "doc"))
	assert.Equal(t, 1, report.Moves, "the report covers the rename pass")
	assert.Equal(t, 1, report.Uploads, "and the follow-up pass that uploaded the edit")
	assert.Equal(t, report.Moves+report.Uploads, report.Succeeded)
	assertSyncInvariants(t, h)
	requireScenarioQuietPlan(t, h.eng)
}

// Validates: R-2.1.7, R-2.1.5
//
// A dry run previews one pass and never follows up: it executes nothing, so
// there is nothing a second pass could see differently.
func TestRunOnce_OneShotDryRunPlansOnePass(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-doc", "root", "doc")
	remote.addFile(t, "item-plan", "item-doc", "plan.txt", "plan v1\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	h.beginRound(t)
	remote.moveAt(t, "doc", "docs-renamed")
	writeLocalFile(t, h.syncRoot, "doc/plan.txt", "plan v2 locally\n")

	report, err := h.eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Moves)
	assert.Zero(t, report.Uploads, "the edit is planned only after the rename, which a dry run never makes")
	assert.Empty(t, remote.moves)
	assert.Zero(t, remote.uploads)
	_, err = os.Stat(filepath.Join(h.syncRoot, "doc", "plan.txt"))
	require.NoError(t, err, "a dry run moves nothing")
}

func requireScenarioRemoteID(t *testing.T, remote *moveScenarioRemote, remotePath string) string {
	t.Helper()

	remote.mu.Lock()
	defer remote.mu.Unlock()
	item, ok := remote.itemAt(remotePath)
	require.True(t, ok, "OneDrive has %s", remotePath)

	return item.ID
}

// Validates: R-2.1.7, R-2.8.1
//
// Watch mode reaches the same follow-up state by itself. The bootstrap pass
// applies OneDrive's folder rename before the observers start, so no
// filesystem event reports it; the edit inside the folder must still reach
// OneDrive without waiting for an unrelated change or the hourly poll.
func TestRunWatch_FinishesWorkTheBootstrapMovesLeaveForTheNextPass(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-doc", "root", "doc")
	remote.addFile(t, "item-plan", "item-doc", "plan.txt", "plan v1\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	remote.moveAt(t, "doc", "docs-renamed")
	writeLocalFile(t, h.syncRoot, "doc/plan.txt", "plan v2 locally\n")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- h.eng.RunWatch(ctx, SyncBidirectional, WatchOptions{PollInterval: time.Hour, Debounce: 5 * time.Millisecond})
	}()

	require.Eventually(t, func() bool {
		return remote.tree()["docs-renamed/plan.txt"] == "plan v2 locally\n"
	}, 10*time.Second, 10*time.Millisecond, "the edit reaches OneDrive with no further event")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return after cancellation")
	}
	requireScenarioLocalFile(t, h.syncRoot, "docs-renamed/plan.txt", "plan v2 locally\n")
}

// Validates: R-2.1.7, R-2.8.1, R-2.17.6
//
// The same rename on both sides re-keys the baseline and nothing else; the
// OneDrive edit that came with it is planned by the next pass. Nothing local
// changes, so no filesystem event follows: watch mode must run that pass by
// itself rather than leave the edit until an unrelated change.
func TestRunWatch_FinishesTheRemoteEditBehindARekey(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-file", "root", "a.txt", "alpha\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	require.NoError(t, os.Rename(filepath.Join(h.syncRoot, "a.txt"), filepath.Join(h.syncRoot, "b.txt")))
	remote.move("item-file", "root", "b.txt")
	remote.setContent(t, "item-file", "edited online\n")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- h.eng.RunWatch(ctx, SyncBidirectional, WatchOptions{PollInterval: time.Hour, Debounce: 5 * time.Millisecond})
	}()

	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(h.syncRoot, "b.txt"))
		return err == nil && string(data) == "edited online\n"
	}, 10*time.Second, 10*time.Millisecond, "the remote edit is downloaded with no further event")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return after cancellation")
	}
	assert.Zero(t, remote.uploads)
}

// plannedFollowUp plans current truth without executing it and returns what
// the plan leaves for the next pass.
func plannedFollowUp(t *testing.T, eng *testEngine) planFollowUp {
	t.Helper()

	runner := newOneShotRunner(eng.Engine)
	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	runtime, err := runner.runDryRunCurrentPlan(t.Context(), bl, SyncBidirectional, RunOptions{DryRun: true})
	require.NoError(t, err)

	return runtime.Plan.FollowUp
}

// Validates: R-2.1.7
//
// The planner says which plans leave work for the next pass: a converged
// tree leaves none, a rename online over a local edit leaves the edit (the
// move is counted), and a swap online parks one item for the next pass.
func TestPlanFollowUp_CountsWorkLeftForTheNextPass(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-doc", "root", "doc")
	remote.addFile(t, "item-plan", "item-doc", "plan.txt", "plan v1\n")
	remote.addFile(t, "item-a", "root", "a.txt", "A\n")
	remote.addFile(t, "item-b", "root", "b.txt", "B\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	assert.Equal(t, planFollowUp{}, plannedFollowUp(t, h.eng), "a converged tree leaves nothing")
	assert.False(t, planFollowUp{}.pending())

	remote.moveAt(t, "doc", "docs-renamed")
	writeLocalFile(t, h.syncRoot, "doc/plan.txt", "plan v2 locally\n")
	assert.Equal(t, planFollowUp{Renames: 1}, plannedFollowUp(t, h.eng), "the folder move leaves the edit")

	remote.moveAt(t, "a.txt", "swap.txt")
	remote.moveAt(t, "b.txt", "a.txt")
	remote.moveAt(t, "swap.txt", "b.txt")
	f := plannedFollowUp(t, h.eng)
	assert.Equal(t, 1, f.Deferred, "the parked item finishes next pass")
	assert.Equal(t, 3, f.Renames, "the folder move, the park and the move into the freed name")
	assert.True(t, f.pending())
}

// Validates: R-2.1.7
//
// A one-shot run's report adds up the work of every pass, and reports what
// is still held back as the last pass saw it.
func TestReport_WithPassAddsWorkAndKeepsTheLastHeldState(t *testing.T) {
	t.Parallel()

	errFirst := assert.AnError
	first := &Report{
		Mode: SyncUploadOnly, Moves: 1, Uploads: 2, ConflictCopies: 1, Succeeded: 4, Failed: 1,
		Errors: []error{errFirst}, DeferredByMode: DeferredCounts{Downloads: 3}, HeldRemoteDeletes: 2,
	}
	second := &Report{
		Mode: SyncUploadOnly, Uploads: 1, FolderCreates: 1, Succeeded: 2,
		DeferredByMode: DeferredCounts{Downloads: 3}, HeldLocalDeletes: 1,
	}

	var run *Report
	run = run.withPass(nil)
	assert.Nil(t, run, "a run with no pass report has none")
	run = run.withPass(first)
	require.NotNil(t, run)
	run = run.withPass(second)

	assert.Equal(t, SyncUploadOnly, run.Mode)
	assert.Equal(t, 1, run.Moves)
	assert.Equal(t, 3, run.Uploads)
	assert.Equal(t, 1, run.FolderCreates)
	assert.Equal(t, 1, run.ConflictCopies)
	assert.Equal(t, 6, run.Succeeded)
	assert.Equal(t, 1, run.Failed)
	assert.Equal(t, []error{errFirst}, run.Errors)
	assert.Equal(t, DeferredCounts{Downloads: 3}, run.DeferredByMode, "held back by the mode once, not once per pass")
	assert.Zero(t, run.HeldRemoteDeletes)
	assert.Equal(t, 1, run.HeldLocalDeletes)
	assert.Equal(t, 4, first.Succeeded, "the first pass's own report is not changed")
}

// Validates: R-2.1.7
//
// Another pass runs only for planned follow-up work that a pass can finish:
// not without progress, not after a failure (it waits in retry backoff), not
// once the run is canceled, and not past the pass bound.
func TestOneShotFollowUp_StopsWithoutProgressOnFailureAndAtTheBound(t *testing.T) {
	t.Parallel()

	eng := &Engine{logger: testLogger(t)}
	leaves := planFollowUp{Renames: 1}
	ok := &Report{Succeeded: 1}
	tests := []struct {
		name     string
		canceled bool
		pass     int
		result   oneShotPass
		want     bool
	}{
		{name: "planned follow-up", pass: 1, result: oneShotPass{report: ok, followUp: leaves}, want: true},
		{name: "nothing left", pass: 1, result: oneShotPass{report: ok}},
		{name: "no progress", pass: 1, result: oneShotPass{report: &Report{}, followUp: leaves}},
		{name: "a failure", pass: 1, result: oneShotPass{report: &Report{Succeeded: 3, Failed: 1}, followUp: leaves}},
		{name: "canceled", canceled: true, pass: 1, result: oneShotPass{report: ok, followUp: leaves}},
		{name: "last allowed pass", pass: oneShotMaxPasses - 1, result: oneShotPass{report: ok, followUp: leaves}, want: true},
		{name: "pass bound", pass: oneShotMaxPasses, result: oneShotPass{report: ok, followUp: leaves}},
	}
	for _, tt := range tests {
		ctx, cancel := context.WithCancel(t.Context())
		if tt.canceled {
			cancel()
		}
		reason, again := eng.oneShotFollowUp(ctx, tt.pass, tt.result)
		assert.Equal(t, tt.want, again, tt.name)
		assert.Equal(t, tt.want, reason != "", tt.name)
		cancel()
	}
}
