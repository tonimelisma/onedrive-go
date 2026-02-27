package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// §2: Per-Side Hash / Enrichment Tests
//
// SharePoint document libraries modify files post-upload (injecting metadata),
// which changes the remote hash and sometimes the size. Per-side baseline
// hashes (LocalHash vs RemoteHash) prevent infinite re-sync loops.
// ---------------------------------------------------------------------------

// TestPerSideHash_PreventsReUploadLoop validates that after uploading a file
// (LocalHash=AAA), if the server modifies it (RemoteHash=BBB), the next sync
// cycle does NOT produce an upload action because LocalHash still matches.
func TestPerSideHash_PreventsReUploadLoop(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	// Baseline: local and remote hashes diverge (SharePoint enrichment).
	baseline := baselineWith(&BaselineEntry{
		Path:       "enriched.docx",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "localHashAAA",
		RemoteHash: "remoteHashBBB",
	})

	// Next cycle: local unchanged (still AAA), remote unchanged (still BBB).
	changes := []PathChanges{
		{
			Path: "enriched.docx",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeModify,
					Path:     "enriched.docx",
					ItemType: ItemTypeFile,
					Hash:     "remoteHashBBB",
					ItemID:   "item1",
					DriveID:  driveid.New(testDriveID),
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeModify,
					Path:     "enriched.docx",
					ItemType: ItemTypeFile,
					Hash:     "localHashAAA",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	require.NoError(t, err)

	// No upload, no download — both sides match their respective baselines.
	uploads := ActionsOfType(plan.Actions, ActionUpload)
	downloads := ActionsOfType(plan.Actions, ActionDownload)
	assert.Empty(t, uploads, "no re-upload when local hash matches baseline")
	assert.Empty(t, downloads, "no re-download when remote hash matches baseline")
}

// TestPerSideHash_PreventsReDownloadLoop validates the reverse: after
// download, if the local file's hash differs from remote (because server
// enriched it), the next cycle does NOT produce a download action.
func TestPerSideHash_PreventsReDownloadLoop(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	baseline := baselineWith(&BaselineEntry{
		Path:       "enriched.pptx",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "downloadedHash",
		RemoteHash: "serverEnrichedHash",
	})

	changes := []PathChanges{
		{
			Path: "enriched.pptx",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeModify,
					Path:     "enriched.pptx",
					ItemType: ItemTypeFile,
					Hash:     "serverEnrichedHash",
					ItemID:   "item1",
					DriveID:  driveid.New(testDriveID),
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeModify,
					Path:     "enriched.pptx",
					ItemType: ItemTypeFile,
					Hash:     "downloadedHash",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	require.NoError(t, err)

	downloads := ActionsOfType(plan.Actions, ActionDownload)
	uploads := ActionsOfType(plan.Actions, ActionUpload)
	assert.Empty(t, downloads, "no re-download when remote hash matches baseline")
	assert.Empty(t, uploads, "no re-upload when local hash matches baseline")
}

// TestPerSideHash_5CycleStabilityProof runs 5 planner cycles with divergent
// local/remote hashes in the baseline and verifies that zero actions are
// produced in every cycle. This proves the per-side hash scheme prevents
// infinite sync loops caused by SharePoint enrichment.
func TestPerSideHash_5CycleStabilityProof(t *testing.T) {
	baseline := baselineWith(&BaselineEntry{
		Path:       "stable.docx",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "localHashXXX",
		RemoteHash: "remoteHashYYY",
	})

	for cycle := range 5 {
		planner := NewPlanner(testLogger(t))

		changes := []PathChanges{
			{
				Path: "stable.docx",
				RemoteEvents: []ChangeEvent{
					{
						Source:   SourceRemote,
						Type:     ChangeModify,
						Path:     "stable.docx",
						ItemType: ItemTypeFile,
						Hash:     "remoteHashYYY",
						ItemID:   "item1",
						DriveID:  driveid.New(testDriveID),
					},
				},
				LocalEvents: []ChangeEvent{
					{
						Source:   SourceLocal,
						Type:     ChangeModify,
						Path:     "stable.docx",
						ItemType: ItemTypeFile,
						Hash:     "localHashXXX",
					},
				},
			},
		}

		plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
		require.NoError(t, err, "cycle %d", cycle)

		assert.Empty(t, plan.Actions,
			"cycle %d: divergent per-side hashes should produce zero actions", cycle)
	}
}

// TestPerSideHash_NormalDriveUnaffected validates that for normal (non-enriched)
// files where LocalHash == RemoteHash, the per-side hash scheme does not
// interfere with normal change detection.
func TestPerSideHash_NormalDriveUnaffected(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	// Normal case: both hashes are the same.
	baseline := baselineWith(&BaselineEntry{
		Path:       "normal.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "sameHash",
		RemoteHash: "sameHash",
	})

	// File actually changed on remote side.
	changes := []PathChanges{
		{
			Path: "normal.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeModify,
					Path:     "normal.txt",
					ItemType: ItemTypeFile,
					Hash:     "newRemoteHash",
					ItemID:   "item1",
					DriveID:  driveid.New(testDriveID),
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	require.NoError(t, err)

	// Should produce a download because remote hash changed.
	downloads := ActionsOfType(plan.Actions, ActionDownload)
	assert.Len(t, downloads, 1,
		"normal drive: remote hash change should produce download")
}

// TestDetectLocalChange_UsesLocalHash validates that detectLocalChange
// compares against baseline.LocalHash (not RemoteHash), which is critical
// for the per-side hash scheme.
func TestDetectLocalChange_UsesLocalHash(t *testing.T) {
	t.Run("local_matches_local_hash", func(t *testing.T) {
		view := &PathView{
			Path: "test.txt",
			Local: &LocalState{
				Hash: "localHash",
			},
			Baseline: &BaselineEntry{
				ItemType:   ItemTypeFile,
				LocalHash:  "localHash",
				RemoteHash: "differentRemoteHash",
			},
		}

		assert.False(t, detectLocalChange(view),
			"local hash matching LocalHash should NOT be a change")
	})

	t.Run("local_differs_from_local_hash", func(t *testing.T) {
		view := &PathView{
			Path: "test.txt",
			Local: &LocalState{
				Hash: "newLocalHash",
			},
			Baseline: &BaselineEntry{
				ItemType:   ItemTypeFile,
				LocalHash:  "oldLocalHash",
				RemoteHash: "oldLocalHash",
			},
		}

		assert.True(t, detectLocalChange(view),
			"local hash differing from LocalHash should be a change")
	})
}

// TestDetectRemoteChange_UsesRemoteHash validates that detectRemoteChange
// compares against baseline.RemoteHash (not LocalHash).
func TestDetectRemoteChange_UsesRemoteHash(t *testing.T) {
	t.Run("remote_matches_remote_hash", func(t *testing.T) {
		view := &PathView{
			Path: "test.txt",
			Remote: &RemoteState{
				Hash: "remoteHash",
			},
			Baseline: &BaselineEntry{
				ItemType:   ItemTypeFile,
				LocalHash:  "differentLocalHash",
				RemoteHash: "remoteHash",
			},
		}

		assert.False(t, detectRemoteChange(view),
			"remote hash matching RemoteHash should NOT be a change")
	})

	t.Run("remote_differs_from_remote_hash", func(t *testing.T) {
		view := &PathView{
			Path: "test.txt",
			Remote: &RemoteState{
				Hash: "newRemoteHash",
			},
			Baseline: &BaselineEntry{
				ItemType:   ItemTypeFile,
				LocalHash:  "someHash",
				RemoteHash: "oldRemoteHash",
			},
		}

		assert.True(t, detectRemoteChange(view),
			"remote hash differing from RemoteHash should be a change")
	})
}
