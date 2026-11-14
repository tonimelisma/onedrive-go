package config

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/fsroot"
)

// configFilePermissions is the mode for config files: owner read/write only.
// The config names accounts in its `personal:<email>` section keys, and other
// local users reading it is an actor in the threat model.
const configFilePermissions = 0o600

// configDirPermissions is the mode of a folder the config writers create: the
// config folder, the data folder the catalog lives in, and any folder above
// them that is missing. It is owner-only, as the XDG base directory rule asks
// of a directory created where none existed. A folder that already exists
// keeps its mode.
const configDirPermissions = 0o700

// Drive type aliases for readability.
const (
	driveTypePersonal   = driveid.DriveTypePersonal
	driveTypeBusiness   = driveid.DriveTypeBusiness
	driveTypeSharePoint = driveid.DriveTypeSharePoint
	driveTypeShared     = driveid.DriveTypeShared
)

// Drive-section keys that drive enrollment writes.
const (
	syncDirKey     = "sync_dir"
	displayNameKey = "display_name"
)

// defaultConfigTemplate returns the first-login config content. All global
// settings are present as commented-out defaults so users can discover every
// option without reading docs. The template uses the live default constants so
// docs and config creation do not drift when defaults change.
func defaultConfigTemplate() string {
	return fmt.Sprintf(`# onedrive-go configuration
# Docs: https://github.com/tonimelisma/onedrive-go/blob/main/docs/configuration.md

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
#
# Per-drive options go in a drive's section, for example:
#   ignore_junk_files = false     # sync junk too; by default editor/OS temp files
#                                 # (*.tmp, *.swp, *~, ~$*, .DS_Store, Thumbs.db,
#                                 # SQLite -wal/-shm/-journal) are skipped
#   ignore_dotfiles = true        # skip names starting with "."
#   ignored_dirs = ["node_modules"]
#   ignored_paths = ["*.iso"]
#   included_dirs = ["Documents"] # sync only these folders
#   follow_symlinks = true
#   cross_filesystems = true      # also sync disks mounted inside the folder
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

// driveSection generates the TOML text for a new drive section holding keys,
// in the order given. The blank line before the header is intentional — it
// visually separates drive sections from each other and from the global
// settings.
func driveSection(canonicalID string, keys []driveKey) string {
	var b strings.Builder
	b.WriteString("\n" + renderSectionHeaderLine(canonicalID) + "\n")
	for _, key := range keys {
		b.WriteString(renderKeyValueLine(key.name, key.formattedValue, "") + "\n")
	}

	return b.String()
}

// AppendDriveSection appends a new drive section to a config file, under the
// config lock. If the file does not exist, it is created from the default
// template first. A section that is already there is refused. Login and
// `drive add` enroll drives through AddDrive instead; this is the plain
// append that tests use to set up a configured drive.
func AppendDriveSection(path string, canonicalID driveid.CanonicalID, syncDir string) error {
	return withConfigMutationLock(path, true, func() error {
		syncDirValue, err := newDriveKey(syncDirKey, syncDir)
		if err != nil {
			return err
		}

		data, exists, err := readManagedFileIfExists(defaultConfigIO(), path)
		if err != nil {
			return fmt.Errorf("reading config file: %w", err)
		}

		content, err := withDriveSectionAppended(data, exists, canonicalID, []driveKey{syncDirValue})
		if err != nil {
			return err
		}

		return atomicWriteFile(path, content)
	})
}

// withDriveSectionAppended returns config text with a new drive section
// appended: the default template comes first when the file does not exist
// yet, and a newline when the existing text does not end with one, so the
// header starts its own line. A section that is already there is refused: a
// second table with the same name makes the whole file unloadable.
func withDriveSectionAppended(
	data []byte,
	fileExists bool,
	canonicalID driveid.CanonicalID,
	keys []driveKey,
) ([]byte, error) {
	section := driveSection(canonicalID.String(), keys)
	if !fileExists {
		return []byte(defaultConfigTemplate() + section), nil
	}

	if _, found := findSectionByName(parseLines(string(data)), canonicalID.String()); found {
		return nil, fmt.Errorf("drive section %q is already in the config", canonicalID.String())
	}

	content := string(data)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	return []byte(content + section), nil
}

// driveKey is one key of a drive section and its value, already rendered as
// TOML by newDriveKey.
type driveKey struct {
	name           string
	formattedValue string
}

// newDriveKey renders value for a drive-section key by the kind of value the
// schema (newDriveKeyKinds) says the key holds. A bool key takes "true" or
// "false", written bare; a string key is always written as a TOML string, so
// a display name "true" stays a name. A list key cannot be given one value,
// and a key the schema does not have is refused: the strict load would refuse
// the file it produced.
func newDriveKey(name, value string) (driveKey, error) {
	kind, known := newDriveKeyKinds()[name]
	if !known {
		return driveKey{}, fmt.Errorf("drive key %q is not a drive setting", name)
	}

	switch kind {
	case driveKeyBool:
		if value != "true" && value != "false" {
			return driveKey{}, fmt.Errorf("drive key %q takes true or false, not %q", name, value)
		}

		return driveKey{name: name, formattedValue: value}, nil
	case driveKeyString:
		return driveKey{name: name, formattedValue: tomlString(value)}, nil
	case driveKeyStringList:
		return driveKey{}, fmt.Errorf("drive key %q holds a list; it cannot be set to one value", name)
	default:
		return driveKey{}, fmt.Errorf("drive key %q has a value kind the writer does not know", name)
	}
}

// withDriveKeysSet returns config text with each key set in the drive's
// section: an existing line is replaced, keeping its inline comment, and a
// missing one is inserted after the header, in the order given.
func withDriveKeysSet(data []byte, canonicalID driveid.CanonicalID, keys []driveKey) ([]byte, error) {
	lines := parseLines(string(data))

	headerIdx, found := findSectionByName(lines, canonicalID.String())
	if !found {
		return nil, fmt.Errorf("drive section %q not found in config", canonicalID.String())
	}

	insertAt := headerIdx + 1
	for _, key := range keys {
		contentStart, contentEnd := sectionContentRange(lines, headerIdx)
		if idx, keyFound := findKeyInRange(lines, contentStart, contentEnd, key.name); keyFound {
			lines[idx].raw = renderKeyValueLine(key.name, key.formattedValue, lines[idx].inlineComment)
			continue
		}

		lines = slices.Insert(lines, insertAt, parseLine(renderKeyValueLine(key.name, key.formattedValue, "")))
		insertAt++
	}

	return []byte(renderLines(lines)), nil
}

// SetDriveKey finds a drive section by canonical ID and sets a key-value pair.
// If the key already exists within the section, its line is replaced (preserving
// any inline comment). If not found, the key is inserted after the section header.
//
// The value is written as the kind the key holds (newDriveKey): a bool key
// refuses anything but "true" and "false", and a string key is always quoted.
// A refused value leaves the file as it was.
func SetDriveKey(path string, canonicalID driveid.CanonicalID, key, value string) error {
	return defaultConfigLocker().setDriveKey(path, canonicalID, key, value)
}

func setDriveKeySnapshot(path string, canonicalID driveid.CanonicalID, key, value string) error {
	setting, err := newDriveKey(key, value)
	if err != nil {
		return err
	}

	data, err := readManagedFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	content, err := withDriveKeysSet(data, canonicalID, []driveKey{setting})
	if err != nil {
		return err
	}

	return atomicWriteFile(path, content)
}

// DeleteDriveKey removes a single key from a drive section. Idempotent:
// returns nil if the key does not exist in the section. Used by `resume`
// to clear `paused` and `paused_until` keys.
func DeleteDriveKey(path string, canonicalID driveid.CanonicalID, key string) error {
	return withConfigMutationLock(path, false, func() error {
		return deleteDriveKeySnapshot(path, canonicalID, key)
	})
}

func deleteDriveKeySnapshot(path string, canonicalID driveid.CanonicalID, key string) error {
	data, err := readManagedFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	lines, sectionFound := deleteDriveKeyFromSnapshot(data, canonicalID, key)
	if !sectionFound {
		return fmt.Errorf("drive section %q not found in config", canonicalID.String())
	}

	return atomicWriteFile(path, []byte(renderLines(lines)))
}

// DeleteDriveSection removes a drive section (header + all keys) from the
// config file. Also removes blank lines immediately preceding the section
// header for clean formatting. Used by `drive remove --purge` and
// `logout --purge`.
func DeleteDriveSection(path string, canonicalID driveid.CanonicalID) error {
	return withConfigMutationLock(path, false, func() error {
		return deleteDriveSectionSnapshot(path, canonicalID)
	})
}

func deleteDriveSectionSnapshot(path string, canonicalID driveid.CanonicalID) error {
	data, err := readManagedFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	lines, deleted := deleteDriveSectionFromSnapshot(data, canonicalID)
	if !deleted {
		return fmt.Errorf("drive section %q not found in config", canonicalID.String())
	}

	return atomicWriteFile(path, []byte(renderLines(lines)))
}

// driveSectionCollisionError is a section rename refused because a section
// under the new name exists beside the one under the old name. The account
// email change reads both names from it to say which section to remove.
type driveSectionCollisionError struct {
	from string
	to   string
}

func (e *driveSectionCollisionError) Error() string {
	return fmt.Sprintf("config section rename collision: %q already exists beside %q", e.to, e.from)
}

// RenameDriveSections renames one or more drive section headers in place while
// preserving their contents, comments, and relative order. A target section
// that already exists is treated as a collision when the source section also
// exists (*driveSectionCollisionError), because the config would otherwise
// end up with two owners for the same canonical ID; the file is then left as
// it is. A source section that does not exist is skipped, so a rename that
// already happened is done.
func RenameDriveSections(path string, renames map[driveid.CanonicalID]driveid.CanonicalID) error {
	if len(renames) == 0 {
		return nil
	}

	return withConfigMutationLock(path, false, func() error {
		return renameDriveSectionsSnapshot(path, renames)
	})
}

func renameDriveSectionsSnapshot(path string, renames map[driveid.CanonicalID]driveid.CanonicalID) error {
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

	renamed := false
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
			return &driveSectionCollisionError{from: fromName, to: toName}
		default:
			lines[fromIdx].sectionName = toName
			lines[fromIdx].raw = renderSectionHeaderLine(toName)
			delete(headerIndex, fromName)
			headerIndex[toName] = fromIdx
			renamed = true
		}
	}
	if !renamed {
		return nil
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
	base := baseSyncDir(cid, orgName, displayName)
	if base == "" {
		return ""
	}

	if !containsExpanded(existingDirs, base) {
		return base
	}

	// Level 1: disambiguate with display name (friendly).
	if displayName != "" {
		withName := base + " - " + sanitizePathComponent(displayName)
		if !containsExpanded(existingDirs, withName) {
			return withName
		}
	}

	// Level 2: disambiguate with email (guaranteed unique).
	return base + " - " + cid.Email()
}

// baseSyncDir returns the base sync directory name for a drive type, without
// collision detection; otherSyncDirs uses it for drives whose section leaves
// sync_dir to the default.
//
// The displayName parameter is only used for shared drives (to create
// per-drive subdirectories under ~/OneDrive-Shared/). Personal, business,
// and SharePoint drives ignore it.
func baseSyncDir(cid driveid.CanonicalID, orgName, displayName string) string {
	switch cid.DriveType() {
	case driveTypePersonal:
		return "~/OneDrive"
	case driveTypeBusiness:
		if orgName != "" {
			return "~/OneDrive - " + sanitizePathComponent(orgName)
		}

		return "~/OneDrive - Business"
	case driveTypeSharePoint:
		site, lib := cid.Site(), cid.Library()
		if site != "" && lib != "" {
			return fmt.Sprintf("~/SharePoint - %s - %s",
				sanitizePathComponent(site), sanitizePathComponent(lib))
		}

		return "~/SharePoint"
	case driveTypeShared:
		if displayName != "" {
			return "~/OneDrive-Shared/" + sanitizePathComponent(displayName)
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

// sanitizePathComponent replaces filesystem-unsafe characters with "-".
func sanitizePathComponent(s string) string {
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

func deleteDriveKeyFromSnapshot(
	data []byte,
	canonicalID driveid.CanonicalID,
	key string,
) ([]parsedLine, bool) {
	lines := parseLines(string(data))

	headerIdx, found := findSectionByName(lines, canonicalID.String())
	if !found {
		return lines, false
	}

	contentStart, contentEnd := sectionContentRange(lines, headerIdx)
	if idx, keyFound := findKeyInRange(lines, contentStart, contentEnd, key); keyFound {
		lines = append(lines[:idx], lines[idx+1:]...)
	}

	return lines, true
}

func deleteDriveSectionFromSnapshot(
	data []byte,
	canonicalID driveid.CanonicalID,
) ([]parsedLine, bool) {
	lines := parseLines(string(data))

	headerIdx, found := findSectionByName(lines, canonicalID.String())
	if !found {
		return lines, false
	}

	_, contentEnd := sectionContentRange(lines, headerIdx)

	// Remove preceding blank lines for clean formatting.
	blankStart := headerIdx
	for blankStart > 0 && lines[blankStart-1].kind == lineBlank {
		blankStart--
	}

	return append(lines[:blankStart], lines[contentEnd:]...), true
}

// atomicWriteFile writes data to a temporary file in the same directory as
// path, then renames it to the target path. This prevents partial writes
// from corrupting a managed file (config, catalog) on crash. AtomicWrite
// creates missing parent directories with configDirPermissions. Files are
// created with configFilePermissions (0600).
func atomicWriteFile(path string, data []byte) (err error) {
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return fmt.Errorf("opening the directory of %s: %w", path, err)
	}

	if err := root.AtomicWrite(name, data, configFilePermissions, configDirPermissions, ".config-*.tmp"); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}
