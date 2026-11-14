package multisync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// Validates: R-2.4.8, R-6.6.28
//
// A clean final drain lets the parent remove the removed shared folder's
// emptied root, so the drain is clean only when the child's report says
// nothing holds the tree: nothing left by its last pass, not a dry run, no
// delete of a local file held, no file an interrupted removal set aside and
// nothing its release kept, which the blocked detail names. Sums over the
// run's passes (Failed, Errors) decide nothing: a failure an earlier pass had
// and a later pass redid leaves nothing.
func TestClassifyShortcutChildDrainReport_CleanOnlyWhenTheLocalTreeMayGo(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		report syncengine.Report
		clean  bool
		detail string
	}{
		{
			name: "work left for the next sync", report: syncengine.Report{Uploads: 2, Succeeded: 1, LeftForNextSync: 1},
			detail: "1 to be retried on the next sync",
		},
		{
			name: "an issue that keeps a file unsynced", report: syncengine.Report{UnsyncedIssues: 2},
			detail: "2 still not synced",
		},
		{
			name: "a failure of the last pass", report: syncengine.Report{Uploads: 1, Failed: 1, LastPassFailed: 1},
			detail: "1 failed",
		},
		{name: "a failure only an earlier pass had", clean: true, report: syncengine.Report{
			Uploads: 2, Succeeded: 2, Failed: 1, Errors: []error{errors.New("upload throttled")},
		}},
		{name: "a dry run", report: syncengine.Report{DryRun: true}, detail: "dry run"},
		{
			name: "held deletes of local files", report: syncengine.Report{HeldLocalDeletes: 3},
			detail: "3 local deletes held for confirmation",
		},
		{name: "held deletes of files on OneDrive alone", clean: true, report: syncengine.Report{HeldRemoteDeletes: 3}},
		{
			name: "a file an interrupted removal set aside", report: syncengine.Report{InterruptedRemovals: 1},
			detail: "1 file set aside by an interrupted removal",
		},
		{
			name: "a release that kept content sync does not sync",
			report: syncengine.Report{KeptNotOnOneDrive: syncengine.KeptEntries{
				Count: 4, Examples: []string{"Docs/link", "Docs/pipe", ".notes"},
			}},
			detail: "folder still contains files that are not on OneDrive (Docs/link, Docs/pipe, .notes and 1 more); " +
				"move or delete them to finish",
		},
		{
			name: "a release that kept changes it could not upload",
			report: syncengine.Report{UnsyncedIssues: 1, KeptNotUploaded: syncengine.KeptEntries{
				Count: 1, Examples: []string{"Docs/edited.txt"},
			}},
			detail: "1 change could not be uploaded because the shared folder can no longer be reached (Docs/edited.txt)",
		},
		{
			name: "a release that kept a folder that cannot be written to",
			report: syncengine.Report{KeptNotWritable: syncengine.KeptEntries{
				Count: 1, Examples: []string{"Docs"},
			}},
			detail: "1 folder cannot be written to (Docs); make it writable to finish",
		},
		{
			name: "a release that kept a synced item its delete could not remove",
			report: syncengine.Report{KeptNotRemoved: syncengine.KeptEntries{
				Count: 1, Examples: []string{"Docs/locked.txt"},
			}},
			detail: "1 synced item could not be moved to the trash or deleted (Docs/locked.txt); " +
				"remove it by hand or fix what keeps it, then sync again",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			report := tc.report
			status, detail := classifyShortcutChildDrainReport(&MountReport{Report: &report})
			if tc.clean {
				assert.Equal(t, shortcutChildDrainClean, status, detail)
				return
			}
			assert.Equal(t, shortcutChildDrainBlocked, status)
			assert.Contains(t, detail, tc.detail)
		})
	}
}

// Validates: R-2.4.8
//
// A final drain whose child run left work without a failed action keeps the
// removed shared folder's local copy: the parent is not told the drain
// finished, so it releases nothing, and the child's state is not purged.
func TestRunOnce_FinalDrainChildThatLeftWorkKeepsItsLocalCopy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report syncengine.Report
	}{
		{name: "left for the next sync", report: syncengine.Report{Uploads: 1, LeftForNextSync: 1}},
		{name: "an issue that keeps a file unsynced", report: syncengine.Report{UnsyncedIssues: 1}},
		{name: "a release that kept a symbolic link", report: syncengine.Report{
			KeptNotOnOneDrive: syncengine.KeptEntries{Count: 1, Examples: []string{"link"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())

			parent := testStandaloneMount(t, "personal:final-drain-left@example.com", "Parent")
			child := testChildRecord(t, mountID(parent.CanonicalID.String()), "binding-drain", "Shortcuts/Docs")
			child.Mode = syncengine.ShortcutChildRunModeFinalDrain
			childID := child.ChildMountID
			childRoot := filepath.Join(parent.SyncRoot, "Shortcuts", "Docs")
			child.Engine.LocalRoot = childRoot
			require.NoError(t, os.MkdirAll(childRoot, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(childRoot, "local-change.txt"), []byte("not uploaded"), 0o600))

			cfg := testOrchestratorConfig(t, parent)
			cfg.Runtime.TokenSourceFn = stubTokenSourceFn
			seedShortcutChildStateArtifactsForTest(t, cfg.DataDir, &parent, &child, false)
			orch := NewOrchestrator(cfg)
			var acks atomic.Int32
			orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
				return &mockEngine{
					ackDrainFn: func(context.Context, syncengine.ShortcutChildDrainAck) (syncengine.ShortcutChildWorkSnapshot, error) {
						acks.Add(1)
						return syncengine.ShortcutChildWorkSnapshot{}, nil
					},
					runOnceFn: func(ctx context.Context, mode syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
						if req.Mount.projectionKind() == MountProjectionStandalone {
							if err := req.Mount.shortcutChildWorkSink()(ctx, processSnapshot(parent.CanonicalID.String(), child)); err != nil {
								return nil, err
							}
							return &syncengine.Report{Mode: mode}, nil
						}
						report := tc.report
						report.Mode = mode
						return &report, nil
					},
				}, nil
			}

			result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

			require.Len(t, result.Reports, 2)
			assert.Zero(t, acks.Load(), "the parent is not told the drain finished")
			assert.FileExists(t, filepath.Join(childRoot, "local-change.txt"), "the local copy is kept")
			assert.FileExists(t, config.MountStatePathForDataDir(cfg.DataDir, childID), "the child's state is kept")
		})
	}
}

// Validates: R-2.4.8
//
// A final drain runs bidirectional with a full reconcile whatever mode the
// command asked for, in one-shot (buildEngineWork) and in watch
// (runWatchRunnerEngine) alike, so no change in mode lets work a one-way
// mode holds back, which is not work left, stay unsynced under a clean drain.
func TestFinalDrain_RunsBidirectionalFullReconcileInOneShotAndWatch(t *testing.T) {
	t.Parallel()

	type runCall struct {
		mode syncengine.SyncMode
		opts syncengine.RunOptions
	}
	var calls []runCall
	engine := &mockEngine{
		runOnceFn: func(_ context.Context, mode syncengine.SyncMode, opts syncengine.RunOptions) (*syncengine.Report, error) {
			calls = append(calls, runCall{mode: mode, opts: opts})
			return &syncengine.Report{Mode: mode}, nil
		},
		runWatchFn: func(context.Context, syncengine.SyncMode, syncengine.WatchOptions) error {
			return errors.New("a final drain does not watch")
		},
	}
	orch := NewOrchestrator(testOrchestratorConfig(t))
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return engine, nil
	}
	mount := testFinalDrainMount(t, "child-modes", "binding-modes")

	work, err := orch.buildEngineWork(t.Context(), mount, nil, syncengine.SyncUploadOnly, syncengine.RunOptions{})
	require.NoError(t, err)
	_, err = work.fn(t.Context())
	require.NoError(t, err)
	_, err = orch.runWatchRunnerEngine(t.Context(), mount, engine, syncengine.SyncDownloadOnly, syncengine.WatchOptions{})
	require.NoError(t, err)

	require.Len(t, calls, 2, "one-shot and watch each run the drain once")
	for _, call := range calls {
		assert.Equal(t, syncengine.SyncBidirectional, call.mode)
		assert.True(t, call.opts.FullReconcile)
		assert.True(t, call.opts.ReleaseLocalTree, "the drain ends by releasing the local tree")
	}
}
