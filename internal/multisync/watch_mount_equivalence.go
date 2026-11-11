package multisync

import (
	"slices"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func mountSpecsEquivalentForWatchRestart(current *mountSpec, next *mountSpec) bool {
	if current == nil || next == nil {
		return current == next
	}
	return mountSpecCoreEquivalent(current, next)
}

func mountSpecCoreEquivalent(current *mountSpec, next *mountSpec) bool {
	return mountSpecIdentityEquivalent(current, next) &&
		mountSpecRemoteEquivalent(current, next) &&
		mountSpecRuntimeEquivalent(current, next) &&
		mountSpecTuningEquivalent(current, next)
}

func mountSpecIdentityEquivalent(current *mountSpec, next *mountSpec) bool {
	return current.id() == next.id() &&
		current.childParentMountID() == next.childParentMountID() &&
		current.projectionKind() == next.projectionKind() &&
		current.parentDriveType() == next.parentDriveType() &&
		current.rejectSharePointRootForms() == next.rejectSharePointRootForms()
}

func mountSpecRemoteEquivalent(current *mountSpec, next *mountSpec) bool {
	return current.remoteDriveID() == next.remoteDriveID() &&
		current.remoteRootItemID() == next.remoteRootItemID() &&
		current.tokenOwnerCanonical() == next.tokenOwnerCanonical() &&
		current.accountEmail() == next.accountEmail()
}

func mountSpecRuntimeEquivalent(current *mountSpec, next *mountSpec) bool {
	return current.syncRoot() == next.syncRoot() &&
		current.statePath() == next.statePath() &&
		current.enableWebsocket() == next.enableWebsocket() &&
		current.remoteRootDeltaCapable() == next.remoteRootDeltaCapable() &&
		contentFilterConfigsEquivalent(current.contentFilter(), next.contentFilter()) &&
		childEngineSpecsEquivalent(current, next)
}

func childEngineSpecsEquivalent(current *mountSpec, next *mountSpec) bool {
	if current == nil || next == nil || current.child == nil || next.child == nil {
		return (current == nil || current.child == nil) && (next == nil || next.child == nil)
	}
	return syncengine.ShortcutChildEngineSpecsEqual(&current.child.engine, &next.child.engine)
}

func mountSpecTuningEquivalent(current *mountSpec, next *mountSpec) bool {
	return current.transferWorkers() == next.transferWorkers() &&
		current.checkWorkers() == next.checkWorkers() &&
		current.minFreeSpace() == next.minFreeSpace()
}

func contentFilterConfigsEquivalent(current syncengine.ContentFilterConfig, next syncengine.ContentFilterConfig) bool {
	return slices.Equal(current.IgnoredDirs, next.IgnoredDirs) &&
		slices.Equal(current.IncludedDirs, next.IncludedDirs) &&
		slices.Equal(current.IgnoredPaths, next.IgnoredPaths) &&
		current.IgnoreDotfiles == next.IgnoreDotfiles &&
		current.IgnoreJunkFiles == next.IgnoreJunkFiles &&
		current.FollowSymlinks == next.FollowSymlinks &&
		current.CrossFilesystems == next.CrossFilesystems
}
