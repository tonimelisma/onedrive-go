package sync

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	gosync "sync"

	ignore "github.com/sabhiram/go-gitignore"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// OneDrive path and name length limits.
const (
	maxPathLength = 400 // characters — OneDrive's max full path length
	maxNameLength = 255 // bytes — filesystem component limit
)

// oneDriveIllegalChars contains characters that OneDrive forbids in file/folder names.
const oneDriveIllegalChars = `"*:<>?/\|`

// safetyS7Suffixes are always excluded to prevent syncing partial/temp files
// that indicate incomplete operations (sync-algorithm.md safety invariant S7).
var safetyS7Suffixes = []string{".partial", ".tmp"}

// safetyS7Prefix is a tilde prefix pattern for temp files (e.g., ~lockfile).
const safetyS7Prefix = "~"

// reservedNames are Windows/OneDrive reserved device names (case-insensitive).
// See sync-algorithm.md section 6.4.
var reservedNames = func() map[string]bool {
	names := map[string]bool{
		"CON": true, "PRN": true, "AUX": true, "NUL": true,
	}

	for i := range 10 {
		names[fmt.Sprintf("COM%d", i)] = true
		names[fmt.Sprintf("LPT%d", i)] = true
	}

	return names
}()

// FilterEngine implements the Filter interface with a three-layer cascade:
// sync_paths allowlist, config patterns (skip_files, skip_dirs, skip_dotfiles,
// max_file_size, S7 safety), and .odignore marker files. It also validates
// OneDrive naming restrictions. See sync-algorithm.md section 6.
type FilterEngine struct {
	cfg      config.FilterConfig
	logger   *slog.Logger
	syncRoot string

	// maxFileSizeBytes is the parsed max_file_size threshold (0 = no limit).
	maxFileSizeBytes int64

	// odignoreCache stores parsed .odignore files per directory path.
	// A nil entry means the directory was checked but had no .odignore file.
	// Protected by mu for concurrent scanner access.
	odignoreCache map[string]*ignore.GitIgnore
	mu            gosync.RWMutex
}

// NewFilterEngine creates a filter engine from the given config and sync root.
// It parses the max_file_size threshold and initializes the .odignore cache.
func NewFilterEngine(cfg *config.FilterConfig, syncRoot string, logger *slog.Logger) (*FilterEngine, error) {
	logger.Info("initializing filter engine",
		"sync_root", syncRoot,
		"skip_dotfiles", cfg.SkipDotfiles,
		"skip_files", cfg.SkipFiles,
		"skip_dirs", cfg.SkipDirs,
		"max_file_size", cfg.MaxFileSize,
		"sync_paths", cfg.SyncPaths,
		"ignore_marker", cfg.IgnoreMarker,
	)

	maxBytes, err := parseSizeFilter(cfg.MaxFileSize)
	if err != nil {
		return nil, fmt.Errorf("invalid max_file_size %q: %w", cfg.MaxFileSize, err)
	}

	return &FilterEngine{
		cfg:              *cfg,
		logger:           logger,
		syncRoot:         syncRoot,
		maxFileSizeBytes: maxBytes,
		odignoreCache:    make(map[string]*ignore.GitIgnore),
	}, nil
}

// ShouldSync evaluates whether the given path should be included in sync.
// It applies the three-layer filter cascade and OneDrive name validation.
// The path must be relative to the sync root.
func (f *FilterEngine) ShouldSync(path string, isDir bool, size int64) FilterResult {
	// Layer 0: Name validation — reject items that OneDrive cannot store.
	if result := f.checkNameValidation(path); !result.Included {
		return result
	}

	// Layer 1: sync_paths allowlist — if configured, only allowed subtrees pass.
	if result := f.checkSyncPaths(path, isDir); !result.Included {
		return result
	}

	// Layer 2: Config patterns — S7 safety, skip_files, skip_dirs, skip_dotfiles, max_file_size.
	if result := f.checkConfigPatterns(path, isDir, size); !result.Included {
		return result
	}

	// Layer 3: .odignore marker files — per-directory gitignore-style patterns.
	if result := f.checkOdignore(path, isDir); !result.Included {
		return result
	}

	return FilterResult{Included: true}
}

// checkNameValidation verifies the path complies with OneDrive naming restrictions.
func (f *FilterEngine) checkNameValidation(path string) FilterResult {
	// Check total path length.
	if valid, reason := isValidPath(path); !valid {
		f.logger.Debug("path excluded by name validation", "path", path, "reason", reason)
		return FilterResult{Included: false, Reason: reason}
	}

	// Check each component for naming rules.
	components := strings.Split(filepath.ToSlash(path), "/")
	for _, comp := range components {
		if comp == "" || comp == "." || comp == ".." {
			continue
		}

		if valid, reason := isValidOneDriveName(comp); !valid {
			f.logger.Debug("path excluded by name validation", "path", path, "component", comp, "reason", reason)
			return FilterResult{Included: false, Reason: reason}
		}
	}

	return FilterResult{Included: true}
}

// checkSyncPaths evaluates Layer 1: the sync_paths allowlist.
// If sync_paths is not configured, all paths pass. Parent directories of
// allowed paths are traversable but not themselves synced as content.
func (f *FilterEngine) checkSyncPaths(path string, isDir bool) FilterResult {
	if len(f.cfg.SyncPaths) == 0 {
		return FilterResult{Included: true}
	}

	if f.matchesSyncPaths(path, isDir) {
		return FilterResult{Included: true}
	}

	f.logger.Debug("path excluded by sync_paths", "path", path)

	return FilterResult{Included: false, Reason: "not in sync_paths"}
}

// checkConfigPatterns evaluates Layer 2: S7 safety patterns, skip_files,
// skip_dirs, skip_dotfiles, and max_file_size.
func (f *FilterEngine) checkConfigPatterns(path string, isDir bool, size int64) FilterResult {
	name := filepath.Base(path)

	// S7 safety patterns always apply — never sync partial/temp files.
	if !isDir {
		if result := f.checkSafetyPatterns(name, path); !result.Included {
			return result
		}
	}

	// skip_dotfiles applies to both files and directories.
	if f.cfg.SkipDotfiles && strings.HasPrefix(name, ".") {
		f.logger.Debug("path excluded by skip_dotfiles", "path", path)
		return FilterResult{Included: false, Reason: "dotfile excluded"}
	}

	if isDir {
		return f.checkDirPatterns(name, path)
	}

	return f.checkFilePatterns(name, path, size)
}

// checkSafetyPatterns checks S7 safety invariant patterns (.partial, .tmp, ~*).
func (f *FilterEngine) checkSafetyPatterns(name, path string) FilterResult {
	lower := strings.ToLower(name)

	for _, suffix := range safetyS7Suffixes {
		if strings.HasSuffix(lower, suffix) {
			f.logger.Debug("path excluded by S7 safety pattern", "path", path, "suffix", suffix)
			return FilterResult{Included: false, Reason: fmt.Sprintf("S7 safety: matches %s pattern", suffix)}
		}
	}

	if strings.HasPrefix(name, safetyS7Prefix) {
		f.logger.Debug("path excluded by S7 safety pattern", "path", path, "prefix", safetyS7Prefix)
		return FilterResult{Included: false, Reason: "S7 safety: matches ~* pattern"}
	}

	return FilterResult{Included: true}
}

// checkDirPatterns checks skip_dirs glob patterns against the directory basename.
func (f *FilterEngine) checkDirPatterns(name, path string) FilterResult {
	if matchesSkipPattern(name, f.cfg.SkipDirs) {
		f.logger.Debug("path excluded by skip_dirs", "path", path, "name", name)
		return FilterResult{Included: false, Reason: "matches skip_dirs pattern"}
	}

	return FilterResult{Included: true}
}

// checkFilePatterns checks skip_files glob patterns and max_file_size threshold.
func (f *FilterEngine) checkFilePatterns(name, path string, size int64) FilterResult {
	if matchesSkipPattern(name, f.cfg.SkipFiles) {
		f.logger.Debug("path excluded by skip_files", "path", path, "name", name)
		return FilterResult{Included: false, Reason: "matches skip_files pattern"}
	}

	if f.maxFileSizeBytes > 0 && size > f.maxFileSizeBytes {
		f.logger.Debug("path excluded by max_file_size",
			"path", path, "size", size, "max", f.maxFileSizeBytes)
		return FilterResult{Included: false, Reason: "exceeds max_file_size"}
	}

	return FilterResult{Included: true}
}

// checkOdignore evaluates Layer 3: .odignore marker file patterns.
func (f *FilterEngine) checkOdignore(path string, isDir bool) FilterResult {
	if f.cfg.IgnoreMarker == "" {
		return FilterResult{Included: true}
	}

	dir := filepath.Dir(path)
	gi := f.loadOdignore(dir)

	if gi == nil {
		return FilterResult{Included: true}
	}

	// go-gitignore expects forward slashes and uses trailing slash for dirs.
	matchPath := filepath.ToSlash(path)
	if isDir {
		matchPath += "/"
	}

	if gi.MatchesPath(matchPath) {
		f.logger.Debug("path excluded by .odignore", "path", path, "dir", dir)
		return FilterResult{Included: false, Reason: "excluded by " + f.cfg.IgnoreMarker}
	}

	return FilterResult{Included: true}
}

// matchesSyncPaths checks whether path falls under any configured sync_path.
// A path matches if it equals, is a child of, or is a parent of a sync path.
// Parent directories are "traversable" — they pass the filter so the scanner
// can reach the allowed subtrees, but their own content is not synced.
func (f *FilterEngine) matchesSyncPaths(path string, isDir bool) bool {
	normalPath := filepath.ToSlash(filepath.Clean(path))

	for _, sp := range f.cfg.SyncPaths {
		normalSP := filepath.ToSlash(filepath.Clean(sp))

		// Exact match — always included.
		if normalPath == normalSP {
			return true
		}

		// Path is under a sync path — included.
		if strings.HasPrefix(normalPath, normalSP+"/") {
			return true
		}

		// Path is a parent of a sync path — traversable (directories only).
		if isDir && strings.HasPrefix(normalSP, normalPath+"/") {
			return true
		}
	}

	return false
}

// matchesSkipPattern checks if name matches any of the given glob patterns.
// Comparison is case-insensitive. Malformed patterns are logged and skipped.
func matchesSkipPattern(name string, patterns []string) bool {
	lowerName := strings.ToLower(name)

	for _, pattern := range patterns {
		lowerPattern := strings.ToLower(pattern)

		matched, err := filepath.Match(lowerPattern, lowerName)
		if err != nil {
			// Malformed pattern — skip it rather than failing the entire filter.
			slog.Warn("malformed skip pattern", "pattern", pattern, "error", err)
			continue
		}

		if matched {
			return true
		}
	}

	return false
}

// loadOdignore loads and caches the .odignore file for the given directory.
// Returns nil if no .odignore file exists in that directory.
func (f *FilterEngine) loadOdignore(dir string) *ignore.GitIgnore {
	// Fast path: check cache with read lock.
	f.mu.RLock()
	gi, cached := f.odignoreCache[dir]
	f.mu.RUnlock()

	if cached {
		return gi
	}

	// Slow path: load from disk and cache.
	f.mu.Lock()
	defer f.mu.Unlock()

	// Double-check after acquiring write lock (another goroutine may have loaded it).
	if gi, cached = f.odignoreCache[dir]; cached {
		return gi
	}

	odignorePath := filepath.Join(f.syncRoot, dir, f.cfg.IgnoreMarker)

	parsed, err := ignore.CompileIgnoreFile(odignorePath)
	if err != nil {
		// File doesn't exist or is unreadable — cache nil to avoid repeated attempts.
		f.logger.Debug("no .odignore file found", "dir", dir, "path", odignorePath)
		f.odignoreCache[dir] = nil

		return nil
	}

	f.logger.Debug("loaded .odignore file", "dir", dir, "path", odignorePath)
	f.odignoreCache[dir] = parsed

	return parsed
}

// isValidOneDriveName checks whether a single path component is valid for OneDrive.
// Returns (true, "") if valid, or (false, reason) if invalid.
func isValidOneDriveName(name string) (bool, string) {
	// Check for illegal characters.
	for _, ch := range name {
		if strings.ContainsRune(oneDriveIllegalChars, ch) {
			return false, fmt.Sprintf("contains illegal character %q", string(ch))
		}
	}

	// Check reserved names (case-insensitive, with or without extension).
	upper := strings.ToUpper(name)
	baseName := upper
	if dot := strings.IndexByte(upper, '.'); dot >= 0 {
		baseName = upper[:dot]
	}

	if reservedNames[baseName] {
		return false, fmt.Sprintf("%q is a reserved name", name)
	}

	// Check trailing dots and spaces.
	if strings.HasSuffix(name, ".") {
		return false, "name ends with a dot"
	}

	if strings.HasSuffix(name, " ") {
		return false, "name ends with a space"
	}

	// Check leading whitespace (spaces, tabs, etc.).
	if name != "" && name[0] == ' ' {
		return false, "name starts with a space"
	}

	// Check names starting with ~$ (Office lock files).
	if strings.HasPrefix(name, "~$") {
		return false, "name starts with ~$"
	}

	// Check names containing _vti_ (SharePoint internal).
	if strings.Contains(name, "_vti_") {
		return false, "name contains _vti_"
	}

	// Check component length (filesystem limit).
	if len(name) > maxNameLength {
		return false, fmt.Sprintf("name exceeds %d bytes", maxNameLength)
	}

	return true, ""
}

// isValidPath checks whether the full relative path is within OneDrive's length limit.
func isValidPath(path string) (bool, string) {
	// OneDrive measures path length in characters (runes), not bytes.
	if len([]rune(path)) > maxPathLength {
		return false, fmt.Sprintf("path exceeds %d characters", maxPathLength)
	}

	return true, ""
}

// Size multiplier constants for parseSizeFilter (decimal / SI).
// Duplicated from config package because config.parseSize is unexported.
const (
	filterKilobyte = 1000
	filterMegabyte = 1000 * filterKilobyte
	filterGigabyte = 1000 * filterMegabyte
	filterTerabyte = 1000 * filterGigabyte
)

// Size multiplier constants (binary / IEC).
const (
	filterKibibyte = 1024
	filterMebibyte = 1024 * filterKibibyte
	filterGibibyte = 1024 * filterMebibyte
	filterTebibyte = 1024 * filterGibibyte
)

// parseSizeFilter converts a human-readable size string (e.g., "50GB", "10MiB") to bytes.
// Returns 0 for empty string or "0" (meaning no limit).
// This duplicates config.parseSize because that function is unexported;
// exporting it would require modifying config package files owned by Agent E.
func parseSizeFilter(s string) (int64, error) {
	if s == "" || s == "0" {
		return 0, nil
	}

	s = strings.TrimSpace(s)
	upper := strings.ToUpper(s)

	suffixes := []struct {
		suffix     string
		multiplier int64
	}{
		{"TIB", filterTebibyte},
		{"GIB", filterGibibyte},
		{"MIB", filterMebibyte},
		{"KIB", filterKibibyte},
		{"TB", filterTerabyte},
		{"GB", filterGigabyte},
		{"MB", filterMegabyte},
		{"KB", filterKilobyte},
		{"B", 1},
	}

	for _, sf := range suffixes {
		if strings.HasSuffix(upper, sf.suffix) {
			numStr := strings.TrimSpace(s[:len(s)-len(sf.suffix)])

			return parseSizeNumber(numStr, sf.multiplier)
		}
	}

	// No suffix: treat as raw bytes.
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}

	if n < 0 {
		return 0, fmt.Errorf("invalid size %q: must be non-negative", s)
	}

	return n, nil
}

// parseSizeNumber parses the numeric portion of a size string and applies the multiplier.
func parseSizeNumber(numStr string, multiplier int64) (int64, error) {
	n, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size number %q: %w", numStr, err)
	}

	result := int64(n * float64(multiplier))
	if result < 0 {
		return 0, fmt.Errorf("invalid size: must be non-negative")
	}

	return result, nil
}
