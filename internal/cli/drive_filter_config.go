package cli

import (
	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// contentFilterConfigFor is the engine's view of a drive's filter settings
// (sync_runtime.go builds every mount with it). status reads the same scope,
// so its remote-changes count matches what the planner would act on
// (R-2.10.48). It lives in a file of its own so the status snapshot, which
// the store family owns, does not reference the engine family.
func contentFilterConfigFor(f *config.DriveFilterConfig) syncengine.ContentFilterConfig {
	return syncengine.ContentFilterConfig{
		IgnoredDirs:      append([]string(nil), f.IgnoredDirs...),
		IncludedDirs:     append([]string(nil), f.IncludedDirs...),
		IgnoredPaths:     append([]string(nil), f.IgnoredPaths...),
		IgnoreDotfiles:   f.IgnoreDotfiles,
		IgnoreJunkFiles:  f.JunkFilesIgnored(),
		FollowSymlinks:   f.FollowSymlinks,
		CrossFilesystems: f.CrossFilesystems,
	}
}
