package syncdomain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-6.3.4
//
// A snapshot owns copies: neither the entries it was built from nor the
// values it hands out can change what it answers.
func TestBaselineSnapshot_HandsOutValuesItOwns(t *testing.T) {
	t.Parallel()

	source := &BaselineEntry{
		Path:     "docs/Readme.txt",
		DriveID:  driveid.New("drive1"),
		ItemID:   "item-1",
		ItemType: ItemTypeFile,
		ETag:     "etag-1",
	}
	snapshot := NewBaselineSnapshot([]*BaselineEntry{source})
	source.ETag = "changed by the caller"

	byPath, ok := snapshot.GetByPath("docs/Readme.txt")
	require.True(t, ok)
	assert.Equal(t, "etag-1", byPath.ETag)
	byPath.ETag = "changed by a reader"

	byID, ok := snapshot.GetByID("item-1")
	require.True(t, ok)
	assert.Equal(t, "etag-1", byID.ETag)

	var seen []string
	snapshot.ForEachPath(func(path string, entry BaselineEntry) {
		seen = append(seen, path+"="+entry.ETag)
	})
	assert.Equal(t, []string{"docs/Readme.txt=etag-1"}, seen)
	assert.Equal(t, 1, snapshot.Len())

	_, ok = snapshot.GetByPath("docs/missing.txt")
	assert.False(t, ok)
	_, ok = snapshot.GetByID("missing")
	assert.False(t, ok)
	assert.Equal(t, 0, NewBaselineSnapshot(nil).Len())
}
