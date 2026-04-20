package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func seedObservedRemoteItemForRetryTest(
	t *testing.T,
	store *SyncStore,
	driveID driveid.ID,
	itemID string,
	path string,
	hash string,
) {
	t.Helper()

	require.NoError(t, store.CommitObservation(t.Context(), []ObservedItem{{
		DriveID:  driveID,
		ItemID:   itemID,
		ParentID: "root",
		Path:     path,
		ItemType: ItemTypeFile,
		Hash:     hash,
	}}, "", driveID))
}

// Validates: R-2.10.33
func TestRetryWorkDriveID_UsesEngineDriveFallback(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)

	driveID, err := flow.retryWorkDriveID(t.Context())
	require.NoError(t, err)
	assert.Equal(t, eng.driveID, driveID)
}

// Validates: R-2.10.33
func TestRetryWorkDriveID_PrefersPersistedConfiguredDrive(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	configuredDriveID := driveid.New("00000000000000ab")

	require.NoError(t, eng.baseline.CommitObservation(t.Context(), nil, "", configuredDriveID))

	driveID, err := flow.retryWorkDriveID(t.Context())
	require.NoError(t, err)
	assert.Equal(t, configuredDriveID, driveID)
}

// Validates: R-2.10.33
func TestBaselineRetryHelpers_MatchRemoteStateAndLookupPath(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("00000000000000cd")
	entry := &BaselineEntry{
		Path:       "mirror.txt",
		DriveID:    driveID,
		ItemID:     "item-1",
		ItemType:   ItemTypeFile,
		RemoteHash: "same-hash",
	}
	remote := &RemoteStateRow{
		DriveID:  driveID,
		ItemID:   "item-1",
		Path:     "mirror.txt",
		ItemType: ItemTypeFile,
		Hash:     "same-hash",
	}

	assert.True(t, baselineEntryMatchesRemoteState(entry, remote))
	assert.False(t, baselineEntryMatchesRemoteState(entry, &RemoteStateRow{
		DriveID:  driveID,
		ItemID:   "item-1",
		Path:     "mirror.txt",
		ItemType: ItemTypeFile,
		Hash:     "different-hash",
	}))
	assert.False(t, baselineEntryMatchesRemoteState(nil, remote))
	assert.False(t, baselineEntryMatchesRemoteState(entry, nil))

	bl := newBaselineForTest([]*BaselineEntry{entry})
	assert.Same(t, entry, baselineEntryForPathInBaseline(bl, "mirror.txt", driveID))
	assert.Nil(t, baselineEntryForPathInBaseline(bl, "missing.txt", driveID))
	assert.Nil(t, baselineEntryForPathInBaseline(bl, "mirror.txt", driveid.New("00000000000000ef")))
	assert.Nil(t, baselineEntryForPathInBaseline(nil, "mirror.txt", driveID))
}

// Validates: R-2.10.33
func TestBuildRetryCandidateFromRetryWork_ResolvesDownloadsAgainstCurrentRemoteState(t *testing.T) {
	t.Parallel()

	t.Run("missing remote state resolves", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t, &engineMockClient{})
		flow := testEngineFlow(t, eng)

		candidate := flow.buildRetryCandidateFromRetryWork(t.Context(), nil, &RetryWorkRow{
			Path:       "missing.txt",
			ActionType: ActionDownload,
		}, eng.driveID)

		assert.True(t, candidate.resolved)
		assert.Nil(t, candidate.skipped)
		assert.NoError(t, candidate.err)
	})

	t.Run("matching remote state resolves", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t, &engineMockClient{})
		flow := testEngineFlow(t, eng)

		seedObservedRemoteItemForRetryTest(t, eng.baseline, eng.driveID, "item-1", "mirror.txt", "same-hash")
		bl := newBaselineForTest([]*BaselineEntry{{
			Path:       "mirror.txt",
			DriveID:    eng.driveID,
			ItemID:     "item-1",
			ItemType:   ItemTypeFile,
			RemoteHash: "same-hash",
		}})

		candidate := flow.buildRetryCandidateFromRetryWork(t.Context(), bl, &RetryWorkRow{
			Path:       "mirror.txt",
			ActionType: ActionDownload,
		}, eng.driveID)

		assert.True(t, candidate.resolved)
		assert.Nil(t, candidate.skipped)
		assert.NoError(t, candidate.err)
	})
}

// Validates: R-2.10.33
func TestBuildRetryCandidateFromRetryWork_ResolvesMissingLocalDelete(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)

	candidate := flow.buildRetryCandidateFromRetryWork(t.Context(), nil, &RetryWorkRow{
		Path:       "gone.txt",
		ActionType: ActionLocalDelete,
	}, eng.driveID)

	assert.True(t, candidate.resolved)
	assert.Nil(t, candidate.skipped)
	assert.NoError(t, candidate.err)
}

// Validates: R-2.10.33
func TestBuildRetryCandidateFromRetryWork_MapsFilteredLocalPathToSkippedItem(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	eng.localRules = LocalObservationRules{RejectSharePointRootForms: true}
	flow := testEngineFlow(t, eng)

	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "forms"), 0o750))

	candidate := flow.buildRetryCandidateFromRetryWork(t.Context(), nil, &RetryWorkRow{
		Path:       "forms",
		ActionType: ActionUpload,
	}, eng.driveID)

	require.NotNil(t, candidate.skipped)
	assert.Equal(t, IssueInvalidFilename, candidate.skipped.Reason)
	assert.False(t, candidate.resolved)
	assert.NoError(t, candidate.err)
}

// Validates: R-2.10.33
func TestRecordRetryTrialSkippedItem_PersistsObservationIssueThroughObservationBatch(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)

	flow.recordRetryTrialSkippedItem(t.Context(), nil, RetryWorkKey{
		Path:       "Private/file.txt",
		ActionType: ActionUpload,
	}, eng.driveID, &SkippedItem{
		Path:   "Private/file.txt",
		Reason: IssueLocalReadDenied,
		Detail: "file not accessible (check filesystem permissions)",
	})

	rows, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "Private/file.txt", rows[0].Path)
	assert.Equal(t, IssueLocalReadDenied, rows[0].IssueType)
	assert.True(t, rows[0].ScopeKey.IsZero())
}

// Validates: R-2.10.33
func TestReconcileRetryTrialObservationResult_ClearsOnlyManagedPath(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	ctx := t.Context()

	writeLocalFile(t, syncRoot, "Private/file.txt", "content")
	seedObservationIssueRowForTest(t, eng.baseline, &ObservationIssue{
		Path:       "Private/file.txt",
		DriveID:    eng.driveID,
		ActionType: ActionUpload,
		IssueType:  IssueLocalReadDenied,
		Error:      "file not accessible",
	})
	seedObservationIssueRowForTest(t, eng.baseline, &ObservationIssue{
		Path:       "Other/file.txt",
		DriveID:    eng.driveID,
		ActionType: ActionUpload,
		IssueType:  IssueLocalReadDenied,
		Error:      "file not accessible",
	})

	flow.reconcileRetryTrialObservationResult(ctx, nil, RetryWorkKey{
		Path:       "Private/file.txt",
		ActionType: ActionUpload,
	}, eng.driveID, "Private/file.txt", &SinglePathObservation{
		Event: &ChangeEvent{
			Source:   SourceLocal,
			Type:     ChangeModify,
			Path:     "Private/file.txt",
			ItemType: ItemTypeFile,
		},
	})

	rows, err := eng.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "Other/file.txt", rows[0].Path)
	assert.Equal(t, IssueLocalReadDenied, rows[0].IssueType)
}

// Validates: R-2.10.33
func TestReconcileRetryTrialObservationResult_ReplacesManagedFileIssueWithBoundaryIssue(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	ctx := t.Context()

	seedObservationIssueRowForTest(t, eng.baseline, &ObservationIssue{
		Path:       "Private/file.txt",
		DriveID:    eng.driveID,
		ActionType: ActionUpload,
		IssueType:  IssueLocalReadDenied,
		Error:      "file not accessible",
	})

	flow.reconcileRetryTrialObservationResult(ctx, nil, RetryWorkKey{
		Path:       "Private/file.txt",
		ActionType: ActionUpload,
	}, eng.driveID, "Private/file.txt", &SinglePathObservation{
		Skipped: &SkippedItem{
			Path:            "Private",
			Reason:          IssueLocalReadDenied,
			Detail:          "directory not accessible (check filesystem permissions)",
			BlocksReadScope: true,
		},
	})

	rows, err := eng.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "Private", rows[0].Path)
	assert.Equal(t, IssueLocalReadDenied, rows[0].IssueType)
	assert.Equal(t, SKPermLocalRead("Private"), rows[0].ScopeKey)

	blocks, err := eng.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, SKPermLocalRead("Private"), blocks[0].Key)
}

// Validates: R-2.10.33
func TestBuildRetryCandidateFromRetryWork_LeavesDistinctRemoteMoveUnresolved(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	writeLocalFile(t, syncRoot, "new-name.txt", "content")

	bl := newBaselineForTest([]*BaselineEntry{{
		Path:     "old-name.txt",
		DriveID:  eng.driveID,
		ItemID:   "item-old",
		ItemType: ItemTypeFile,
	}})

	candidate := flow.buildRetryCandidateFromRetryWork(t.Context(), bl, &RetryWorkRow{
		Path:       "new-name.txt",
		OldPath:    "old-name.txt",
		ActionType: ActionRemoteMove,
	}, eng.driveID)

	assert.False(t, candidate.resolved)
	assert.Nil(t, candidate.skipped)
	assert.NoError(t, candidate.err)
}

// Validates: R-2.10.33
func TestBuildRetryCandidateFromRetryWork_ResolvesRemoteDeleteWhenLocalFileExists(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	writeLocalFile(t, syncRoot, "present.txt", "content")

	bl := newBaselineForTest([]*BaselineEntry{{
		Path:     "present.txt",
		DriveID:  eng.driveID,
		ItemID:   "item-present",
		ItemType: ItemTypeFile,
	}})

	candidate := flow.buildRetryCandidateFromRetryWork(t.Context(), bl, &RetryWorkRow{
		Path:       "present.txt",
		ActionType: ActionRemoteDelete,
	}, eng.driveID)

	assert.True(t, candidate.resolved)
	assert.Nil(t, candidate.skipped)
	assert.NoError(t, candidate.err)
}

// Validates: R-2.10.33
func TestClearStaleRetrySweepRow_ResolvedRetryClearsRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	row := &RetryWorkRow{
		Path:         "stale-download.txt",
		ActionType:   ActionDownload,
		AttemptCount: 1,
		FirstSeenAt:  10,
		LastSeenAt:   20,
	}
	work := retryWorkKeyForRetryWork(row)

	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), row))

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	rt.clearStaleRetrySweepRow(t.Context(), bl, row, work)

	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
	assert.Empty(t, actionableObservationIssuesForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.33
func TestClearStaleRetrySweepRow_SkippedRetryPersistsObservationFindings(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	eng.nowFn = func() time.Time { return time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC) }
	setupWatchEngine(t, eng)
	eng.localRules = LocalObservationRules{RejectSharePointRootForms: true}
	rt := testWatchRuntime(t, eng)
	row := &RetryWorkRow{
		Path:         "forms",
		ActionType:   ActionUpload,
		AttemptCount: 1,
		FirstSeenAt:  30,
		LastSeenAt:   40,
	}

	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "forms"), 0o750))
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), row))

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	rt.clearStaleRetrySweepRow(t.Context(), bl, row, retryWorkKeyForRetryWork(row))

	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
	issues := actionableObservationIssuesForTest(t, eng.baseline, t.Context())
	require.Len(t, issues, 1)
	assert.Equal(t, "forms", issues[0].Path)
	assert.Equal(t, IssueInvalidFilename, issues[0].IssueType)
}

// Validates: R-2.10.5
func TestClearStaleTrialRetryWork_PreservesScopeWhenBlockedRetryWorkRemains(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := SKService()

	setTestBlockScope(t, eng, &BlockScope{
		Key:           scopeKey,
		ConditionType: IssueServiceOutage,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	_, err := eng.baseline.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:          "stale.txt",
		ActionType:    ActionDownload,
		ConditionType: IssueServiceOutage,
		ScopeKey:      scopeKey,
		LastError:     "blocked stale retry",
		Blocked:       true,
	}, nil)
	require.NoError(t, err)
	_, err = eng.baseline.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:          "still-blocked.txt",
		ActionType:    ActionUpload,
		ConditionType: IssueServiceOutage,
		ScopeKey:      scopeKey,
		LastError:     "blocked remaining retry",
		Blocked:       true,
	}, nil)
	require.NoError(t, err)

	rt.clearStaleTrialRetryWork(t.Context(), scopeKey, &RetryWorkRow{
		Path:       "stale.txt",
		ActionType: ActionDownload,
		ScopeKey:   scopeKey,
	})

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "still-blocked.txt", retryRows[0].Path)
	assert.True(t, isTestBlockScopeed(eng, scopeKey))

	block, ok := getTestBlockScope(eng, scopeKey)
	require.True(t, ok)
	assert.WithinDuration(t, eng.nowFn().Add(10*time.Second), block.NextTrialAt, time.Second)
}

// Validates: R-2.10.5
func TestClearStaleTrialRetryWork_DiscardsScopeWhenBlockedRetryWorkDisappears(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := SKService()

	setTestBlockScope(t, eng, &BlockScope{
		Key:           scopeKey,
		ConditionType: IssueServiceOutage,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	_, err := eng.baseline.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:          "stale.txt",
		ActionType:    ActionDownload,
		ConditionType: IssueServiceOutage,
		ScopeKey:      scopeKey,
		LastError:     "blocked stale retry",
		Blocked:       true,
	}, nil)
	require.NoError(t, err)

	rt.clearStaleTrialRetryWork(t.Context(), scopeKey, &RetryWorkRow{
		Path:       "stale.txt",
		ActionType: ActionDownload,
		ScopeKey:   scopeKey,
	})

	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
	assert.False(t, isTestBlockScopeed(eng, scopeKey))
}

// Validates: R-2.10.5
func TestRunTrialDispatch_CleansDueScopesUsingCurrentRetryWorkState(t *testing.T) {
	t.Parallel()

	t.Run("no_blocked_retry_work_discards_scope", func(t *testing.T) {
		t.Parallel()

		eng := newSingleOwnerEngine(t)
		rt := testWatchRuntime(t, eng)
		scopeKey := SKService()

		setTestBlockScope(t, eng, &BlockScope{
			Key:           scopeKey,
			ConditionType: IssueServiceOutage,
			BlockedAt:     eng.nowFn().Add(-time.Minute),
			NextTrialAt:   eng.nowFn().Add(-time.Second),
			TrialInterval: 10 * time.Second,
		})

		bl, err := eng.baseline.Load(t.Context())
		require.NoError(t, err)

		outbox := rt.runTrialDispatch(t.Context(), bl, SyncBidirectional, DefaultSafetyConfig())
		assert.Empty(t, outbox)
		assert.False(t, isTestBlockScopeed(eng, scopeKey))
	})

	t.Run("disappeared_blocked_retry_work_discards_empty_scope", func(t *testing.T) {
		t.Parallel()

		eng := newSingleOwnerEngine(t)
		rt := testWatchRuntime(t, eng)
		scopeKey := SKService()

		setTestBlockScope(t, eng, &BlockScope{
			Key:           scopeKey,
			ConditionType: IssueServiceOutage,
			BlockedAt:     eng.nowFn().Add(-time.Minute),
			NextTrialAt:   eng.nowFn().Add(-time.Second),
			TrialInterval: 10 * time.Second,
		})

		_, err := eng.baseline.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
			Path:          "stale-download.txt",
			ActionType:    ActionDownload,
			ConditionType: IssueServiceOutage,
			ScopeKey:      scopeKey,
			LastError:     "blocked stale retry",
			Blocked:       true,
		}, nil)
		require.NoError(t, err)

		bl, err := eng.baseline.Load(t.Context())
		require.NoError(t, err)

		outbox := rt.runTrialDispatch(t.Context(), bl, SyncBidirectional, DefaultSafetyConfig())
		assert.Empty(t, outbox)
		assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
		assert.False(t, isTestBlockScopeed(eng, scopeKey))
	})
}

// Validates: R-2.10.33
func TestRunRetrierSweep_ClearsStaleRetryWorkWithoutDispatch(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	now := eng.nowFn()

	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:         "stale-download.txt",
		ActionType:   ActionDownload,
		AttemptCount: 1,
		NextRetryAt:  now.Add(-time.Second).UnixNano(),
		FirstSeenAt:  now.Add(-time.Minute).UnixNano(),
		LastSeenAt:   now.UnixNano(),
	}))

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	outbox := rt.runRetrierSweep(t.Context(), bl, SyncBidirectional, DefaultSafetyConfig())
	assert.Empty(t, outbox)
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.33
func TestClearRetryWorkOnSuccess_RemovesResolvedRetryRow(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	now := eng.nowFunc().UnixNano()

	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:         "done.txt",
		ActionType:   ActionUpload,
		AttemptCount: 2,
		NextRetryAt:  now,
		FirstSeenAt:  now - 10,
		LastSeenAt:   now,
	}))

	flow.clearRetryWorkOnSuccess(t.Context(), &ActionCompletion{
		Path:       "done.txt",
		ActionType: ActionUpload,
	})

	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}
