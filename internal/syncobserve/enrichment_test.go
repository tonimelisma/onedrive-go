package syncobserve

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// §2: Per-Side Hash / Enrichment Tests
//
// SharePoint document libraries modify files post-upload (injecting metadata),
// which changes the remote hash and sometimes the size. Per-side baseline
// hashes (LocalHash vs RemoteHash) prevent infinite re-sync loops.
// ---------------------------------------------------------------------------

// Validates: R-6.7.4
// TestPerSideHash_PreventsReUploadLoop validates that after uploading a file
// (LocalHash=AAA), if the server modifies it (RemoteHash=BBB), the next sync
// run does NOT produce an upload action because LocalHash still matches.
func TestPerSideHash_PreventsReUploadLoop(t *testing.T) {
	planner := syncplan.NewPlanner(synctest.TestLogger(t))

	// Baseline: local and remote hashes diverge (SharePoint enrichment).
	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "enriched.docx",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "localHashAAA",
		RemoteHash: "remoteHashBBB",
	})

	// Next run: local unchanged (still AAA), remote unchanged (still BBB).
	changes := []synctypes.PathChanges{
		{
			Path: "enriched.docx",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeModify,
					Path:     "enriched.docx",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "remoteHashBBB",
					ItemID:   "item1",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeModify,
					Path:     "enriched.docx",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "localHashAAA",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// No upload, no download — both sides match their respective baselines.
	uploads := syncplan.ActionsOfType(plan.Actions, synctypes.ActionUpload)
	downloads := syncplan.ActionsOfType(plan.Actions, synctypes.ActionDownload)
	assert.Empty(t, uploads, "no re-upload when local hash matches baseline")
	assert.Empty(t, downloads, "no re-download when remote hash matches baseline")
}

// TestPerSideHash_PreventsReDownloadLoop validates the reverse: after
// download, if the local file's hash differs from remote (because server
// enriched it), the next run does NOT produce a download action.
func TestPerSideHash_PreventsReDownloadLoop(t *testing.T) {
	planner := syncplan.NewPlanner(synctest.TestLogger(t))

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "enriched.pptx",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "downloadedHash",
		RemoteHash: "serverEnrichedHash",
	})

	changes := []synctypes.PathChanges{
		{
			Path: "enriched.pptx",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeModify,
					Path:     "enriched.pptx",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "serverEnrichedHash",
					ItemID:   "item1",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeModify,
					Path:     "enriched.pptx",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "downloadedHash",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	downloads := syncplan.ActionsOfType(plan.Actions, synctypes.ActionDownload)
	uploads := syncplan.ActionsOfType(plan.Actions, synctypes.ActionUpload)
	assert.Empty(t, downloads, "no re-download when remote hash matches baseline")
	assert.Empty(t, uploads, "no re-upload when local hash matches baseline")
}

// TestPerSideHash_5RunStabilityProof runs 5 planner runs with divergent
// local/remote hashes in the baseline and verifies that zero actions are
// produced in every run. This proves the per-side hash scheme prevents
// infinite sync loops caused by SharePoint enrichment.
func TestPerSideHash_5RunStabilityProof(t *testing.T) {
	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "stable.docx",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "localHashXXX",
		RemoteHash: "remoteHashYYY",
	})

	for run := range 5 {
		planner := syncplan.NewPlanner(synctest.TestLogger(t))

		changes := []synctypes.PathChanges{
			{
				Path: "stable.docx",
				RemoteEvents: []synctypes.ChangeEvent{
					{
						Source:   synctypes.SourceRemote,
						Type:     synctypes.ChangeModify,
						Path:     "stable.docx",
						ItemType: synctypes.ItemTypeFile,
						Hash:     "remoteHashYYY",
						ItemID:   "item1",
						DriveID:  driveid.New(synctest.TestDriveID),
					},
				},
				LocalEvents: []synctypes.ChangeEvent{
					{
						Source:   synctypes.SourceLocal,
						Type:     synctypes.ChangeModify,
						Path:     "stable.docx",
						ItemType: synctypes.ItemTypeFile,
						Hash:     "localHashXXX",
					},
				},
			},
		}

		plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
		require.NoError(t, err, "run %d", run)

		assert.Empty(t, plan.Actions,
			"run %d: divergent per-side hashes should produce zero actions", run)
	}
}

// TestPerSideHash_NormalDriveUnaffected validates that for normal (non-enriched)
// files where LocalHash == RemoteHash, the per-side hash scheme does not
// interfere with normal change detection.
func TestPerSideHash_NormalDriveUnaffected(t *testing.T) {
	planner := syncplan.NewPlanner(synctest.TestLogger(t))

	// Normal case: both hashes are the same.
	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "normal.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "sameHash",
		RemoteHash: "sameHash",
	})

	// File actually changed on remote side.
	changes := []synctypes.PathChanges{
		{
			Path: "normal.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeModify,
					Path:     "normal.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "newRemoteHash",
					ItemID:   "item1",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Should produce a download because remote hash changed.
	downloads := syncplan.ActionsOfType(plan.Actions, synctypes.ActionDownload)
	assert.Len(t, downloads, 1,
		"normal drive: remote hash change should produce download")
}

// TestDetectLocalChange_UsesLocalHash validates that DetectLocalChange
// compares against baseline.LocalHash (not RemoteHash), which is critical
// for the per-side hash scheme.
func TestDetectLocalChange_UsesLocalHash(t *testing.T) {
	t.Run("local_matches_local_hash", func(t *testing.T) {
		view := &synctypes.PathView{
			Path: "test.txt",
			Local: &synctypes.LocalState{
				Hash: "localHash",
			},
			Baseline: &synctypes.BaselineEntry{
				ItemType:   synctypes.ItemTypeFile,
				LocalHash:  "localHash",
				RemoteHash: "differentRemoteHash",
			},
		}

		assert.False(t, syncplan.DetectLocalChange(view),
			"local hash matching LocalHash should NOT be a change")
	})

	t.Run("local_differs_from_local_hash", func(t *testing.T) {
		view := &synctypes.PathView{
			Path: "test.txt",
			Local: &synctypes.LocalState{
				Hash: "newLocalHash",
			},
			Baseline: &synctypes.BaselineEntry{
				ItemType:   synctypes.ItemTypeFile,
				LocalHash:  "oldLocalHash",
				RemoteHash: "oldLocalHash",
			},
		}

		assert.True(t, syncplan.DetectLocalChange(view),
			"local hash differing from LocalHash should be a change")
	})
}

// TestDetectRemoteChange_UsesRemoteHash validates that DetectRemoteChange
// compares against baseline.RemoteHash (not LocalHash).
func TestDetectRemoteChange_UsesRemoteHash(t *testing.T) {
	t.Run("remote_matches_remote_hash", func(t *testing.T) {
		view := &synctypes.PathView{
			Path: "test.txt",
			Remote: &synctypes.RemoteState{
				Hash: "remoteHash",
			},
			Baseline: &synctypes.BaselineEntry{
				ItemType:   synctypes.ItemTypeFile,
				LocalHash:  "differentLocalHash",
				RemoteHash: "remoteHash",
			},
		}

		assert.False(t, syncplan.DetectRemoteChange(view),
			"remote hash matching RemoteHash should NOT be a change")
	})

	t.Run("remote_differs_from_remote_hash", func(t *testing.T) {
		view := &synctypes.PathView{
			Path: "test.txt",
			Remote: &synctypes.RemoteState{
				Hash: "newRemoteHash",
			},
			Baseline: &synctypes.BaselineEntry{
				ItemType:   synctypes.ItemTypeFile,
				LocalHash:  "someHash",
				RemoteHash: "oldRemoteHash",
			},
		}

		assert.True(t, syncplan.DetectRemoteChange(view),
			"remote hash differing from RemoteHash should be a change")
	})
}
