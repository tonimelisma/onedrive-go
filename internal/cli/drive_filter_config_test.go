package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// Every drive filter setting reaches the engine. cross_filesystems was once
// left out here, so one-shot sync and status ignored it while watch mode
// honored it (found live, 2026-11-04).
//
// Validates: R-6.2.15, R-2.10.48
func TestContentFilterConfigFor_CarriesEveryFilterSetting(t *testing.T) {
	t.Parallel()

	junk := false
	got := contentFilterConfigFor(&config.DriveFilterConfig{
		IgnoredDirs:      []string{"Ignored"},
		IncludedDirs:     []string{"Included"},
		IgnoredPaths:     []string{"*.tmp"},
		IgnoreDotfiles:   true,
		IgnoreJunkFiles:  &junk,
		FollowSymlinks:   true,
		CrossFilesystems: true,
	})

	assert.Equal(t, syncengine.ContentFilterConfig{
		IgnoredDirs:      []string{"Ignored"},
		IncludedDirs:     []string{"Included"},
		IgnoredPaths:     []string{"*.tmp"},
		IgnoreDotfiles:   true,
		IgnoreJunkFiles:  false,
		FollowSymlinks:   true,
		CrossFilesystems: true,
	}, got)
}
