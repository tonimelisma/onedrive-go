package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContentFilter_IncludedDirsIncludeAncestorsRootsAndDescendants(t *testing.T) {
	filter := newContentFilter(ContentFilterConfig{
		IncludedDirs: []string{"Projects/App"},
	})

	assert.True(t, filter.Visible("Projects", ItemTypeFolder))
	assert.True(t, filter.Visible("Projects/App", ItemTypeFolder))
	assert.True(t, filter.Visible("Projects/App/main.go", ItemTypeFile))
	assert.False(t, filter.Visible("Projects/Other", ItemTypeFolder))
	assert.False(t, filter.Visible("Projects", ItemTypeFile))
	assert.False(t, filter.Visible("Notes.txt", ItemTypeFile))
}

// Validates: R-2.4.11
func TestContentFilter_ShouldObserveUnknownKindIncludesDirectoryCapablePaths(t *testing.T) {
	filter := newContentFilter(ContentFilterConfig{
		IncludedDirs: []string{"Projects/App"},
	})

	assert.True(t, filter.ShouldObserveLocalPath("Projects", observedKindUnknown),
		"unknown watch events for include ancestors must pass pre-stat filtering")
	assert.True(t, filter.ShouldObserveLocalPath("Projects/App", observedKindUnknown),
		"unknown watch events for exact include roots must pass pre-stat filtering")
	assert.True(t, filter.ShouldObserveLocalPath("Projects/App/main.go", observedKindUnknown),
		"unknown watch events below include roots must still pass as file-capable")
	assert.False(t, filter.ShouldObserveLocalPath("Projects/Other", observedKindUnknown),
		"unknown watch events outside included subtrees should stay filtered")
}

func TestContentFilter_IgnoreWinsOverInclude(t *testing.T) {
	filter := newContentFilter(ContentFilterConfig{
		IncludedDirs: []string{"Projects"},
		IgnoredDirs:  []string{"Projects/build"},
	})

	assert.True(t, filter.Visible("Projects", ItemTypeFolder))
	assert.False(t, filter.Visible("Projects/build", ItemTypeFolder))
	assert.False(t, filter.Visible("Projects/build/app.o", ItemTypeFile))
}

func TestContentFilter_IgnoredPathsCoverFilesAndDirectories(t *testing.T) {
	filter := newContentFilter(ContentFilterConfig{
		IgnoredPaths: []string{"*.log", "tmp/*", "Cache"},
	})

	assert.False(t, filter.Visible("debug.log", ItemTypeFile))
	assert.False(t, filter.Visible("logs/debug.log", ItemTypeFile))
	assert.False(t, filter.Visible("tmp/run", ItemTypeFolder))
	assert.False(t, filter.Visible("tmp/run/out.txt", ItemTypeFile))
	assert.False(t, filter.Visible("src/Cache/data.bin", ItemTypeFile))
	assert.True(t, filter.Visible("src/main.go", ItemTypeFile))
}

func TestContentFilter_DotfilesAreBidirectionalOptIn(t *testing.T) {
	filter := newContentFilter(ContentFilterConfig{IgnoreDotfiles: true})

	assert.False(t, filter.Visible(".env", ItemTypeFile))
	assert.False(t, filter.Visible("src/.cache/data", ItemTypeFile))
	assert.True(t, filter.Visible("src/cache/data", ItemTypeFile))
}

func TestContentFilter_JunkFilesAreBidirectionalOptIn(t *testing.T) {
	filter := newContentFilter(ContentFilterConfig{IgnoreJunkFiles: true})

	assert.False(t, filter.Visible(".DS_Store", ItemTypeFile))
	assert.False(t, filter.Visible("Thumbs.db", ItemTypeFile))
	assert.False(t, filter.Visible("__MACOSX/file.txt", ItemTypeFile))
	assert.False(t, filter.Visible("download.partial", ItemTypeFile))
	assert.False(t, filter.Visible("draft.tmp", ItemTypeFile))
	assert.False(t, filter.Visible("swap.swp", ItemTypeFile))
	assert.False(t, filter.Visible("video.crdownload", ItemTypeFile))
	assert.False(t, filter.Visible("._resource", ItemTypeFile))
	assert.False(t, filter.Visible(".~lock", ItemTypeFile))
	assert.False(t, filter.Visible("~backup", ItemTypeFile))
}

// Validates: R-2.4.14
func TestContentFilter_ExtendedJunkList(t *testing.T) {
	filter := newContentFilter(ContentFilterConfig{IgnoreJunkFiles: true})

	for _, junk := range []string{
		"~$office-lock.docx", // Office owner file (P-2)
		"notes.txt~", ".notes.txt.swx", ".notes.txt.swo", "4913", "#notes.txt#", ".#notes.txt",
		"Main.java___jb_tmp___", "Main.java___jb_old___",
		".Spotlight-V100", ".Trashes", ".fseventsd", ".TemporaryItems", ".Trash-1000", "$RECYCLE.BIN", "Icon\r",
		"app.db-wal", "app.db-shm", "places.sqlite-journal", // SQLite sidecars (P-3)
		"photos/.fseventsd/000001",
	} {
		assert.False(t, filter.Visible(junk, ItemTypeFile), "%q is junk", junk)
	}

	for _, kept := range []string{
		"travel-journal", "fire-wal.txt", "notes.txt", "#hashtag", "app.db", "Icons", "budget-2026.xlsx",
	} {
		assert.True(t, filter.Visible(kept, ItemTypeFile), "%q is a user file", kept)
	}
}

// Validates: R-2.4.14
func TestContentFilter_FileOnlyJunkRulesKeepFolders(t *testing.T) {
	filter := newContentFilter(ContentFilterConfig{IgnoreJunkFiles: true})

	for _, folder := range []string{"~Projects", "notes~", "4913"} {
		assert.True(t, filter.Visible(folder, ItemTypeFolder), "folder %q syncs", folder)
		assert.True(t, filter.Visible(folder+"/report.docx", ItemTypeFile), "and so does its content")
		assert.False(t, filter.Visible(folder, ItemTypeFile), "the same name on a file is an editor temporary")
	}
	assert.False(t, filter.Visible("~Projects/~$report.docx", ItemTypeFile), "an owner file inside it is still junk")

	for _, trash := range []string{"$RECYCLE.BIN/x.txt", ".Trash-1000/files/x.txt"} {
		assert.False(t, filter.Visible(trash, ItemTypeFile), "%q: trash folders stay junk at any depth", trash)
	}
}

// Validates: R-6.2.7
func TestContentFilter_JunkFilesDefaultVisible(t *testing.T) {
	filter := newContentFilter(ContentFilterConfig{})

	assert.True(t, filter.Visible(".DS_Store", ItemTypeFile))
	assert.True(t, filter.Visible("Thumbs.db", ItemTypeFile))
	assert.True(t, filter.Visible("download.partial", ItemTypeFile))
	assert.False(t, filter.Visible(".onedrive-go.download.partial", ItemTypeFile))
}
