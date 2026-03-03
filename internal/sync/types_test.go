package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// Interface satisfaction checks — compile-time verification that
// *graph.Client implements the consumer-defined interfaces.
var (
	_ DeltaFetcher        = (*graph.Client)(nil)
	_ ItemClient          = (*graph.Client)(nil)
	_ driveops.Downloader = (*graph.Client)(nil)
	_ driveops.Uploader   = (*graph.Client)(nil)
)
