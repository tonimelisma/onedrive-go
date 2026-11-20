package sync

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.11.10
func TestOneDrivePathLength_CountsUTF16CodeUnits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want int
	}{
		{name: "ascii", path: "docs/a.txt", want: 10},
		{name: "finnish", path: strings.Repeat("\u00e4", 300), want: 300},
		{name: "cjk", path: strings.Repeat("\u65e5", 150), want: 150},
		{name: "emoji is two units", path: "\U0001F600", want: 2},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, oneDrivePathLength(tt.path), tt.name)
	}
}

// A single name at the root is held only to the path limit: 400 units pass,
// 401 are path_too_long with the path check's message, whatever the bytes.
//
// Validates: R-2.11.10, R-2.11.12
func TestShouldObserve_RootLevelNameMeetsOnlyThePathLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		label    string
		name     string
		tooLong  bool
		wantUnit int
	}{
		{label: "400 ASCII", name: strings.Repeat("a", maxOneDrivePathLength), wantUnit: 400},
		{label: "401 ASCII", name: strings.Repeat("a", maxOneDrivePathLength+1), tooLong: true, wantUnit: 401},
		// U+4E2D: 400 units are 1,200 bytes.
		{label: "400 BMP", name: strings.Repeat("\u4e2d", maxOneDrivePathLength), wantUnit: 400},
		// U+1F600: 200 of them are 400 units; one more letter is 401.
		{label: "401 units with emoji", name: strings.Repeat("\U0001F600", 200) + "a", tooLong: true, wantUnit: 401},
	}
	for _, tt := range tests {
		require.Equal(t, tt.wantUnit, oneDrivePathLength(tt.name), tt.label)
		skip := shouldObserveWithFilter(tt.name, tt.name, ObservedKindFile, ContentFilterConfig{}, nil, LocalObservationRules{})
		if !tt.tooLong {
			assert.Nil(t, skip, tt.label)
			continue
		}
		require.NotNil(t, skip, tt.label)
		assert.Equal(t, IssuePathTooLong, skip.Reason, tt.label)
		assert.Equal(t, fmt.Sprintf("path length 401 exceeds %d-character limit", maxOneDrivePathLength), skip.Detail, tt.label)
	}
}
