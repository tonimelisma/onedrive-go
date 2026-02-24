package sync

import (
	"testing"

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
		if got := tt.it.String(); got != tt.want {
			t.Errorf("ItemType(%d).String() = %q, want %q", int(tt.it), got, tt.want)
		}
	}
}

func TestItemType_StringUnknown(t *testing.T) {
	t.Parallel()

	got := ItemType(99).String()
	if got == "" {
		t.Error("unknown ItemType.String() returned empty string")
	}
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
		if err != nil {
			t.Errorf("ParseItemType(%q) error: %v", tt.input, err)
			continue
		}

		if got != tt.want {
			t.Errorf("ParseItemType(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestParseItemType_Error(t *testing.T) {
	t.Parallel()

	_, err := ParseItemType("unknown")
	if err == nil {
		t.Error("ParseItemType(\"unknown\") expected error, got nil")
	}
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
		if tt.str == "" {
			t.Errorf("%s.String() returned empty string", tt.name)
		}
	}
}

// Interface satisfaction checks — compile-time verification that
// *graph.Client implements the consumer-defined interfaces.
var (
	_ DeltaFetcher = (*graph.Client)(nil)
	_ ItemClient   = (*graph.Client)(nil)
	_ Downloader   = (*graph.Client)(nil)
	_ Uploader     = (*graph.Client)(nil)
)
