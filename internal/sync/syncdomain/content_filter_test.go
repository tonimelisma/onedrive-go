package syncdomain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContentFilter_IncludedDirsIncludeAncestorsRootsAndDescendants(t *testing.T) {
	filter := NewContentFilter(ContentFilterConfig{
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
	filter := NewContentFilter(ContentFilterConfig{
		IncludedDirs: []string{"Projects/App"},
	})

	assert.True(t, filter.ShouldObserveLocalPath("Projects", ObservedKindUnknown),
		"unknown watch events for include ancestors must pass pre-stat filtering")
	assert.True(t, filter.ShouldObserveLocalPath("Projects/App", ObservedKindUnknown),
		"unknown watch events for exact include roots must pass pre-stat filtering")
	assert.True(t, filter.ShouldObserveLocalPath("Projects/App/main.go", ObservedKindUnknown),
		"unknown watch events below include roots must still pass as file-capable")
	assert.False(t, filter.ShouldObserveLocalPath("Projects/Other", ObservedKindUnknown),
		"unknown watch events outside included subtrees should stay filtered")
}

func TestContentFilter_IgnoreWinsOverInclude(t *testing.T) {
	filter := NewContentFilter(ContentFilterConfig{
		IncludedDirs: []string{"Projects"},
		IgnoredDirs:  []string{"Projects/build"},
	})

	assert.True(t, filter.Visible("Projects", ItemTypeFolder))
	assert.False(t, filter.Visible("Projects/build", ItemTypeFolder))
	assert.False(t, filter.Visible("Projects/build/app.o", ItemTypeFile))
}

func TestContentFilter_IgnoredPathsCoverFilesAndDirectories(t *testing.T) {
	filter := NewContentFilter(ContentFilterConfig{
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
	filter := NewContentFilter(ContentFilterConfig{IgnoreDotfiles: true})

	assert.False(t, filter.Visible(".env", ItemTypeFile))
	assert.False(t, filter.Visible("src/.cache/data", ItemTypeFile))
	assert.True(t, filter.Visible("src/cache/data", ItemTypeFile))
}

func TestContentFilter_JunkFilesAreBidirectionalOptIn(t *testing.T) {
	filter := NewContentFilter(ContentFilterConfig{IgnoreJunkFiles: true})

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
	filter := NewContentFilter(ContentFilterConfig{IgnoreJunkFiles: true})

	for _, junk := range []string{
		"~$office-lock.docx", // Office owner file (R-2.4.14)
		"notes.txt~", ".notes.txt.swx", ".notes.txt.swo", "4913", "#notes.txt#", ".#notes.txt",
		"Main.java___jb_tmp___", "Main.java___jb_old___",
		".Spotlight-V100", ".Trashes", ".fseventsd", ".TemporaryItems", ".Trash-1000", "$RECYCLE.BIN", "Icon\r",
		"app.db-wal", "app.db-shm", "places.sqlite-journal", // SQLite sidecars (R-2.4.15)
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
	filter := NewContentFilter(ContentFilterConfig{IgnoreJunkFiles: true})

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
	filter := NewContentFilter(ContentFilterConfig{})

	assert.True(t, filter.Visible(".DS_Store", ItemTypeFile))
	assert.True(t, filter.Visible("Thumbs.db", ItemTypeFile))
	assert.True(t, filter.Visible("download.partial", ItemTypeFile))
	assert.False(t, filter.Visible(".onedrive-go.download.partial", ItemTypeFile))
	assert.False(t, filter.Visible("Docs/.onedrive-go.0123456789abcdef.probe", ItemTypeFile), "a rename probe a crash left")
}

// The name a temporary spelling-rename sibling was on its way to is read back
// only when the sibling's name says it: a -1 to -9 suffix may be a later
// attempt's, so such a name answers no rather than a guess.
//
// Validates: R-2.12.8
func TestSpellingRenameArtifactPlainName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		plain     string
		hasAnswer bool
	}{
		{name: SpellingRenameArtifactPrefix + "A.txt", plain: "A.txt", hasAnswer: true},
		{name: SpellingRenameArtifactPrefix + "Report-2026.docx", plain: "Report-2026.docx", hasAnswer: true},
		{name: SpellingRenameArtifactPrefix + "v-10", plain: "v-10", hasAnswer: true},
		{name: SpellingRenameArtifactPrefix + "v-0", plain: "v-0", hasAnswer: true},
		{name: SpellingRenameArtifactPrefix + "v-01", plain: "v-01", hasAnswer: true},
		{name: SpellingRenameArtifactPrefix + "A.txt-1"},
		{name: SpellingRenameArtifactPrefix + "v-9"},
		{name: SpellingRenameArtifactPrefix},
		{name: SpellingRenameArtifactPrefix + ".."},
		{name: "A.txt"},
		{name: ".onedrive-go.spelling"},
	}
	for _, tt := range tests {
		plain, hasAnswer := SpellingRenameArtifactPlainName(tt.name)
		assert.Equal(t, tt.hasAnswer, hasAnswer, "%q", tt.name)
		assert.Equal(t, tt.plain, plain, "%q", tt.name)
	}
}
