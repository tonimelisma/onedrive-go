package sync

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
	assert.True(t, filter.Visible("~$office-lock.docx", ItemTypeFile))
}

func TestContentFilter_JunkFilesDefaultVisible(t *testing.T) {
	filter := NewContentFilter(ContentFilterConfig{})

	assert.True(t, filter.Visible(".DS_Store", ItemTypeFile))
	assert.True(t, filter.Visible("Thumbs.db", ItemTypeFile))
	assert.True(t, filter.Visible("download.partial", ItemTypeFile))
	assert.False(t, filter.Visible(".onedrive-go.download.partial", ItemTypeFile))
}
