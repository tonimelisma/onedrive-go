package synctypes

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func TestItemType_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		it   ItemType
		want string
	}{
		{ItemTypeFile, "file"},
		{ItemTypeFolder, "folder"},
		{ItemTypeRoot, "root"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.it.String())
	}
}

func TestItemType_StringUnknown(t *testing.T) {
	t.Parallel()

	got := ItemType(99).String()
	assert.NotEmpty(t, got, "unknown ItemType.String() returned empty string")
}

func TestParseItemType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  ItemType
	}{
		{"file", ItemTypeFile},
		{"folder", ItemTypeFolder},
		{"root", ItemTypeRoot},
	}

	for _, tt := range tests {
		got, err := ParseItemType(tt.input)
		require.NoError(t, err, "ParseItemType(%q)", tt.input)
		assert.Equal(t, tt.want, got, "ParseItemType(%q)", tt.input)
	}
}

func TestParseItemType_Error(t *testing.T) {
	t.Parallel()

	_, err := ParseItemType("unknown")
	require.Error(t, err, "ParseItemType(\"unknown\") expected error")
}

func TestEnumStrings_NonEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		str  string
	}{
		{"SourceRemote", SourceRemote.String()},
		{"SourceLocal", SourceLocal.String()},
		{"ChangeCreate", ChangeCreate.String()},
		{"ChangeModify", ChangeModify.String()},
		{"ChangeDelete", ChangeDelete.String()},
		{"ChangeMove", ChangeMove.String()},
		{"SyncBidirectional", SyncBidirectional.String()},
		{"SyncDownloadOnly", SyncDownloadOnly.String()},
		{"SyncUploadOnly", SyncUploadOnly.String()},
		{"ActionDownload", ActionDownload.String()},
		{"ActionUpload", ActionUpload.String()},
		{"ActionLocalDelete", ActionLocalDelete.String()},
		{"ActionRemoteDelete", ActionRemoteDelete.String()},
		{"ActionLocalMove", ActionLocalMove.String()},
		{"ActionRemoteMove", ActionRemoteMove.String()},
		{"ActionFolderCreate", ActionFolderCreate.String()},
		{"ActionConflict", ActionConflict.String()},
		{"ActionUpdateSynced", ActionUpdateSynced.String()},
		{"ActionCleanup", ActionCleanup.String()},
		{"CreateLocal", CreateLocal.String()},
		{"CreateRemote", CreateRemote.String()},
	}

	for _, tt := range tests {
		assert.NotEmpty(t, tt.str, "%s.String() returned empty string", tt.name)
	}
}

func TestActionTypeDirection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		action ActionType
		want   Direction
	}{
		{action: ActionDownload, want: DirectionDownload},
		{action: ActionUpload, want: DirectionUpload},
		{action: ActionLocalDelete, want: DirectionDelete},
		{action: ActionRemoteDelete, want: DirectionDelete},
		{action: ActionLocalMove, want: DirectionDownload},
		{action: ActionRemoteMove, want: DirectionDownload},
		{action: ActionFolderCreate, want: DirectionDownload},
		{action: ActionConflict, want: DirectionDownload},
		{action: ActionUpdateSynced, want: DirectionDownload},
		{action: ActionCleanup, want: DirectionDownload},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.action.Direction())
	}
}

// ---------------------------------------------------------------------------
// Baseline.DescendantsOf tests
// ---------------------------------------------------------------------------

func TestBaseline_DescendantsOf_BasicPrefixMatching(t *testing.T) {
	t.Parallel()

	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "docs", ItemType: ItemTypeFolder, ItemID: "d1"},
		{Path: "docs/readme.txt", ItemType: ItemTypeFile, ItemID: "d2"},
		{Path: "docs/sub", ItemType: ItemTypeFolder, ItemID: "d3"},
		{Path: "docs/sub/deep.txt", ItemType: ItemTypeFile, ItemID: "d4"},
		{Path: "other.txt", ItemType: ItemTypeFile, ItemID: "o1"},
	})

	descendants := bl.DescendantsOf("docs")
	assert.Len(t, descendants, 3, "docs has 3 descendants")

	paths := make(map[string]bool)
	for _, d := range descendants {
		paths[d.Path] = true
	}

	assert.True(t, paths["docs/readme.txt"])
	assert.True(t, paths["docs/sub"])
	assert.True(t, paths["docs/sub/deep.txt"])
	assert.False(t, paths["other.txt"])
	assert.False(t, paths["docs"]) // prefix itself excluded
}

func TestBaseline_DescendantsOf_PrefixOfAnotherName(t *testing.T) {
	t.Parallel()

	// "docs" should NOT match "documents/file.txt" — only "docs/" prefix.
	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "docs", ItemType: ItemTypeFolder, ItemID: "d1"},
		{Path: "documents", ItemType: ItemTypeFolder, ItemID: "doc1"},
		{Path: "documents/file.txt", ItemType: ItemTypeFile, ItemID: "doc2"},
		{Path: "docs/real-child.txt", ItemType: ItemTypeFile, ItemID: "d2"},
	})

	descendants := bl.DescendantsOf("docs")
	require.Len(t, descendants, 1)
	assert.Equal(t, "docs/real-child.txt", descendants[0].Path)
}

func TestBaseline_DescendantsOf_EmptyResults(t *testing.T) {
	t.Parallel()

	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "lonely-folder", ItemType: ItemTypeFolder, ItemID: "l1"},
		{Path: "other.txt", ItemType: ItemTypeFile, ItemID: "o1"},
	})

	descendants := bl.DescendantsOf("lonely-folder")
	assert.Empty(t, descendants)
}

func TestBaseline_PutGetByIDDeleteAndLen(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("drive1")
	original := &BaselineEntry{
		Path:     "docs/readme.txt",
		DriveID:  driveID,
		ItemID:   "item-1",
		ItemType: ItemTypeFile,
	}

	bl := NewBaselineForTest(nil)
	bl.Put(original)

	assert.Equal(t, 1, bl.Len())

	gotByPath, ok := bl.GetByPath(original.Path)
	require.True(t, ok)
	assert.Equal(t, original, gotByPath)

	gotByID, ok := bl.GetByID(driveid.NewItemKey(driveID, original.ItemID))
	require.True(t, ok)
	assert.Equal(t, original, gotByID)

	replacement := &BaselineEntry{
		Path:     original.Path,
		DriveID:  driveID,
		ItemID:   "item-2",
		ItemType: ItemTypeFile,
	}
	bl.Put(replacement)

	_, ok = bl.GetByID(driveid.NewItemKey(driveID, original.ItemID))
	assert.False(t, ok, "stale item ID should be removed when a path is reassigned")

	gotByID, ok = bl.GetByID(driveid.NewItemKey(driveID, replacement.ItemID))
	require.True(t, ok)
	assert.Equal(t, replacement, gotByID)

	var seenPaths []string
	bl.ForEachPath(func(path string, _ *BaselineEntry) {
		seenPaths = append(seenPaths, path)
	})
	assert.Equal(t, []string{replacement.Path}, seenPaths)

	bl.Delete(replacement.Path)
	assert.Equal(t, 0, bl.Len())

	_, ok = bl.GetByPath(replacement.Path)
	assert.False(t, ok)

	_, ok = bl.GetByID(driveid.NewItemKey(driveID, replacement.ItemID))
	assert.False(t, ok)

	assert.Empty(t, bl.GetCaseVariants("docs", "readme.txt"))
}

// Interface satisfaction checks — compile-time verification that
// *graph.Client implements the consumer-defined interfaces.
var (
	_ DeltaFetcher        = (*graph.Client)(nil)
	_ ItemClient          = (*graph.Client)(nil)
	_ driveops.Downloader = (*graph.Client)(nil)
	_ driveops.Uploader   = (*graph.Client)(nil)
)
