package main

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func TestCleanRemotePath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"root slash", "/", ""},
		{"nested with trailing slash", "/foo/bar/", "foo/bar"},
		{"empty string", "", ""},
		{"no slashes", "foo", "foo"},
		{"double slashes", "//double//", "double"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, cleanRemotePath(tt.path))
		})
	}
}

func TestSplitParentAndName(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantParent string
		wantName   string
	}{
		{"nested path", "foo/bar/baz", "foo/bar", "baz"},
		{"single segment", "baz", "", "baz"},
		{"empty string", "", "", ""},
		{"trailing slash top-level", "/top/", "", "top"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent, name := splitParentAndName(tt.path)
			assert.Equal(t, tt.wantParent, parent)
			assert.Equal(t, tt.wantName, name)
		})
	}
}

// captureStdout redirects os.Stdout to a pipe and returns what fn wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = w

	t.Cleanup(func() { os.Stdout = old })

	fn()
	w.Close()

	out, err := io.ReadAll(r)
	require.NoError(t, err)

	return string(out)
}

func TestPrintItemsTable(t *testing.T) {
	items := []graph.Item{
		{
			Name:       "readme.txt",
			Size:       1024,
			IsFolder:   false,
			ModifiedAt: time.Date(2025, time.January, 15, 10, 30, 0, 0, time.UTC),
		},
		{
			Name:       "docs",
			Size:       0,
			IsFolder:   true,
			ModifiedAt: time.Date(2025, time.February, 1, 9, 0, 0, 0, time.UTC),
		},
	}

	output := captureStdout(t, func() {
		printItemsTable(items)
	})

	// Headers should be present.
	assert.Contains(t, output, "NAME")
	assert.Contains(t, output, "SIZE")
	assert.Contains(t, output, "MODIFIED")
	// Folders sort first and get a trailing slash.
	assert.Contains(t, output, "docs/")
	assert.Contains(t, output, "readme.txt")
}

func TestPrintStatText(t *testing.T) {
	item := &graph.Item{
		ID:         "item-123",
		Name:       "photo.jpg",
		Size:       2048,
		IsFolder:   false,
		MimeType:   "image/jpeg",
		ModifiedAt: time.Date(2025, time.March, 10, 14, 0, 0, 0, time.UTC),
		CreatedAt:  time.Date(2025, time.January, 5, 8, 0, 0, 0, time.UTC),
	}

	output := captureStdout(t, func() {
		printStatText(item)
	})

	assert.Contains(t, output, "photo.jpg")
	assert.Contains(t, output, "file")
	assert.Contains(t, output, "2048 bytes")
	assert.Contains(t, output, "item-123")
	assert.Contains(t, output, "image/jpeg")
}

func TestPrintStatText_Folder(t *testing.T) {
	item := &graph.Item{
		ID:         "folder-456",
		Name:       "Documents",
		Size:       0,
		IsFolder:   true,
		ModifiedAt: time.Date(2025, time.June, 1, 12, 0, 0, 0, time.UTC),
		CreatedAt:  time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC),
	}

	output := captureStdout(t, func() {
		printStatText(item)
	})

	assert.Contains(t, output, "Documents")
	assert.Contains(t, output, "folder")
	assert.Contains(t, output, "folder-456")
	// MIME should not appear for folders (empty string).
	assert.NotContains(t, output, "MIME:")
}

func TestPrintWhoamiText(t *testing.T) {
	user := &graph.User{
		ID:          "user-789",
		DisplayName: "Test User",
		Email:       "test@example.com",
	}

	drives := []graph.Drive{
		{
			ID:         driveid.New("drive-abc"),
			Name:       "OneDrive",
			DriveType:  "personal",
			QuotaUsed:  1073741824, // 1 GB
			QuotaTotal: 5368709120, // 5 GB
		},
	}

	output := captureStdout(t, func() {
		printWhoamiText(user, drives)
	})

	assert.Contains(t, output, "Test User")
	assert.Contains(t, output, "test@example.com")
	assert.Contains(t, output, "user-789")
	assert.Contains(t, output, "OneDrive")
	assert.Contains(t, output, "personal")
	assert.Contains(t, output, "drive-abc")
}
