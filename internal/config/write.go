package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// configFilePermissions is the standard permission mode for config files.
// Owner read/write, group and others read-only.
const configFilePermissions = 0o644

// configDirPermissions is the standard permission mode for config directories.
const configDirPermissions = 0o755

// sectionHeaderPrefix is the line prefix that starts a TOML section header
// for drive sections. Used to detect section boundaries in line-based edits.
const sectionHeaderPrefix = `["`

// Drive type aliases for readability.
const (
	driveTypePersonal   = driveid.DriveTypePersonal
	driveTypeBusiness   = driveid.DriveTypeBusiness
	driveTypeSharePoint = driveid.DriveTypeSharePoint
	driveTypeShared     = driveid.DriveTypeShared
)

// configTemplate is the default config file content written on first login.
// All global settings are present as commented-out defaults so users can
// discover every option without reading docs. This template is written once
// and never regenerated — user modifications are preserved by subsequent
// text-level edits.
const configTemplate = `# onedrive-go configuration
# Docs: https://github.com/tonimelisma/onedrive-go

# ── Global settings ──
# Uncomment and modify to override defaults.

# Log file verbosity: debug, info, warn, error
# log_level = "info"

# Log file path (default: platform standard location)
# log_file = ""

# Check interval for sync --watch
# poll_interval = "5m"

# ── Drives ──
# Added automatically by 'login' and 'drive add'.
# Each section name is the canonical drive identifier.
# Filter settings (skip_dotfiles, skip_dirs, skip_files, etc.) are
# per-drive only — configure them inside each drive section below.
`

// driveSection generates the TOML text for a new drive section. The blank
// line before the header is intentional — it visually separates drive
// sections from each other and from the global settings.
func driveSection(canonicalID, syncDir string) string {
	return fmt.Sprintf("\n[%q]\nsync_dir = %q\n", canonicalID, syncDir)
}

// CreateConfigWithDrive creates a new config file from the default template
// and appends a drive section. Used on first login when no config file exists.
// The write is atomic (temp file + rename) and parent directories are created
// as needed.
func CreateConfigWithDrive(path string, canonicalID driveid.CanonicalID, syncDir string) error {
	slog.Info("creating config file with drive",
		"path", path,
		"canonical_id", canonicalID.String(),
		"sync_dir", syncDir,
	)

	content := configTemplate + driveSection(canonicalID.String(), syncDir)

	return atomicWriteFile(path, []byte(content))
}

// AppendDriveSection appends a new drive section at the end of an existing
// config file. Used by subsequent logins and `drive add`. The write is atomic
// to avoid partial writes on crash.
func AppendDriveSection(path string, canonicalID driveid.CanonicalID, syncDir string) error {
	slog.Info("appending drive section to config",
		"path", path,
		"canonical_id", canonicalID.String(),
		"sync_dir", syncDir,
	)

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
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

// SetDriveKey finds a drive section by canonical ID and sets a key-value pair.
// If the key already exists within the section, its line is replaced. If not
// found, the key is inserted on the line after the section header.
//
// Value formatting: booleans ("true"/"false") are written without quotes;
// all other values are written as quoted strings.
func SetDriveKey(path string, canonicalID driveid.CanonicalID, key, value string) error {
	slog.Info("setting drive key in config",
		"path", path,
		"canonical_id", canonicalID.String(),
		"key", key,
		"value", value,
	)

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	lines := strings.Split(string(data), "\n")

	headerLine, sectionStart := findSectionHeader(lines, canonicalID.String())
	if sectionStart < 0 {
		return fmt.Errorf("drive section %q not found in config", canonicalID.String())
	}

	formattedValue := formatTOMLValue(value)
	newLine := fmt.Sprintf("%s = %s", key, formattedValue)

	lines = setKeyInSection(lines, headerLine, sectionStart, key, newLine)

	return atomicWriteFile(path, []byte(strings.Join(lines, "\n")))
}

// DeleteDriveKey removes a single key from a drive section. Idempotent:
// returns nil if the key does not exist in the section. Used by `resume`
// to clear `paused` and `paused_until` keys.
func DeleteDriveKey(path string, canonicalID driveid.CanonicalID, key string) error {
	slog.Info("deleting drive key from config",
		"path", path,
		"canonical_id", canonicalID.String(),
		"key", key,
	)

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	lines := strings.Split(string(data), "\n")

	headerLine, sectionStart := findSectionHeader(lines, canonicalID.String())
	if sectionStart < 0 {
		return fmt.Errorf("drive section %q not found in config", canonicalID.String())
	}

	lines = deleteKeyInSection(lines, headerLine, sectionStart, key)

	return atomicWriteFile(path, []byte(strings.Join(lines, "\n")))
}

// DeleteDriveSection removes a drive section (header + all keys) from the
// config file. Also removes blank lines immediately preceding the section
// header for clean formatting. Used by `drive remove --purge` and
// `logout --purge`.
func DeleteDriveSection(path string, canonicalID driveid.CanonicalID) error {
	slog.Info("deleting drive section from config",
		"path", path,
		"canonical_id", canonicalID.String(),
	)

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	lines := strings.Split(string(data), "\n")

	headerLine, sectionStart := findSectionHeader(lines, canonicalID.String())
	if sectionStart < 0 {
		return fmt.Errorf("drive section %q not found in config", canonicalID.String())
	}

	sectionEnd := findSectionEnd(lines, sectionStart)

	// Remove preceding blank lines for clean formatting. Start from the
	// header line itself so the entire section (header + content) is deleted.
	blankStart := headerLine
	for blankStart > 0 && strings.TrimSpace(lines[blankStart-1]) == "" {
		blankStart--
	}

	lines = append(lines[:blankStart], lines[sectionEnd:]...)

	return atomicWriteFile(path, []byte(strings.Join(lines, "\n")))
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
	base := BaseSyncDir(cid, orgName)
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
func BaseSyncDir(cid driveid.CanonicalID, orgName string) string {
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

// findSectionHeader locates the line index of a drive section header.
// Returns the header line index and the section content start (header + 1).
// Returns -1 for both if the section is not found.
func findSectionHeader(lines []string, canonicalID string) (int, int) {
	header := fmt.Sprintf("[%q]", canonicalID)

	for i, line := range lines {
		if strings.TrimSpace(line) == header {
			return i, i + 1
		}
	}

	return -1, -1
}

// findSectionEnd returns the index of the first line after the section's
// own content. This excludes blank lines and comments that precede the
// next section header (those belong to the next section's preamble, not
// this section's content).
func findSectionEnd(lines []string, sectionStart int) int {
	nextHeader := len(lines)

	for i := sectionStart; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, sectionHeaderPrefix) {
			nextHeader = i

			break
		}
	}

	// Walk backwards from the next section header to skip blank lines and
	// comment lines that belong to the next section's preamble.
	end := nextHeader
	for end > sectionStart {
		trimmed := strings.TrimSpace(lines[end-1])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			end--

			continue
		}

		break
	}

	return end
}

// deleteKeyInSection removes a key line from a section if it exists.
// Returns the original slice unchanged if the key is not found.
func deleteKeyInSection(lines []string, headerLine, sectionStart int, key string) []string {
	sectionEnd := findSectionEnd(lines, sectionStart)
	keyPrefix := key + " "
	keyPrefixEq := key + "="

	for i := headerLine + 1; i < sectionEnd; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, keyPrefix) || strings.HasPrefix(trimmed, keyPrefixEq) {
			return append(lines[:i], lines[i+1:]...)
		}
	}

	return lines
}

// setKeyInSection either replaces an existing key line or inserts a new
// one after the section header.
func setKeyInSection(lines []string, headerLine, sectionStart int, key, newLine string) []string {
	sectionEnd := findSectionEnd(lines, sectionStart)
	keyPrefix := key + " "
	keyPrefixEq := key + "="

	// Search for existing key within the section.
	for i := headerLine + 1; i < sectionEnd; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, keyPrefix) || strings.HasPrefix(trimmed, keyPrefixEq) {
			lines[i] = newLine

			return lines
		}
	}

	// Key not found — insert after header.
	inserted := make([]string, 0, len(lines)+1)
	inserted = append(inserted, lines[:headerLine+1]...)
	inserted = append(inserted, newLine)
	inserted = append(inserted, lines[headerLine+1:]...)

	return inserted
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
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, configDirPermissions); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	f, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}

	tempPath := f.Name()

	// Clean up the temp file on any error path.
	succeeded := false
	defer func() {
		if !succeeded {
			os.Remove(tempPath)
		}
	}()

	if _, err := f.Write(data); err != nil {
		f.Close()

		return fmt.Errorf("writing temp file: %w", err)
	}

	// Flush data to disk before rename. Without fsync, a power loss after
	// rename could leave the file empty (rename is metadata-only on POSIX).
	if err := f.Sync(); err != nil {
		f.Close()

		return fmt.Errorf("syncing temp file: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	if err := os.Chmod(tempPath, configFilePermissions); err != nil {
		return fmt.Errorf("setting file permissions: %w", err)
	}

	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("renaming temp file: %w", err)
	}

	succeeded = true

	return nil
}
