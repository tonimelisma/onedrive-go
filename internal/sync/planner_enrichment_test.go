package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

func assertPerSideHashNoLoop(t *testing.T, path, localHash, remoteHash string) {
	t.Helper()

	planner := NewPlanner(synctest.TestLogger(t))
	baseline := baselineWith(&BaselineEntry{
		Path:       path,
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  localHash,
		RemoteHash: remoteHash,
	})

	changes := []PathChanges{{
		Path: path,
		RemoteEvents: []ChangeEvent{{
			Source:   synctypes.SourceRemote,
			Type:     synctypes.ChangeModify,
			Path:     path,
			ItemType: synctypes.ItemTypeFile,
			Hash:     remoteHash,
			ItemID:   "item1",
			DriveID:  driveid.New(synctest.TestDriveID),
		}},
		LocalEvents: []ChangeEvent{{
			Source:   synctypes.SourceLocal,
			Type:     synctypes.ChangeModify,
			Path:     path,
			ItemType: synctypes.ItemTypeFile,
			Hash:     localHash,
		}},
	}}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)
	assert.Empty(t, actionsOfType(plan.Actions, ActionUpload))
	assert.Empty(t, actionsOfType(plan.Actions, ActionDownload))
}

// Validates: R-6.7.4
// TestPerSideHash_PreventsReUploadLoop validates that after uploading a file
// (LocalHash=AAA), if the server modifies it (RemoteHash=BBB), the next sync
// run does NOT produce an upload action because LocalHash still matches.
func TestPerSideHash_PreventsReUploadLoop(t *testing.T) {
	assertPerSideHashNoLoop(t, "enriched.docx", "localHashAAA", "remoteHashBBB")
}

// Validates: R-6.7.25
func TestSharePointExtraVersionAcceptance_PreventsSyncLoop(t *testing.T) {
	assertPerSideHashNoLoop(t, "sharepoint-versioned.docx", "localHashAAA", "remoteHashBBB")
}

// TestPerSideHash_PreventsReDownloadLoop validates the reverse: after
// download, if the local file's hash differs from remote (because server
// enriched it), the next run does NOT produce a download action.
func TestPerSideHash_PreventsReDownloadLoop(t *testing.T) {
	assertPerSideHashNoLoop(t, "enriched.pptx", "downloadedHash", "serverEnrichedHash")
}

// TestPerSideHash_5RunStabilityProof runs 5 planner runs with divergent
// local/remote hashes in the baseline and verifies that zero actions are
// produced in every run. This proves the per-side hash scheme prevents
// infinite sync loops caused by SharePoint enrichment.
func TestPerSideHash_5RunStabilityProof(t *testing.T) {
	baseline := baselineWith(&BaselineEntry{
		Path:       "stable.docx",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "localHashXXX",
		RemoteHash: "remoteHashYYY",
	})

	for run := range 5 {
		planner := NewPlanner(synctest.TestLogger(t))

		changes := []PathChanges{
			{
				Path: "stable.docx",
				RemoteEvents: []ChangeEvent{
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
				LocalEvents: []ChangeEvent{
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

		plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
		require.NoError(t, err, "run %d", run)

		assert.Empty(t, plan.Actions,
			"run %d: divergent per-side hashes should produce zero actions", run)
	}
}

// Validates: R-6.7.17
func TestZeroHashFallback_5RunStabilityProof(t *testing.T) {
	baseline := baselineWith(&BaselineEntry{
		Path:            "hashless.docx",
		DriveID:         driveid.New(synctest.TestDriveID),
		ItemID:          "item1",
		ItemType:        synctypes.ItemTypeFile,
		LocalSize:       0,
		LocalSizeKnown:  true,
		RemoteSize:      0,
		RemoteSizeKnown: true,
		LocalMtime:      100,
		RemoteMtime:     100,
		ETag:            "etag-stable",
	})

	for run := range 5 {
		planner := NewPlanner(synctest.TestLogger(t))

		changes := []PathChanges{
			{
				Path: "hashless.docx",
				RemoteEvents: []ChangeEvent{
					{
						Source:   synctypes.SourceRemote,
						Type:     synctypes.ChangeModify,
						Path:     "hashless.docx",
						ItemType: synctypes.ItemTypeFile,
						ItemID:   "item1",
						DriveID:  driveid.New(synctest.TestDriveID),
						Size:     0,
						Mtime:    100,
						ETag:     "etag-stable",
					},
				},
				LocalEvents: []ChangeEvent{
					{
						Source:   synctypes.SourceLocal,
						Type:     synctypes.ChangeModify,
						Path:     "hashless.docx",
						ItemType: synctypes.ItemTypeFile,
						Size:     0,
						Mtime:    100,
					},
				},
			},
		}

		plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
		require.NoError(t, err, "run %d", run)
		assert.Empty(t, actionsOfType(plan.Actions, ActionUpload), "run %d", run)
		assert.Empty(t, actionsOfType(plan.Actions, ActionDownload), "run %d", run)
	}
}

// TestPerSideHash_NormalDriveUnaffected validates that for normal (non-enriched)
// files where LocalHash == RemoteHash, the per-side hash scheme does not
// interfere with normal change detection.
func TestPerSideHash_NormalDriveUnaffected(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))

	// Normal case: both hashes are the same.
	baseline := baselineWith(&BaselineEntry{
		Path:       "normal.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "sameHash",
		RemoteHash: "sameHash",
	})

	// File actually changed on remote side.
	changes := []PathChanges{
		{
			Path: "normal.txt",
			RemoteEvents: []ChangeEvent{
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Should produce a download because remote hash changed.
	downloads := actionsOfType(plan.Actions, ActionDownload)
	assert.Len(t, downloads, 1,
		"normal drive: remote hash change should produce download")
}
