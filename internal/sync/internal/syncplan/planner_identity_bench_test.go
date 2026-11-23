package syncplan

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// BenchmarkPlanIdentityKeyed times the identity-keyed shadow planner, which
// every watch replan of a bidirectional, unfiltered drive runs beside the
// current planner, over a synced tree of 1k and 10k files in folders of
// benchIdentityFilesPerFolder, where every tenth file was edited here and
// every tenth (another one) on OneDrive. It is the number to read against
// the replan's own planning time before the shadow plan is kept on.
//
// Run with: go test ./internal/sync/internal/syncplan -run '^$' -bench 'BenchmarkPlanIdentityKeyed' -benchtime 1x

const (
	benchIdentityFilesPerFolder = 50
	// benchIdentityEditEvery is the stride of edited files on each side.
	benchIdentityEditEvery = 10
)

// Validates: R-2.3.1
func BenchmarkPlanIdentityKeyed(b *testing.B) {
	for _, fileCount := range []int{1_000, 10_000} {
		b.Run(fmt.Sprintf("files=%d", fileCount), func(b *testing.B) {
			in := benchIdentityInputs(fileCount)

			b.ResetTimer()
			for b.Loop() {
				plan, err := PlanIdentityKeyed(in)
				require.NoError(b, err)
				b.ReportMetric(float64(len(plan.Actions)), "actions")
			}
		})
	}
}

// benchIdentityInputs is a synced tree of fileCount files with the edits
// BenchmarkPlanIdentityKeyed describes.
func benchIdentityInputs(fileCount int) *IdentityInputs {
	driveID := driveid.New("bench-drive")
	entries := make([]*syncdomain.BaselineEntry, 0, fileCount+fileCount/benchIdentityFilesPerFolder)
	local := make([]syncdomain.LocalStateRow, 0, cap(entries))
	remote := make([]syncdomain.RemoteStateRow, 0, cap(entries))
	folders := (fileCount + benchIdentityFilesPerFolder - 1) / benchIdentityFilesPerFolder
	for f := range folders {
		folder := fmt.Sprintf("folder-%04d", f)
		folderID := "id-" + folder
		entries = append(entries, &syncdomain.BaselineEntry{
			Path: folder, DriveID: driveID, ItemID: folderID, ItemType: syncdomain.ItemTypeFolder,
		})
		local = append(local, syncdomain.LocalStateRow{Path: folder, ItemType: syncdomain.ItemTypeFolder})
		remote = append(remote, syncdomain.RemoteStateRow{
			DriveID: driveID, ItemID: folderID, Path: folder, ItemType: syncdomain.ItemTypeFolder,
		})
	}
	for i := range fileCount {
		path := fmt.Sprintf("folder-%04d/file-%06d.txt", i/benchIdentityFilesPerFolder, i)
		itemID := fmt.Sprintf("id-file-%06d", i)
		hash := fmt.Sprintf("hash-%06d", i)
		entries = append(entries, &syncdomain.BaselineEntry{
			Path: path, DriveID: driveID, ItemID: itemID, ItemType: syncdomain.ItemTypeFile,
			LocalHash: hash, RemoteHash: hash, LocalSize: 1, LocalSizeKnown: true, RemoteSize: 1, RemoteSizeKnown: true,
		})
		localHash, remoteHash := hash, hash
		switch i % benchIdentityEditEvery {
		case 0:
			localHash = hash + "-edited-here"
		case 1:
			remoteHash = hash + "-edited-online"
		}
		local = append(local, syncdomain.LocalStateRow{Path: path, ItemType: syncdomain.ItemTypeFile, Hash: localHash, Size: 1})
		remote = append(remote, syncdomain.RemoteStateRow{
			DriveID: driveID, ItemID: itemID, Path: path, ItemType: syncdomain.ItemTypeFile,
			Hash: remoteHash, Size: 1, SizeKnown: true,
		})
	}

	return &IdentityInputs{
		Baseline: syncdomain.NewBaselineSnapshot(entries),
		Local:    local,
		Remote:   remote,
		DriveID:  driveID,
	}
}
