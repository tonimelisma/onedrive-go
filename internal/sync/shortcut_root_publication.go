package sync

import (
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

func shortcutChildWorkSnapshotFromRootsWithParentRoot(
	namespaceID string,
	parentSyncRoot string,
	parentContentFilter ContentFilterConfig,
	roots []ShortcutRootRecord,
) ShortcutChildWorkSnapshot {
	snapshot := ShortcutChildWorkSnapshot{
		NamespaceID: namespaceID,
		RunCommands: make([]ShortcutChildRunCommand, 0, len(roots)),
	}
	for i := range roots {
		root := normalizeShortcutRootRecord(&roots[i])
		if root.NamespaceID != "" && root.NamespaceID != namespaceID {
			continue
		}
		metadata, _ := shortcutRootLifecycleMetadataFor(root.State)
		if metadata.publishesCleanup {
			childMountID := config.ChildMountID(namespaceID, root.BindingItemID)
			snapshot.CleanupCommands = append(snapshot.CleanupCommands, ShortcutChildCleanupCommand{
				ChildMountID: childMountID,
				LocalRoot:    shortcutChildCleanupLocalRoot(parentSyncRoot, root.RelativeLocalPath),
				Reason:       ShortcutChildArtifactCleanupParentRemoved,
				AckRef:       newShortcutChildAckRef(root.BindingItemID),
			})
			continue
		}
		if metadata.runMode == "" {
			continue
		}
		childFilter, visible := projectShortcutChildContentFilter(parentContentFilter, root.RelativeLocalPath)
		if !visible {
			continue
		}
		child := ShortcutChildRunCommand{
			ChildMountID: config.ChildMountID(namespaceID, root.BindingItemID),
			DisplayName:  root.LocalAlias,
			Engine: ShortcutChildEngineSpec{
				LocalRoot:         shortcutChildLocalRoot(parentSyncRoot, root.RelativeLocalPath),
				RemoteDriveID:     root.RemoteDriveID.String(),
				RemoteItemID:      root.RemoteItemID,
				LocalRootIdentity: shortcutRootIdentityFromFileIdentity(root.LocalRootIdentity),
				ContentFilter:     childFilter,
			},
			Mode:   metadata.runMode,
			AckRef: newShortcutChildAckRef(root.BindingItemID),
		}
		snapshot.RunCommands = append(snapshot.RunCommands, child)
	}
	return snapshot
}

func projectShortcutChildContentFilter(parentFilter ContentFilterConfig, aliasPath string) (ContentFilterConfig, bool) {
	aliasPath = normalizeContentFilterPath(aliasPath)
	if aliasPath == "" || aliasPath == "." {
		return cloneContentFilterConfig(parentFilter), true
	}

	if !NewContentFilter(parentFilter).Visible(aliasPath, ItemTypeFolder) {
		return ContentFilterConfig{}, false
	}

	childFilter := ContentFilterConfig{
		IgnoredDirs:     projectChildIgnoredDirs(parentFilter.IgnoredDirs, aliasPath),
		IncludedDirs:    projectChildIncludedDirs(parentFilter.IncludedDirs, aliasPath),
		IgnoredPaths:    projectChildIgnoredPaths(parentFilter.IgnoredPaths, aliasPath),
		IgnoreDotfiles:  parentFilter.IgnoreDotfiles,
		IgnoreJunkFiles: parentFilter.IgnoreJunkFiles,
		FollowSymlinks:  parentFilter.FollowSymlinks,
	}
	return childFilter, true
}

func projectChildIgnoredDirs(parentIgnoredDirs []string, aliasPath string) []string {
	projected := make([]string, 0, len(parentIgnoredDirs))
	for _, ignoredDir := range parentIgnoredDirs {
		ignoredDir = normalizeContentFilterPath(ignoredDir)
		if ignoredDir == "" || ignoredDir == "." {
			continue
		}
		if childPath, ok := trimContentFilterPathBelowRoot(aliasPath, ignoredDir); ok && childPath != "." {
			projected = appendUniqueFilterPath(projected, childPath)
		}
	}
	return projected
}

func projectChildIncludedDirs(parentIncludedDirs []string, aliasPath string) []string {
	if len(parentIncludedDirs) == 0 {
		return nil
	}

	projected := make([]string, 0, len(parentIncludedDirs))
	for _, includedDir := range parentIncludedDirs {
		includedDir = normalizeContentFilterPath(includedDir)
		if includedDir == "" || includedDir == "." {
			continue
		}
		if includedDir == aliasPath || isAncestorPath(includedDir, aliasPath) {
			return nil
		}
		if childPath, ok := trimContentFilterPathBelowRoot(aliasPath, includedDir); ok && childPath != "." {
			projected = appendUniqueFilterPath(projected, childPath)
		}
	}
	return projected
}

func projectChildIgnoredPaths(parentIgnoredPaths []string, aliasPath string) []string {
	projected := make([]string, 0, len(parentIgnoredPaths))
	aliasParts := strings.Split(aliasPath, "/")
	for _, ignoredPath := range parentIgnoredPaths {
		ignoredPath = normalizeContentFilterPath(ignoredPath)
		if ignoredPath == "" || ignoredPath == "." {
			continue
		}
		if !strings.Contains(ignoredPath, "/") {
			projected = appendUniqueFilterPath(projected, ignoredPath)
			continue
		}
		if childPath, ok := trimContentFilterPathBelowRoot(aliasPath, ignoredPath); ok && childPath != "." {
			projected = appendUniqueFilterPath(projected, childPath)
			continue
		}
		if childPattern, ok := projectSlashGlobBelowAlias(ignoredPath, aliasParts); ok && childPattern != "." {
			projected = appendUniqueFilterPath(projected, childPattern)
		}
	}
	return projected
}

func trimContentFilterPathBelowRoot(rootPath string, path string) (string, bool) {
	rootPath = normalizeContentFilterPath(rootPath)
	path = normalizeContentFilterPath(path)
	switch {
	case path == rootPath:
		return ".", true
	case isAncestorPath(rootPath, path):
		return strings.TrimPrefix(path, rootPath+"/"), true
	default:
		return "", false
	}
}

func projectSlashGlobBelowAlias(pattern string, aliasParts []string) (string, bool) {
	patternParts := strings.Split(pattern, "/")
	if len(patternParts) <= len(aliasParts) {
		return "", false
	}
	for i, aliasPart := range aliasParts {
		matched, err := filepath.Match(patternParts[i], aliasPart)
		if err != nil || !matched {
			return "", false
		}
	}
	return strings.Join(patternParts[len(aliasParts):], "/"), true
}

func appendUniqueFilterPath(paths []string, path string) []string {
	for _, existing := range paths {
		if existing == path {
			return paths
		}
	}
	return append(paths, path)
}

func shortcutChildCleanupLocalRoot(parentSyncRoot string, relativeLocalPath string) string {
	return shortcutChildLocalRoot(parentSyncRoot, relativeLocalPath)
}

func shortcutChildLocalRoot(parentSyncRoot string, relativeLocalPath string) string {
	if parentSyncRoot == "" || relativeLocalPath == "" {
		return ""
	}
	return filepath.Join(parentSyncRoot, filepath.FromSlash(relativeLocalPath))
}

func shortcutChildRunModeForRoot(state ShortcutRootState) ShortcutChildRunMode {
	metadata, ok := shortcutRootLifecycleMetadataFor(state)
	if !ok {
		return ""
	}
	return metadata.runMode
}
