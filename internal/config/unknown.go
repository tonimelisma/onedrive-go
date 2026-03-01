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

// knownGlobalKeys are the valid flat top-level keys in the config file.
// These correspond to fields in the embedded sub-config structs.
var knownGlobalKeys = map[string]bool{
	// Filter settings
	"skip_files": true, "skip_dirs": true, "skip_dotfiles": true,
	"skip_symlinks": true, "max_file_size": true, "sync_paths": true, "ignore_marker": true,
	// Transfer settings
	"parallel_downloads": true, "parallel_uploads": true, "parallel_checkers": true,
	"chunk_size": true, "bandwidth_limit": true, "bandwidth_schedule": true, "transfer_order": true,
	// Safety settings
	"big_delete_threshold": true, "big_delete_percentage": true, "big_delete_min_items": true,
	"min_free_space": true, "use_recycle_bin": true, "use_local_trash": true,
	"disable_download_validation": true, "disable_upload_validation": true,
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

// knownGlobalKeysList is the sorted slice form of knownGlobalKeys for
// Levenshtein matching. Sorted for deterministic suggestions when two
// candidates have the same edit distance.
var knownGlobalKeysList = func() []string {
	keys := make([]string, 0, len(knownGlobalKeys))
	for k := range knownGlobalKeys {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}()

// knownDriveKeys are the valid keys inside a drive section.
var knownDriveKeys = map[string]bool{
	"sync_dir": true, "state_dir": true, "paused": true, "alias": true, "remote_path": true,
	"drive_id": true, "skip_dotfiles": true, "skip_dirs": true, "skip_files": true,
	"poll_interval": true,
}

// knownDriveKeysList is the sorted slice form for Levenshtein matching.
// Sorted for deterministic suggestions when two candidates have the same
// edit distance.
var knownDriveKeysList = func() []string {
	keys := make([]string, 0, len(knownDriveKeys))
	for k := range knownDriveKeys {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}()

// checkUnknownKeys inspects TOML metadata for undecoded keys and returns
// an error with "did you mean?" suggestions for each unknown key.
// Drive sections (keys containing ":") are skipped because they are parsed
// separately in the two-pass decode.
func checkUnknownKeys(md *toml.MetaData) error {
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

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

// buildGlobalKeyError creates a descriptive error for an unknown top-level key,
// optionally suggesting the closest known key. Returns nil if the key is a
// valid sub-field of a known key (e.g., bandwidth_schedule entries).
func buildGlobalKeyError(keyStr string) error {
	// For nested keys like "bandwidth_schedule.time", extract the leaf.
	parts := strings.SplitN(keyStr, ".", 2)
	fieldName := parts[0]

	if len(parts) > 1 {
		// Nested unknown key — e.g., a sub-field of bandwidth_schedule entries.
		// These are valid TOML but undecoded because of array-of-tables structure.
		if knownGlobalKeys[fieldName] {
			return nil // parent is known, sub-field is expected
		}
	}

	suggestion := closestMatch(fieldName, knownGlobalKeysList)
	if suggestion != "" {
		return fmt.Errorf("unknown config key %q — did you mean %q?", fieldName, suggestion)
	}

	return fmt.Errorf("unknown config key %q", fieldName)
}

// checkDriveUnknownKeys validates that all keys in a drive section map are
// recognized drive keys. Returns an error with suggestions for unknown keys.
func checkDriveUnknownKeys(driveMap map[string]any, canonicalID string) error {
	var errs []error

	for key := range driveMap {
		if knownDriveKeys[key] {
			continue
		}

		suggestion := closestMatch(key, knownDriveKeysList)
		if suggestion != "" {
			errs = append(errs, fmt.Errorf(
				"unknown key %q in drive [%q] — did you mean %q?", key, canonicalID, suggestion))
		} else {
			errs = append(errs, fmt.Errorf("unknown key %q in drive [%q]", key, canonicalID))
		}
	}

	return errors.Join(errs...)
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
