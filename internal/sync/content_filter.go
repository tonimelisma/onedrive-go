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
}

// ContentFilter answers whether a root-relative path belongs to the current
// sync-visible content set.
type ContentFilter struct {
	config ContentFilterConfig
}

// NewContentFilter compiles a content filter from already-validated config.
func NewContentFilter(config ContentFilterConfig) ContentFilter {
	return ContentFilter{config: config}
}

func cloneContentFilterConfig(config ContentFilterConfig) ContentFilterConfig {
	return ContentFilterConfig{
		IgnoredDirs:     slices.Clone(config.IgnoredDirs),
		IncludedDirs:    slices.Clone(config.IncludedDirs),
		IgnoredPaths:    slices.Clone(config.IgnoredPaths),
		IgnoreDotfiles:  config.IgnoreDotfiles,
		IgnoreJunkFiles: config.IgnoreJunkFiles,
		FollowSymlinks:  config.FollowSymlinks,
	}
}

func contentFilterConfigsEqual(a ContentFilterConfig, b ContentFilterConfig) bool {
	return slices.Equal(a.IgnoredDirs, b.IgnoredDirs) &&
		slices.Equal(a.IncludedDirs, b.IncludedDirs) &&
		slices.Equal(a.IgnoredPaths, b.IgnoredPaths) &&
		a.IgnoreDotfiles == b.IgnoreDotfiles &&
		a.IgnoreJunkFiles == b.IgnoreJunkFiles &&
		a.FollowSymlinks == b.FollowSymlinks
}

// Visible reports whether path should be present in planner-visible current
// sync truth for the given item type.
func (f ContentFilter) Visible(path string, itemType ItemType) bool {
	normalized := normalizeContentFilterPath(path)
	if normalized == "" || normalized == "." {
		return true
	}

	isDir := itemType == ItemTypeFolder || itemType == ItemTypeRoot
	if !f.inIncludedScope(normalized, isDir) {
		return false
	}

	if f.isIgnored(normalized) {
		return false
	}

	return true
}

// ShouldObserveLocalPath reports whether local observation should descend into
// or emit the path. It uses observedKind because local observation can know the
// path kind before the sync ItemType has been built.
func (f ContentFilter) ShouldObserveLocalPath(path string, kind observedKind) bool {
	itemType := ItemTypeFile
	switch kind {
	case observedKindFile:
		itemType = ItemTypeFile
	case observedKindDir:
		itemType = ItemTypeFolder
	case observedKindUnknown:
		return f.Visible(path, ItemTypeFile) || f.Visible(path, ItemTypeFolder)
	}

	return f.Visible(path, itemType)
}

// ShouldFollowSymlinks reports whether local observation may follow symlink
// targets. OneDrive has no symlink item type, so this is local-only policy.
func (f ContentFilter) ShouldFollowSymlinks() bool {
	return f.config.FollowSymlinks
}

func (f ContentFilter) inIncludedScope(path string, isDir bool) bool {
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

func (f ContentFilter) isIgnored(path string) bool {
	if matchesExactSubtree(path, f.config.IgnoredDirs) {
		return true
	}

	parts := strings.Split(path, "/")
	for _, part := range parts {
		if driveops.IsOwnedTransferArtifactName(part) {
			return true
		}
	}

	if f.config.IgnoreDotfiles && hasDotfileComponent(parts) {
		return true
	}

	if f.config.IgnoreJunkFiles && hasJunkComponent(parts) {
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

func hasJunkComponent(parts []string) bool {
	for _, part := range parts {
		if isBundledJunkName(part) {
			return true
		}
	}

	return false
}

func isBundledJunkName(name string) bool {
	lower := AsciiLower(name)

	if lower == ".ds_store" || lower == "thumbs.db" || lower == "__macosx" {
		return true
	}

	if strings.HasSuffix(lower, ".partial") ||
		strings.HasSuffix(lower, ".tmp") ||
		strings.HasSuffix(lower, ".swp") ||
		strings.HasSuffix(lower, ".crdownload") {
		return true
	}

	if strings.HasPrefix(name, ".~") || strings.HasPrefix(name, "._") {
		return true
	}

	return strings.HasPrefix(name, "~") && !strings.HasPrefix(name, "~$")
}
