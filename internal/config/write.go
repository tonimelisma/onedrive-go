package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/fsroot"
)

// configFilePermissions is the standard permission mode for config files.
// Owner read/write, group and others read-only.
const configFilePermissions = 0o644

// configDirPermissions is the standard permission mode for config directories.
const configDirPermissions = 0o755

// Drive type aliases for readability.
const (
	driveTypePersonal   = driveid.DriveTypePersonal
	driveTypeBusiness   = driveid.DriveTypeBusiness
	driveTypeSharePoint = driveid.DriveTypeSharePoint
	driveTypeShared     = driveid.DriveTypeShared
)

// defaultConfigTemplate returns the first-login config content. All global
// settings are present as commented-out defaults so users can discover every
// option without reading docs. The template uses the live default constants so
// docs and config creation do not drift when defaults change.
func defaultConfigTemplate() string {
	return fmt.Sprintf(`# onedrive-go configuration
# Docs: https://github.com/tonimelisma/onedrive-go

# ── Global settings ──
# Uncomment and modify to override defaults.

# Transfers
# transfer_workers = %d
# check_workers = %d

# Safety
# min_free_space = %q

# Sync runtime
# poll_interval = %q
# websocket = false
# dry_run = false

# Logging
# log_level = %q
# log_file = %q
# log_format = %q
# log_retention_days = %d

# ── Drives ──
# Added automatically by 'login' and 'drive add'.
# Each section name is the canonical drive identifier.
`,
		defaultTransferWorkers,
		defaultCheckWorkers,
		defaultMinFreeSpace,
		defaultPollInterval,
		defaultLogLevel,
		"",
		defaultLogFormat,
		defaultLogRetentionDays,
	)
}

// driveSection generates the TOML text for a new drive section. The blank
// line before the header is intentional — it visually separates drive
// sections from each other and from the global settings.
func driveSection(canonicalID, syncDir string) string {
	return fmt.Sprintf("\n[%q]\nsync_dir = %q\n", canonicalID, syncDir)
}

// AppendDriveSection appends a new drive section to a config file. If the file
// does not exist, it is created from the default template first. Used by login
// and `drive add`. The write is atomic to avoid partial writes on crash.
func AppendDriveSection(path string, canonicalID driveid.CanonicalID, syncDir string) error {
	data, err := readManagedFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("reading config file: %w", err)
		}

		// File doesn't exist — create from template.
		content := defaultConfigTemplate() + driveSection(canonicalID.String(), syncDir)

		return atomicWriteFile(path, []byte(content))
	}

	content := string(data)

	// Ensure the file ends with a newline before appending, so the new
	// section header starts on its own line.
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	content += driveSection(canonicalID.String(), syncDir)

	return atomicWriteFile(path, []byte(content))
}

// EnsureDriveInConfig is the single entry point for adding a drive to the config
// file. It loads the config (or defaults if missing), checks whether the drive
// already exists, computes the default sync_dir from catalog account data, and writes
// the drive section. Returns the sync directory, whether a new section was added,
// and any error. Used by both login and `drive add`.
func EnsureDriveInConfig(path string, cid driveid.CanonicalID, logger *slog.Logger) (string, bool, error) {
	cfg, err := LoadOrDefault(path, logger)
	if err != nil {
		return "", false, fmt.Errorf("loading config: %w", err)
	}

	if d, exists := cfg.Drives[cid]; exists {
		if d.SyncDir != "" {
			return d.SyncDir, false, nil
		}

		orgName, displayName := ResolveAccountNames(cid, logger)
		existingDirs := CollectOtherSyncDirs(cfg, cid, logger)
		syncDir := DefaultSyncDir(cid, orgName, displayName, existingDirs)
		if err := SetDriveKey(path, cid, "sync_dir", syncDir); err != nil {
			return "", false, fmt.Errorf("writing drive sync_dir: %w", err)
		}

		return syncDir, false, nil
	}

	// Use the catalog account record for org_name/display_name.
	orgName, displayName := ResolveAccountNames(cid, logger)

	existingDirs := CollectOtherSyncDirs(cfg, cid, logger)
	syncDir := DefaultSyncDir(cid, orgName, displayName, existingDirs)

	if err := AppendDriveSection(path, cid, syncDir); err != nil {
		return "", false, fmt.Errorf("writing drive config: %w", err)
	}

	return syncDir, true, nil
}

// SetDriveKey finds a drive section by canonical ID and sets a key-value pair.
// If the key already exists within the section, its line is replaced (preserving
// any inline comment). If not found, the key is inserted after the section header.
//
// Value formatting: booleans ("true"/"false") are written without quotes;
// all other values are written as quoted strings.
func SetDriveKey(path string, canonicalID driveid.CanonicalID, key, value string) error {
	data, err := readManagedFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	lines := parseLines(string(data))

	headerIdx, found := findSectionByName(lines, canonicalID.String())
	if !found {
		return fmt.Errorf("drive section %q not found in config", canonicalID.String())
	}

	contentStart, contentEnd := sectionContentRange(lines, headerIdx)
	formattedValue := formatTOMLValue(value)

	if idx, keyFound := findKeyInRange(lines, contentStart, contentEnd, key); keyFound {
		// Replace existing key, preserving inline comment.
		lines[idx].raw = renderKeyValueLine(key, formattedValue, lines[idx].inlineComment)
	} else {
		// Insert new key after section header.
		newLine := parseLine(renderKeyValueLine(key, formattedValue, ""))
		lines = append(lines[:headerIdx+1], append([]parsedLine{newLine}, lines[headerIdx+1:]...)...)
	}

	return atomicWriteFile(path, []byte(renderLines(lines)))
}

// DeleteDriveKey removes a single key from a drive section. Idempotent:
// returns nil if the key does not exist in the section. Used by `resume`
// to clear `paused` and `paused_until` keys.
func DeleteDriveKey(path string, canonicalID driveid.CanonicalID, key string) error {
	data, err := readManagedFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	lines := parseLines(string(data))

	headerIdx, found := findSectionByName(lines, canonicalID.String())
	if !found {
		return fmt.Errorf("drive section %q not found in config", canonicalID.String())
	}

	contentStart, contentEnd := sectionContentRange(lines, headerIdx)

	if idx, keyFound := findKeyInRange(lines, contentStart, contentEnd, key); keyFound {
		lines = append(lines[:idx], lines[idx+1:]...)
	}

	return atomicWriteFile(path, []byte(renderLines(lines)))
}

// DeleteDriveSection removes a drive section (header + all keys) from the
// config file. Also removes blank lines immediately preceding the section
// header for clean formatting. Used by `drive remove --purge` and
// `logout --purge`.
func DeleteDriveSection(path string, canonicalID driveid.CanonicalID) error {
	data, err := readManagedFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	lines := parseLines(string(data))

	headerIdx, found := findSectionByName(lines, canonicalID.String())
	if !found {
		return fmt.Errorf("drive section %q not found in config", canonicalID.String())
	}

	_, contentEnd := sectionContentRange(lines, headerIdx)

	// Remove preceding blank lines for clean formatting.
	blankStart := headerIdx
	for blankStart > 0 && lines[blankStart-1].kind == lineBlank {
		blankStart--
	}

	lines = append(lines[:blankStart], lines[contentEnd:]...)

	return atomicWriteFile(path, []byte(renderLines(lines)))
}

// RenameDriveSections renames one or more drive section headers in place while
// preserving their contents, comments, and relative order. A target section
// that already exists is treated as a collision when the source section also
// exists, because the config would otherwise end up with two owners for the
// same canonical ID.
func RenameDriveSections(path string, renames map[driveid.CanonicalID]driveid.CanonicalID) error {
	if len(renames) == 0 {
		return nil
	}

	data, err := readManagedFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("reading config file: %w", err)
	}

	lines := parseLines(string(data))
	headerIndex := make(map[string]int)
	for i := range lines {
		if lines[i].kind == lineSection {
			headerIndex[lines[i].sectionName] = i
		}
	}

	for from, to := range renames {
		if from.Equal(to) {
			continue
		}

		fromName := from.String()
		toName := to.String()

		fromIdx, fromFound := headerIndex[fromName]
		_, toFound := headerIndex[toName]

		switch {
		case !fromFound:
			continue
		case toFound:
			return fmt.Errorf("config section rename collision: %q already exists", toName)
		default:
			lines[fromIdx].sectionName = toName
			lines[fromIdx].raw = renderSectionHeaderLine(toName)
			delete(headerIndex, fromName)
			headerIndex[toName] = fromIdx
		}
	}

	return atomicWriteFile(path, []byte(renderLines(lines)))
}

// DefaultSyncDir computes a default sync directory for a drive. Uses a two-level
// scheme: base name first, disambiguated with display name then email on collision.
// All callers must pass existingDirs (tilde-expanded or unexpanded) for accurate
// collision detection.
//
// Personal:   ~/OneDrive → ~/OneDrive - {displayName} → ~/OneDrive - {email}
// Business:   ~/OneDrive - {OrgName} → ~/OneDrive - {OrgName} - {displayName} → + {email}
//
//	(~/OneDrive - Business if no org name)
//
// SharePoint: ~/SharePoint - {site} - {library} → + {displayName} → + {email}
func DefaultSyncDir(cid driveid.CanonicalID, orgName, displayName string, existingDirs []string) string {
	base := BaseSyncDir(cid, orgName, displayName)
	if base == "" {
		return ""
	}

	if !containsExpanded(existingDirs, base) {
		return base
	}

	// Level 1: disambiguate with display name (friendly).
	if displayName != "" {
		withName := base + " - " + SanitizePathComponent(displayName)
		if !containsExpanded(existingDirs, withName) {
			return withName
		}
	}

	// Level 2: disambiguate with email (guaranteed unique).
	return base + " - " + cid.Email()
}

// BaseSyncDir returns the base sync directory name for a drive type, without
// collision detection. Exported for use by collectOtherSyncDirs which needs
// the base name without triggering a collision cascade.
//
// The displayName parameter is only used for shared drives (to create
// per-drive subdirectories under ~/OneDrive-Shared/). Personal, business,
// and SharePoint drives ignore it.
func BaseSyncDir(cid driveid.CanonicalID, orgName, displayName string) string {
	switch cid.DriveType() {
	case driveTypePersonal:
		return "~/OneDrive"
	case driveTypeBusiness:
		if orgName != "" {
			return "~/OneDrive - " + SanitizePathComponent(orgName)
		}

		return "~/OneDrive - Business"
	case driveTypeSharePoint:
		site, lib := cid.Site(), cid.Library()
		if site != "" && lib != "" {
			return fmt.Sprintf("~/SharePoint - %s - %s",
				SanitizePathComponent(site), SanitizePathComponent(lib))
		}

		return "~/SharePoint"
	case driveTypeShared:
		if displayName != "" {
			return "~/OneDrive-Shared/" + SanitizePathComponent(displayName)
		}

		return "~/OneDrive-Shared"
	default:
		return ""
	}
}

// containsExpanded compares with tilde expansion so
// "~/OneDrive" matches "/home/user/OneDrive".
func containsExpanded(dirs []string, candidate string) bool {
	expanded := expandTilde(candidate)

	for _, d := range dirs {
		if expandTilde(d) == expanded {
			return true
		}
	}

	return false
}

// SanitizePathComponent replaces filesystem-unsafe characters with "-".
// Exported for use by callers that build path components from user data.
func SanitizePathComponent(s string) string {
	// Replace: / \ : < > " | ? *
	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		":", "-",
		"<", "-",
		">", "-",
		"\"", "-",
		"|", "-",
		"?", "-",
		"*", "-",
	)

	result := replacer.Replace(s)

	// Collapse consecutive dashes.
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}

	return strings.Trim(result, "- ")
}

// formatTOMLValue formats a value for TOML output. Booleans are written
// bare (true/false); all other values are quoted strings.
func formatTOMLValue(value string) string {
	if value == "true" || value == "false" {
		return value
	}

	return fmt.Sprintf("%q", value)
}

// atomicWriteFile writes data to a temporary file in the same directory as
// path, then renames it to the target path. This prevents partial writes
// from corrupting the config file on crash. Parent directories are created
// as needed. Files are created with configFilePermissions (0644).
func atomicWriteFile(path string, data []byte) (err error) {
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return fmt.Errorf("opening config root: %w", err)
	}

	if err := root.MkdirAll(configDirPermissions); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	if err := root.AtomicWrite(name, data, configFilePermissions, configDirPermissions, ".config-*.tmp"); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}

	return nil
}
