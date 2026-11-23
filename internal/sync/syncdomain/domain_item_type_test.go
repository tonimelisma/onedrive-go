package syncdomain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The item type of a publication, a delete outcome and a move is read from
// the side known to be observed, remote then local then baseline, over the
// shapes the planner's producers build. A file is a type like any other: an
// observed file never yields to a folder the baseline recorded.
//
// Validates: R-2.17.4
func TestResolveItemType_ReadsTheObservedSideOverTheProducersShapes(t *testing.T) {
	t.Parallel()

	folderBaseline := &BaselineEntry{Path: "docs", ItemID: "folder-1", ItemType: ItemTypeFolder}
	fileBaseline := &BaselineEntry{Path: "docs", ItemID: "file-1", ItemType: ItemTypeFile}
	remoteFolder := &RemoteState{ItemID: "folder-1", ItemType: ItemTypeFolder}
	remoteFile := &RemoteState{ItemID: "file-2", ItemType: ItemTypeFile, Hash: "rh"}
	// A local folder carries no hash: the scanner hashes files only.
	localFolder := &LocalState{Name: "docs", ItemType: ItemTypeFolder}
	localFile := &LocalState{Name: "docs", ItemType: ItemTypeFile, Hash: "lh"}

	testCases := []struct {
		name        string
		action      Action
		resolved    ItemType
		deletedType ItemType
	}{
		{
			name:        "cleanup of a folder neither side holds reads the baseline",
			action:      Action{Type: ActionCleanup, View: &PathView{Path: "docs", Baseline: folderBaseline}},
			resolved:    ItemTypeFolder,
			deletedType: ItemTypeFolder,
		},
		{
			name:        "cleanup of a file reads the baseline",
			action:      Action{Type: ActionCleanup, View: &PathView{Path: "docs", Baseline: fileBaseline}},
			resolved:    ItemTypeFile,
			deletedType: ItemTypeFile,
		},
		{
			name: "baseline update of a synced folder",
			action: Action{Type: ActionBaselineUpdate, View: &PathView{
				Path: "docs", Remote: remoteFolder, Local: localFolder, Baseline: folderBaseline,
			}},
			resolved:    ItemTypeFolder,
			deletedType: ItemTypeFolder,
		},
		{
			name: "baseline update of a folder local truth holds and remote truth does not",
			action: Action{Type: ActionBaselineUpdate, View: &PathView{
				Path: "docs", Local: localFolder, Baseline: folderBaseline,
			}},
			resolved:    ItemTypeFolder,
			deletedType: ItemTypeFolder,
		},
		{
			name: "baseline update of a folder both sides replaced with the same file is that file",
			action: Action{Type: ActionBaselineUpdate, View: &PathView{
				Path: "docs", Remote: remoteFile, Local: localFile, Baseline: folderBaseline,
			}},
			resolved:    ItemTypeFile,
			deletedType: ItemTypeFile,
		},
		{
			name: "local delete of a file OneDrive replaced with a folder deletes the local file",
			action: Action{Type: ActionLocalDelete, View: &PathView{
				Path: "docs", Remote: remoteFolder, Local: localFile, Baseline: fileBaseline,
			}},
			resolved:    ItemTypeFolder,
			deletedType: ItemTypeFile,
		},
		{
			name: "remote delete of a folder the user replaced with a file deletes the remote folder",
			action: Action{Type: ActionRemoteDelete, View: &PathView{
				Path: "docs", Remote: remoteFolder, Local: localFile, Baseline: folderBaseline,
			}},
			resolved:    ItemTypeFolder,
			deletedType: ItemTypeFolder,
		},
		{
			name: "remote delete of a synced folder the planner sees only in the baseline",
			action: Action{Type: ActionRemoteDelete, View: &PathView{
				Path: "docs", Baseline: folderBaseline,
			}},
			resolved:    ItemTypeFolder,
			deletedType: ItemTypeFolder,
		},
		{
			name:        "a move with no baseline reads its observed side",
			action:      Action{Type: ActionRemoteMove, View: &PathView{Path: "docs", Local: localFolder}},
			resolved:    ItemTypeFolder,
			deletedType: ItemTypeFolder,
		},
		{
			name:        "no view is a file",
			action:      Action{Type: ActionCleanup},
			resolved:    ItemTypeFile,
			deletedType: ItemTypeFile,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.resolved, ResolveItemType(tc.action.View))
			assert.Equal(t, tc.deletedType, DeletedItemType(&tc.action))
		})
	}
}
