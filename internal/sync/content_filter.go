package sync

import (
	slashpath "path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

// ContentFilterConfig is the sync-engine visibility policy compiled from
// per-drive config. It owns product filtering only; provider structural
// exclusions and local admissibility issues stay in their existing boundaries.
type ContentFilterConfig struct {
	IgnoredDirs     []string
	IncludedDirs    []string
	IgnoredPaths    []string
	IgnoreDotfiles  bool
	IgnoreJunkFiles bool
	FollowSymlinks  bool
	// CrossFilesystems lets local observation descend into mount points
	// (P-11); see config.DriveFilterConfig.CrossFilesystems.
	CrossFilesystems bool
}

// contentFilter answers whether a root-relative path belongs to the current
// sync-visible content set.
type contentFilter struct {
	config ContentFilterConfig
}

// newContentFilter compiles a content filter from already-validated config.
func newContentFilter(config ContentFilterConfig) contentFilter {
	return contentFilter{config: config}
}

func cloneContentFilterConfig(config ContentFilterConfig) ContentFilterConfig {
	return ContentFilterConfig{
		IgnoredDirs:      slices.Clone(config.IgnoredDirs),
		IncludedDirs:     slices.Clone(config.IncludedDirs),
		IgnoredPaths:     slices.Clone(config.IgnoredPaths),
		IgnoreDotfiles:   config.IgnoreDotfiles,
		IgnoreJunkFiles:  config.IgnoreJunkFiles,
		FollowSymlinks:   config.FollowSymlinks,
		CrossFilesystems: config.CrossFilesystems,
	}
}

func contentFilterConfigsEqual(a ContentFilterConfig, b ContentFilterConfig) bool {
	return slices.Equal(a.IgnoredDirs, b.IgnoredDirs) &&
		slices.Equal(a.IncludedDirs, b.IncludedDirs) &&
		slices.Equal(a.IgnoredPaths, b.IgnoredPaths) &&
		a.IgnoreDotfiles == b.IgnoreDotfiles &&
		a.IgnoreJunkFiles == b.IgnoreJunkFiles &&
		a.FollowSymlinks == b.FollowSymlinks &&
		a.CrossFilesystems == b.CrossFilesystems
}

// Visible reports whether path should be present in planner-visible current
// sync truth for the given item type.
func (f contentFilter) Visible(path string, itemType ItemType) bool {
	normalized := normalizeContentFilterPath(path)
	if normalized == "" || normalized == "." {
		return true
	}

	isDir := itemType == ItemTypeFolder || itemType == ItemTypeRoot
	if !f.inIncludedScope(normalized, isDir) {
		return false
	}

	if f.isIgnored(normalized, isDir) {
		return false
	}

	return true
}

// ShouldObserveLocalPath reports whether local observation should descend into
// or emit the path. It uses observedKind because local observation can know the
// path kind before the sync ItemType has been built.
func (f contentFilter) ShouldObserveLocalPath(path string, kind observedKind) bool {
	resolvedType := ItemTypeFile
	switch kind {
	case observedKindFile:
		resolvedType = ItemTypeFile
	case observedKindDir:
		resolvedType = ItemTypeFolder
	case observedKindUnknown:
		return f.Visible(path, ItemTypeFile) || f.Visible(path, ItemTypeFolder)
	}

	return f.Visible(path, resolvedType)
}

// ShouldFollowSymlinks reports whether local observation may follow symlink
// targets. OneDrive has no symlink item type, so this is local-only policy.
func (f contentFilter) ShouldFollowSymlinks() bool {
	return f.config.FollowSymlinks
}

func (f contentFilter) inIncludedScope(path string, isDir bool) bool {
	if len(f.config.IncludedDirs) == 0 {
		return true
	}

	for _, includeDir := range f.config.IncludedDirs {
		includeDir = normalizeContentFilterPath(includeDir)
		if includeDir == "" || includeDir == "." {
			continue
		}

		if path == includeDir {
			return isDir
		}

		if strings.HasPrefix(path, includeDir+"/") {
			return true
		}

		if isDir && isAncestorPath(path, includeDir) {
			return true
		}
	}

	return false
}

func (f contentFilter) isIgnored(path string, isDir bool) bool {
	if matchesExactSubtree(path, f.config.IgnoredDirs) {
		return true
	}

	parts := strings.Split(path, "/")
	for _, part := range parts {
		if driveops.IsOwnedTransferArtifactName(part) || isSpellingRenameArtifactName(part) {
			return true
		}
	}

	if f.config.IgnoreDotfiles && hasDotfileComponent(parts) {
		return true
	}

	if f.config.IgnoreJunkFiles && hasJunkComponent(parts, !isDir) {
		return true
	}

	return matchesIgnoredPathPattern(path, parts, f.config.IgnoredPaths)
}

func normalizeContentFilterPath(path string) string {
	normalized := filepath.ToSlash(path)
	normalized = strings.TrimPrefix(normalized, "/")
	normalized = strings.TrimSuffix(normalized, "/")
	if normalized == "" {
		return "."
	}

	return normalized
}

func isAncestorPath(parent, child string) bool {
	return child != parent && strings.HasPrefix(child, parent+"/")
}

func matchesExactSubtree(path string, roots []string) bool {
	for _, root := range roots {
		normalizedRoot := normalizeContentFilterPath(root)
		if normalizedRoot == "" || normalizedRoot == "." {
			continue
		}

		if path == normalizedRoot || strings.HasPrefix(path, normalizedRoot+"/") {
			return true
		}
	}

	return false
}

func matchesIgnoredPathPattern(path string, parts []string, patterns []string) bool {
	for _, pattern := range patterns {
		normalizedPattern := normalizeContentFilterPath(pattern)
		if normalizedPattern == "" || normalizedPattern == "." {
			continue
		}

		if strings.Contains(normalizedPattern, "/") {
			if pathOrAncestorMatchesSlashPattern(path, normalizedPattern) {
				return true
			}

			continue
		}

		for _, part := range parts {
			if matched, err := slashpath.Match(normalizedPattern, part); err == nil && matched {
				return true
			}
		}
	}

	return false
}

func pathOrAncestorMatchesSlashPattern(path, pattern string) bool {
	for current := path; current != ""; {
		if matched, err := slashpath.Match(pattern, current); err == nil && matched {
			return true
		}

		idx := strings.LastIndex(current, "/")
		if idx < 0 {
			break
		}

		current = current[:idx]
	}

	return false
}

// hasJunkComponent reports a junk name anywhere in the path. The file-only
// rules apply only to the last component, and only when it is a file: a
// folder named "~Projects" or "notes~", or a project folder "4913", is the
// user's, while the same name on a file is an editor temporary.
func hasJunkComponent(parts []string, leafIsFile bool) bool {
	for i, part := range parts {
		if isBundledJunkName(part) {
			return true
		}
		if i == len(parts)-1 && leafIsFile && isBundledJunkFileName(part) {
			return true
		}
	}

	return false
}

// isBundledJunkName reports names that ignore_junk_files excludes: OS
// metadata, editor and IDE temporaries, browser partials, Office owner
// files, and SQLite sidecars. Each is either rewritten constantly while its
// owner runs, meaningless on another device, or (sidecars) harmful when
// synced apart from its database (D-084, P-2, P-3).
func isBundledJunkName(name string) bool {
	lower := asciiLower(name)

	switch lower {
	case ".ds_store", "thumbs.db", "__macosx",
		".spotlight-v100", ".trashes", ".fseventsd", ".temporaryitems", "$recycle.bin",
		"icon\r":
		return true
	}

	for _, suffix := range bundledJunkSuffixes() {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}

	switch {
	case strings.HasPrefix(name, ".~"), strings.HasPrefix(name, "._"),
		strings.HasPrefix(name, ".#"),                                                 // emacs lock links
		strings.HasPrefix(lower, ".trash-"),                                           // freedesktop per-user trash on removable media
		len(name) > 2 && strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#"): // emacs autosave
		return true
	}

	return isSQLiteSidecarName(lower)
}

// isBundledJunkFileName adds the rules that are junk only on a file: vim's
// 4913 write probe, "~"-prefixed temporaries and Office "~$" owner files,
// and "file~" editor backups. On a folder these are ordinary user names.
func isBundledJunkFileName(name string) bool {
	return name == "4913" || strings.HasPrefix(name, "~") || strings.HasSuffix(name, "~")
}

// bundledJunkSuffixes are lowercase name endings of temporaries.
func bundledJunkSuffixes() []string {
	return []string{
		".partial", ".tmp", ".crdownload",
		".swp", ".swo", ".swx", // vim swap files
		"___jb_tmp___", "___jb_old___", // JetBrains safe-write temporaries
	}
}

// isSQLiteSidecarName matches <db>-wal, <db>-shm and <db>-journal where the
// database name itself has an extension (app.db-wal, places.sqlite-shm).
// Requiring the dot keeps ordinary names such as "travel-journal" syncing;
// SQLite databases kept in a sync folder essentially always carry one.
func isSQLiteSidecarName(lower string) bool {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		base, found := strings.CutSuffix(lower, suffix)
		if found && strings.Contains(base, ".") && !strings.HasSuffix(base, ".") {
			return true
		}
	}

	return false
}

// spellingRenameArtifactPrefix starts the temporary sibling of a
// spelling-only local rename (executor renameLocal).
const spellingRenameArtifactPrefix = ".onedrive-go.spelling."

// isSpellingRenameArtifactName reports the temporary sibling of a
// spelling-only local rename. It is excluded whatever the filter settings,
// like download partials, so a crash between its two renames never syncs it;
// unlike partials it is never deleted by cleanup, because it holds a user's
// file.
func isSpellingRenameArtifactName(name string) bool {
	return strings.HasPrefix(name, spellingRenameArtifactPrefix)
}
