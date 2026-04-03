package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// maxLevenshteinDistance is the maximum edit distance for "did you mean?"
// suggestions when unknown config keys are detected.
const maxLevenshteinDistance = 3

// newKnownGlobalKeys returns the set of valid flat top-level keys in the
// config file. Returns a fresh map each call to avoid package-level mutable
// state — config loading is infrequent so the allocation is negligible.
func newKnownGlobalKeys() map[string]bool {
	return map[string]bool{
		// Filter settings
		"skip_files": true, "skip_dirs": true, "skip_dotfiles": true,
		"skip_symlinks": true, "sync_paths": true, "ignore_marker": true,
		// Transfer settings
		"transfer_workers": true, "check_workers": true,
		"chunk_size": true, "bandwidth_limit": true, "bandwidth_schedule": true, "transfer_order": true,
		// Deprecated transfer settings (kept to produce deprecation warning instead of unknown-key error)
		"parallel_downloads": true, "parallel_uploads": true, "parallel_checkers": true,
		// Safety settings
		"big_delete_threshold": true,
		"min_free_space":       true, "use_local_trash": true,
		"sync_dir_permissions": true, "sync_file_permissions": true,
		// Sync settings
		"poll_interval": true, "fullscan_frequency": true, "websocket": true,
		"conflict_strategy": true, "conflict_reminder_interval": true, "dry_run": true,
		"verify_interval": true, "shutdown_timeout": true,
		// Logging settings
		"log_level": true, "log_file": true, "log_format": true, "log_retention_days": true,
		// Network settings
		"connect_timeout": true, "data_timeout": true, "user_agent": true, "force_http_11": true,
	}
}

// newKnownGlobalKeysList returns the sorted slice form of known global keys
// for Levenshtein matching. Sorted for deterministic suggestions when two
// candidates have the same edit distance.
func newKnownGlobalKeysList() []string {
	m := newKnownGlobalKeys()
	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// newKnownDriveKeys returns the set of valid keys inside a drive section.
// Returns a fresh map each call to avoid package-level mutable state.
func newKnownDriveKeys() map[string]bool {
	return map[string]bool{
		"sync_dir": true, "paused": true, "paused_until": true, "display_name": true, "owner": true,
		"skip_dotfiles": true, "skip_dirs": true, "skip_files": true, "poll_interval": true,
	}
}

// newKnownDriveKeysList returns the sorted slice form of known drive keys
// for Levenshtein matching. Sorted for deterministic suggestions when two
// candidates have the same edit distance.
func newKnownDriveKeysList() []string {
	m := newKnownDriveKeys()
	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// collectUnknownGlobalKeyErrors inspects TOML metadata for undecoded keys
// and returns individual errors for each unknown global key. Drive sections
// (keys containing ":") are skipped because they are parsed separately.
// Used by both the strict path (checkUnknownKeys) and the lenient path
// (LoadLenient, which collects these as warnings).
func collectUnknownGlobalKeyErrors(md *toml.MetaData) []error {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}

	var errs []error

	for _, key := range undecoded {
		keyStr := key.String()

		// Skip drive sections — they contain ":" and are handled separately.
		topKey := strings.SplitN(keyStr, ".", 2)[0]
		if strings.Contains(topKey, ":") {
			continue
		}

		if err := buildGlobalKeyError(keyStr); err != nil {
			errs = append(errs, err)
		}
	}

	return errs
}

// checkUnknownKeys wraps collectUnknownGlobalKeyErrors for the strict loading
// path, joining all errors into a single error.
func checkUnknownKeys(md *toml.MetaData) error {
	return errors.Join(collectUnknownGlobalKeyErrors(md)...)
}

// buildGlobalKeyError creates a descriptive error for an unknown top-level key,
// optionally suggesting the closest known key. Returns nil if the key is a
// known key (including deprecated keys that have no struct field but are still
// accepted) or a valid sub-field of a known key (e.g., bandwidth_schedule entries).
func buildGlobalKeyError(keyStr string) error {
	// For nested keys like "bandwidth_schedule.time", extract the leaf.
	parts := strings.SplitN(keyStr, ".", 2)
	fieldName := parts[0]

	// Known key (possibly deprecated — accepted but value ignored).
	// This handles deprecated keys like parallel_downloads that are in
	// the known set but no longer have struct fields.
	if newKnownGlobalKeys()[fieldName] {
		return nil
	}

	// Nested unknown keys (sub-fields of bandwidth_schedule, etc.) fall
	// through to the suggestion path — parent was already checked above.
	suggestion := closestMatch(fieldName, newKnownGlobalKeysList())
	if suggestion != "" {
		return fmt.Errorf("unknown config key %q — did you mean %q?", fieldName, suggestion)
	}

	return fmt.Errorf("unknown config key %q", fieldName)
}

// collectDriveUnknownKeyErrors returns individual errors for each unknown key
// in a drive section. Used by decodeDriveSectionsInternal for both strict and
// lenient parsing modes.
func collectDriveUnknownKeyErrors(driveMap map[string]any, canonicalID string) []error {
	var errs []error

	known := newKnownDriveKeys()
	knownList := newKnownDriveKeysList()

	for key := range driveMap {
		if known[key] {
			continue
		}

		suggestion := closestMatch(key, knownList)
		if suggestion != "" {
			errs = append(errs, fmt.Errorf(
				"unknown key %q in drive [%q] — did you mean %q?", key, canonicalID, suggestion))
		} else {
			errs = append(errs, fmt.Errorf("unknown key %q in drive [%q]", key, canonicalID))
		}
	}

	return errs
}

// closestMatch finds the closest known key by Levenshtein distance.
// Returns empty string if no match is within maxLevenshteinDistance.
func closestMatch(unknown string, known []string) string {
	best := ""
	bestDist := maxLevenshteinDistance + 1

	for _, k := range known {
		d := levenshtein(unknown, k)
		if d < bestDist {
			bestDist = d
			best = k
		}
	}

	if bestDist <= maxLevenshteinDistance {
		return best
	}

	return ""
}

// levenshtein computes the edit distance between two strings.
func levenshtein(a, b string) int {
	if a == "" {
		return len(b)
	}

	if b == "" {
		return len(a)
	}

	// Use single-row optimization to avoid allocating a full matrix.
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)

	for j := range prev {
		prev[j] = j
	}

	for i := range len(a) {
		curr[0] = i + 1

		for j := range len(b) {
			cost := 1
			if a[i] == b[j] {
				cost = 0
			}

			curr[j+1] = minOf(curr[j]+1, prev[j+1]+1, prev[j]+cost)
		}

		prev, curr = curr, prev
	}

	return prev[len(b)]
}

// minOf returns the minimum of three integers.
func minOf(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}

	if c < m {
		m = c
	}

	return m
}
