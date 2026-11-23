package sync

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// heldDeletesOf is the held set the store records now, as the engine reads it.
func heldDeletesOf(t *testing.T, eng *testEngine) []syncdomain.HeldDelete {
	t.Helper()

	return syncstore.HeldDeletesFromRows(readHeldDeletes(t, eng))
}

// A confirmation names the held set the user was shown. The run's next plan
// releases exactly that set: deletes that appeared after the user looked are
// judged by the hold again, and a confirmation of a set that is no longer the
// one held is refused and releases nothing.
//
// Validates: R-6.2.17, R-6.4.2
func TestHeldDeleteConfirmation_ReleasesOnlyTheSetItNames(t *testing.T) {
	t.Parallel()

	remote := seedDeleteSafetyRemote(t, 200)
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	for i := range 70 {
		require.NoError(t, os.Remove(deleteSafetyFilePath(syncRoot, i)))
	}
	report := runScenarioPass(t, eng)
	require.Equal(t, 70, report.HeldRemoteDeletes)
	shown := syncdomain.ConfirmationOf(heldDeletesOf(t, eng))
	require.Equal(t, 70, shown.Count)

	// After the user saw the 70 and before the pass that applies them, 70
	// more go.
	for i := 70; i < 140; i++ {
		require.NoError(t, os.Remove(deleteSafetyFilePath(syncRoot, i)))
	}

	ctx, end := eng.beginRun(t.Context())
	runner := newOneShotRunner(eng.Engine)
	require.NoError(t, runner.acceptHeldDeleteConfirmation(ctx, shown))
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	pass, err := eng.runOneShotLivePass(ctx, runner, bl, syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, end(err))
	require.NotNil(t, pass.report)

	assert.Len(t, remote.deletes, 70, "only the confirmed set is deleted on OneDrive")
	assert.Equal(t, 70, pass.report.HeldRemoteDeletes, "the deletes made after the user looked are held again")
	assert.Nil(t, runner.confirmedHeldDeletes, "a confirmation is released by one plan only")
	heldNow := heldDeletesOf(t, eng)
	require.Len(t, heldNow, 70)
	assert.Equal(t, "Docs/f071.txt", heldNow[0].Path)

	// The first set is gone; confirming it again names a set that is not
	// the one held now.
	stale := newOneShotRunner(eng.Engine)
	err = stale.acceptHeldDeleteConfirmation(t.Context(), shown)
	require.ErrorIs(t, err, ErrHeldDeletesChanged)
	assert.Contains(t, err.Error(), "70 were shown, 70 are held now")
	assert.Nil(t, stale.confirmedHeldDeletes, "a refused confirmation keeps nothing to release")
	assert.Len(t, remote.deletes, 70)
}

// The watch loop takes a confirmation only for the set held now: a matching
// one is kept for the next plan and asks for a replan; a stale one and one
// for a drive that holds nothing are refused and ask for nothing.
//
// Validates: R-6.2.17
func TestWatchHeldDeleteConfirm_AcceptsOnlyTheHeldSet(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(syncfixture.TestLogger(t))
	ctx := t.Context()

	confirm := func(confirmation syncdomain.HeldDeleteConfirmation) error {
		request := heldDeleteConfirmRequest{confirmation: confirmation, answer: make(chan error, 1)}
		rt.handleHeldDeleteConfirm(ctx, request)
		return <-request.answer
	}

	require.ErrorIs(t, confirm(syncdomain.HeldDeleteConfirmation{Count: 1, Token: "0123456789abcdef"}), ErrNoHeldDeletes)
	assert.Nil(t, rt.resources.dirtyBuf.FlushImmediate(), "a refused confirmation asks for no replan")

	held := []syncdomain.HeldDelete{
		{Path: "Docs/a.txt", Side: syncdomain.DeleteSafetySideRemote, Reason: syncdomain.DeleteSafetyReasonTooMany, ItemType: syncdomain.ItemTypeFile, ItemID: "item-a"},
		{Path: "Docs/b.txt", Side: syncdomain.DeleteSafetySideRemote, Reason: syncdomain.DeleteSafetyReasonTooMany, ItemType: syncdomain.ItemTypeFile, ItemID: "item-b"},
	}
	require.NoError(t, eng.baseline.ReplaceHeldDeletes(ctx, held[:1]))
	shownBefore := syncdomain.ConfirmationOf(held[:1])
	require.NoError(t, eng.baseline.ReplaceHeldDeletes(ctx, held))

	require.ErrorIs(t, confirm(shownBefore), ErrHeldDeletesChanged, "a delete was held after the user looked")
	assert.Nil(t, rt.resources.dirtyBuf.FlushImmediate())
	assert.Nil(t, rt.confirmedHeldDeletes)

	require.NoError(t, confirm(syncdomain.ConfirmationOf(held)))
	assert.NotNil(t, rt.resources.dirtyBuf.FlushImmediate(), "a taken confirmation asks for the replan that applies it")

	approved, err := rt.approvedHeldDeletes(ctx, RunOptions{})
	require.NoError(t, err)
	assert.ElementsMatch(t, held, approved, "the next plan releases exactly the confirmed set")
	approved, err = rt.approvedHeldDeletes(ctx, RunOptions{})
	require.NoError(t, err)
	assert.Nil(t, approved, "the plan after it releases nothing")
}

// A confirmation that no running watch takes is refused, never kept for a
// later run.
//
// Validates: R-6.2.17
func TestEngine_ConfirmHeldDeletes_WithoutARunningWatchIsNotTaken(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := eng.ConfirmHeldDeletes(ctx, syncdomain.HeldDeleteConfirmation{Count: 1, Token: "0123456789abcdef"})
	require.ErrorIs(t, err, ErrConfirmationNotTaken)
}
