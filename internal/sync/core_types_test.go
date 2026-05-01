package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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
		{"ActionConflictCopy", ActionConflictCopy.String()},
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
		{action: ActionConflictCopy, want: DirectionDownload},
		{action: ActionUpdateSynced, want: DirectionDownload},
		{action: ActionCleanup, want: DirectionDownload},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.action.Direction())
	}
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

	gotByID, ok := bl.GetByID(original.ItemID)
	require.True(t, ok)
	assert.Equal(t, original, gotByID)

	replacement := &BaselineEntry{
		Path:     original.Path,
		DriveID:  driveID,
		ItemID:   "item-2",
		ItemType: ItemTypeFile,
	}
	bl.Put(replacement)

	_, ok = bl.GetByID(original.ItemID)
	assert.False(t, ok, "stale item ID should be removed when a path is reassigned")

	gotByID, ok = bl.GetByID(replacement.ItemID)
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

	_, ok = bl.GetByID(replacement.ItemID)
	assert.False(t, ok)

	assert.Empty(t, bl.GetCaseVariants("docs", "readme.txt"))
}
