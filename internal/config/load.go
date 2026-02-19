package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// maxLevenshteinDistance is the maximum edit distance for "did you mean?"
// suggestions when unknown config keys are detected.
const maxLevenshteinDistance = 3

// sectionTopLevel is used when an unknown key has no section prefix.
const sectionTopLevel = "top-level"

// Load reads and parses a TOML config file, validates it, and returns the
// resulting Config. Unknown keys are treated as fatal errors with suggestions.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	if err := checkUnknownKeys(&md); err != nil {
		return nil, err
	}

	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
}

// LoadOrDefault reads a TOML config file if it exists, otherwise returns
// a Config with all default values.
func LoadOrDefault(path string) (*Config, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return DefaultConfig(), nil
	}

	return Load(path)
}

// checkUnknownKeys inspects TOML metadata for undecoded keys and returns
// an error with "did you mean?" suggestions for each unknown key.
func checkUnknownKeys(md *toml.MetaData) error {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}

	var errs []error

	for _, key := range undecoded {
		errs = append(errs, buildUnknownKeyError(key))
	}

	return errors.Join(errs...)
}

// knownFilterKeys are the valid keys in the [filter] section.
var knownFilterKeys = []string{
	"skip_files", "skip_dirs", "skip_dotfiles",
	"skip_symlinks", "max_file_size", "sync_paths", "ignore_marker",
}

// knownTransfersKeys are the valid keys in the [transfers] section.
var knownTransfersKeys = []string{
	"parallel_downloads", "parallel_uploads", "parallel_checkers",
	"chunk_size", "bandwidth_limit", "bandwidth_schedule", "transfer_order",
}

// knownSafetyKeys are the valid keys in the [safety] section.
var knownSafetyKeys = []string{
	"big_delete_threshold", "big_delete_percentage", "big_delete_min_items",
	"min_free_space", "use_recycle_bin", "use_local_trash",
	"disable_download_validation", "disable_upload_validation",
	"sync_dir_permissions", "sync_file_permissions", "tombstone_retention_days",
}

// knownSyncKeys are the valid keys in the [sync] section.
var knownSyncKeys = []string{
	"poll_interval", "fullscan_frequency", "websocket",
	"conflict_strategy", "conflict_reminder_interval",
	"dry_run", "verify_interval", "shutdown_timeout",
}

// knownLoggingKeys are the valid keys in the [logging] section.
var knownLoggingKeys = []string{
	"log_level", "log_file", "log_format", "log_retention_days",
}

// knownNetworkKeys are the valid keys in the [network] section.
var knownNetworkKeys = []string{
	"connect_timeout", "data_timeout", "user_agent", "force_http_11",
}

// knownSectionKeys maps each config section to its valid keys.
var knownSectionKeys = map[string][]string{
	"filter":    knownFilterKeys,
	"transfers": knownTransfersKeys,
	"safety":    knownSafetyKeys,
	"sync":      knownSyncKeys,
	"logging":   knownLoggingKeys,
	"network":   knownNetworkKeys,
}

// topLevelSections are the valid top-level section names.
var topLevelSections = []string{
	"profile", "filter", "transfers", "safety", "sync", "logging", "network",
}

// knownProfileKeys are the valid direct keys inside a [profile.NAME] section.
var knownProfileKeys = []string{
	"account_type", "sync_dir", "remote_path", "drive_id",
	"application_id", "azure_ad_endpoint", "azure_tenant_id",
	"filter", "transfers", "safety", "sync", "logging", "network",
}

// profileSectionDepth is the minimum number of dot-separated parts
// for a key to be inside a [profile.NAME] section (e.g., "profile.work.sync_dir").
const profileSectionDepth = 3

// buildUnknownKeyError creates a descriptive error for a single unknown key,
// optionally suggesting the closest known key.
func buildUnknownKeyError(key toml.Key) error {
	keyParts := key.String()

	section, fieldName, isProfile := classifyKey(keyParts)

	if isProfile {
		return buildProfileKeyError(section, fieldName)
	}

	return buildSectionKeyError(section, fieldName)
}

// classifyKey determines whether a key belongs to a profile section or a
// regular section, and extracts the relevant section and field name.
func classifyKey(keyStr string) (section, field string, isProfile bool) {
	parts := strings.Split(keyStr, ".")

	if len(parts) >= profileSectionDepth && parts[0] == "profile" {
		return classifyProfileKey(parts)
	}

	return classifyRegularKey(parts), extractField(parts), false
}

// classifyProfileKey handles keys within [profile.NAME...] sections.
func classifyProfileKey(parts []string) (section, field string, isProfile bool) {
	profileName := parts[1]

	if len(parts) == profileSectionDepth {
		// e.g., "profile.work.sync_dir" -> field in profile
		return fmt.Sprintf("profile.%s", profileName), parts[2], true
	}

	// e.g., "profile.work.filter.skip_files" -> field in profile subsection
	subSection := parts[2]

	return fmt.Sprintf("profile.%s.%s", profileName, subSection), parts[profileSectionDepth], true
}

// classifyRegularKey handles non-profile keys.
func classifyRegularKey(parts []string) string {
	if len(parts) == 1 {
		return sectionTopLevel
	}

	return parts[0]
}

// extractField returns the field name from key parts.
func extractField(parts []string) string {
	if len(parts) == 1 {
		return parts[0]
	}

	return parts[1]
}

// profileSubSectionParts is the number of dot-separated parts in
// a profile subsection path like "profile.work.filter".
const profileSubSectionParts = 3

// buildProfileKeyError creates an error for an unknown key inside a profile.
func buildProfileKeyError(section, fieldName string) error {
	parts := strings.Split(section, ".")

	var known []string

	if len(parts) == profileSubSectionParts {
		// Inside a profile subsection like profile.work.filter
		subSection := parts[2]
		known = knownKeysForGlobalSection(subSection)
	} else {
		// Inside profile.NAME directly
		known = knownProfileKeys
	}

	suggestion := closestMatch(fieldName, known)
	if suggestion != "" {
		return fmt.Errorf(
			"unknown config key %q in [%s] — did you mean %q?",
			fieldName, section, suggestion)
	}

	return fmt.Errorf("unknown config key %q in [%s]", fieldName, section)
}

// buildSectionKeyError creates an error for an unknown key in a regular section.
func buildSectionKeyError(section, fieldName string) error {
	known := knownKeysForGlobalSection(section)
	suggestion := closestMatch(fieldName, known)

	if suggestion != "" {
		return fmt.Errorf(
			"unknown config key %q in [%s] — did you mean %q?",
			fieldName, section, suggestion)
	}

	return fmt.Errorf("unknown config key %q in [%s]", fieldName, section)
}

// knownKeysForGlobalSection returns the valid keys for a given global section.
// For "top-level", it returns the section names. For bandwidth_schedule
// entries, it returns the entry field names.
func knownKeysForGlobalSection(section string) []string {
	if section == sectionTopLevel {
		return topLevelSections
	}

	if keys, ok := knownSectionKeys[section]; ok {
		return keys
	}

	// Could be a nested key like "bandwidth_schedule.time".
	return []string{"time", "limit"}
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
