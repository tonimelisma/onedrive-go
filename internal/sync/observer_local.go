package sync

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// ErrNosyncGuard is returned when a .nosync guard file is present in the
// sync root, indicating the sync directory may be unmounted or guarded.
var ErrNosyncGuard = errors.New("sync: .nosync guard file present (sync dir may be unmounted)")

// Constants for the local observer (satisfy mnd linter).
const (
	nosyncFileName         = ".nosync"
	nanosPerSecond         = 1_000_000_000
	maxComponentLength     = 255
	deviceNameWithDigitLen = 4 // COM0-COM9, LPT0-LPT9 have exactly 4 characters
)

// LocalObserver walks the local filesystem and produces []ChangeEvent by
// comparing each entry against the in-memory baseline. Stateless — syncRoot
// is a parameter of FullScan, allowing reuse across cycles.
type LocalObserver struct {
	baseline *Baseline
	logger   *slog.Logger
}

// NewLocalObserver creates a LocalObserver. The baseline must be loaded (from
// BaselineManager.Load); it is read-only during observation.
func NewLocalObserver(baseline *Baseline, logger *slog.Logger) *LocalObserver {
	return &LocalObserver{
		baseline: baseline,
		logger:   logger,
	}
}

// FullScan walks the sync root directory and returns change events for all
// local changes (creates, modifies, deletes) relative to the baseline.
func (o *LocalObserver) FullScan(ctx context.Context, syncRoot string) ([]ChangeEvent, error) {
	o.logger.Info("local observer starting full scan",
		slog.String("sync_root", syncRoot),
		slog.Int("baseline_entries", len(o.baseline.ByPath)),
	)

	// Guard: abort if .nosync file is present (sync dir may be unmounted).
	if _, err := os.Stat(filepath.Join(syncRoot, nosyncFileName)); err == nil {
		o.logger.Warn("nosync guard file detected, aborting scan",
			slog.String("sync_root", syncRoot))
		return nil, ErrNosyncGuard
	}

	var events []ChangeEvent
	observed := make(map[string]bool)

	walkFn := o.makeWalkFunc(ctx, syncRoot, observed, &events)
	if err := filepath.WalkDir(syncRoot, walkFn); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("sync: local scan canceled: %w", ctx.Err())
		}

		return nil, fmt.Errorf("sync: walking %s: %w", syncRoot, err)
	}

	deletions := o.detectDeletions(observed)
	events = append(events, deletions...)

	o.logger.Debug("deletion detection complete",
		slog.Int("deletions", len(deletions)),
		slog.Int("baseline_entries", len(o.baseline.ByPath)),
		slog.Int("observed", len(observed)),
	)

	o.logger.Info("local observer completed full scan",
		slog.Int("events", len(events)),
		slog.Int("observed", len(observed)),
	)

	return events, nil
}

// makeWalkFunc returns a WalkDirFunc that classifies filesystem entries
// against the baseline and appends change events.
func (o *LocalObserver) makeWalkFunc(
	ctx context.Context, syncRoot string, observed map[string]bool, events *[]ChangeEvent,
) fs.WalkDirFunc {
	return func(fsPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			o.logger.Warn("walk error", slog.String("path", fsPath), slog.String("error", walkErr.Error()))
			return skipEntry(d)
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Skip the sync root directory itself.
		if fsPath == syncRoot {
			return nil
		}

		relPath, err := filepath.Rel(syncRoot, fsPath)
		if err != nil {
			return fmt.Errorf("sync: computing relative path for %s: %w", fsPath, err)
		}

		// Normalize: forward slashes for cross-platform consistency + NFC Unicode.
		dbRelPath := nfcNormalize(filepath.ToSlash(relPath))
		name := nfcNormalize(d.Name())

		// Symlinks are never synced — skip silently.
		if d.Type()&fs.ModeSymlink != 0 {
			o.logger.Debug("skipping symlink", slog.String("path", dbRelPath))
			return skipEntry(d)
		}

		if isAlwaysExcluded(name) {
			o.logger.Debug("skipping excluded file", slog.String("name", name))
			return skipEntry(d)
		}

		if !isValidOneDriveName(name) {
			o.logger.Debug("skipping invalid OneDrive name", slog.String("name", name))
			return skipEntry(d)
		}

		return o.processEntry(fsPath, dbRelPath, name, d, observed, events)
	}
}

// processEntry reads file info, marks the path as observed, and classifies
// the local change against the baseline.
func (o *LocalObserver) processEntry(
	fsPath, dbRelPath, name string, d fs.DirEntry, observed map[string]bool, events *[]ChangeEvent,
) error {
	info, err := d.Info()
	if err != nil {
		// File disappeared between readdir and stat — skip and continue.
		o.logger.Warn("stat failed (file may have disappeared)",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))
		return nil
	}

	observed[dbRelPath] = true

	ev, err := o.classifyLocalChange(fsPath, dbRelPath, name, d, info)
	if err != nil {
		return err
	}

	if ev != nil {
		*events = append(*events, *ev)
	}

	return nil
}

// classifyLocalChange determines the change type for a single local entry
// by comparing it against the baseline.
func (o *LocalObserver) classifyLocalChange(
	fsPath, dbRelPath, name string, d fs.DirEntry, info fs.FileInfo,
) (*ChangeEvent, error) {
	existing := o.baseline.ByPath[dbRelPath]

	// No baseline entry — this is a new item.
	if existing == nil {
		return o.buildCreateEvent(fsPath, dbRelPath, name, d, info)
	}

	// Existing folder — OS-level mtime changes (e.g. adding a file) are noise;
	// the contained files generate their own events.
	if d.IsDir() {
		return nil, nil
	}

	return o.classifyFileChange(fsPath, dbRelPath, name, info, existing)
}

// buildCreateEvent constructs a ChangeCreate event for a new local entry.
func (o *LocalObserver) buildCreateEvent(
	fsPath, dbRelPath, name string, d fs.DirEntry, info fs.FileInfo,
) (*ChangeEvent, error) {
	ev := ChangeEvent{
		Source:   SourceLocal,
		Type:     ChangeCreate,
		Path:     dbRelPath,
		Name:     name,
		ItemType: itemTypeFromDirEntry(d),
		Size:     info.Size(),
		Mtime:    info.ModTime().UnixNano(),
	}

	// Compute hash for files (folders have no content hash).
	if !d.IsDir() {
		hash, err := computeQuickXorHash(fsPath)
		if err != nil {
			o.logger.Warn("hash computation failed, skipping file",
				slog.String("path", dbRelPath), slog.String("error", err.Error()))
			return nil, nil
		}

		ev.Hash = hash
	}

	return &ev, nil
}

// classifyFileChange compares a file against its baseline entry to detect
// content modifications. Always hashes for correctness (mtime optimization
// deferred to B-031 profiling).
func (o *LocalObserver) classifyFileChange(
	fsPath, dbRelPath, name string, info fs.FileInfo, base *BaselineEntry,
) (*ChangeEvent, error) {
	hash, err := computeQuickXorHash(fsPath)
	if err != nil {
		o.logger.Warn("hash computation failed, skipping file",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))
		return nil, nil
	}

	// Hash matches baseline — file is unchanged regardless of mtime.
	if hash == base.LocalHash {
		return nil, nil
	}

	return &ChangeEvent{
		Source:   SourceLocal,
		Type:     ChangeModify,
		Path:     dbRelPath,
		Name:     name,
		ItemType: ItemTypeFile,
		Size:     info.Size(),
		Hash:     hash,
		Mtime:    info.ModTime().UnixNano(),
	}, nil
}

// detectDeletions finds baseline entries that were not observed during the
// walk, emitting ChangeDelete events for each.
func (o *LocalObserver) detectDeletions(observed map[string]bool) []ChangeEvent {
	var events []ChangeEvent

	for path, entry := range o.baseline.ByPath {
		if path == "" {
			continue
		}

		if entry.ItemType == ItemTypeRoot {
			continue
		}

		if observed[path] {
			continue
		}

		events = append(events, ChangeEvent{
			Source:    SourceLocal,
			Type:      ChangeDelete,
			Path:      path,
			Name:      filepath.Base(path),
			ItemType:  entry.ItemType,
			IsDeleted: true,
		})
	}

	return events
}

// ---------------------------------------------------------------------------
// File hashing
// ---------------------------------------------------------------------------

// computeQuickXorHash computes the QuickXorHash of a file and returns
// the base64-encoded digest. Uses streaming I/O (constant memory).
func computeQuickXorHash(fsPath string) (string, error) {
	f, err := os.Open(fsPath)
	if err != nil {
		return "", fmt.Errorf("sync: opening %s for hashing: %w", fsPath, err)
	}
	defer f.Close()

	h := quickxorhash.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("sync: hashing %s: %w", fsPath, err)
	}

	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// ---------------------------------------------------------------------------
// Pure helper functions
// ---------------------------------------------------------------------------

// isAlwaysExcluded returns true for file patterns that must never be synced.
// These are S7 safety invariants: partial downloads, editor temporaries,
// and SQLite database files (which corrupt if synced mid-transaction).
func isAlwaysExcluded(name string) bool {
	lower := strings.ToLower(name)

	// Extension-based: partial downloads, editor temps, SQLite files.
	for _, ext := range alwaysExcludedSuffixes {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}

	// Prefix-based: editor backup files (~file) and LibreOffice locks (.~lock).
	if strings.HasPrefix(name, "~") || strings.HasPrefix(name, ".~") {
		return true
	}

	return false
}

// alwaysExcludedSuffixes lists file extensions that are unsafe to sync.
// SQLite files (.db, .db-wal, .db-shm) corrupt if synced mid-transaction.
var alwaysExcludedSuffixes = []string{
	".partial", ".tmp", ".swp", ".crdownload",
	".db-wal", ".db-shm",
	".db",
}

// isValidOneDriveName returns true if the name can be synced to OneDrive.
// Rejects reserved names, invalid characters, and structural constraints
// per the OneDrive API documentation.
func isValidOneDriveName(name string) bool {
	if name == "" {
		return false
	}

	if name[len(name)-1] == '.' || name[len(name)-1] == ' ' {
		return false
	}

	if name[0] == ' ' {
		return false
	}

	if len(name) > maxComponentLength {
		return false
	}

	return isValidNameContent(name)
}

// isValidNameContent checks the name for reserved patterns, invalid
// characters, and OneDrive-specific restrictions.
func isValidNameContent(name string) bool {
	lower := strings.ToLower(name)

	if isReservedDeviceName(lower) {
		return false
	}

	if isReservedPattern(name, lower) {
		return false
	}

	return !containsInvalidChars(name)
}

// isReservedDeviceName returns true for Windows reserved device names
// (case-insensitive): CON, PRN, AUX, NUL, COM0-COM9, LPT0-LPT9.
func isReservedDeviceName(lower string) bool {
	switch lower {
	case "con", "prn", "aux", "nul":
		return true
	}

	// COM0-COM9, LPT0-LPT9: exactly 4 characters, prefix + single digit.
	if len(lower) == deviceNameWithDigitLen &&
		(strings.HasPrefix(lower, "com") || strings.HasPrefix(lower, "lpt")) {
		digit := lower[3]
		return digit >= '0' && digit <= '9'
	}

	return false
}

// isReservedPattern returns true for OneDrive-specific reserved file patterns:
// .lock extension, desktop.ini, ~$ prefix (Office temp), _vti_ substring.
func isReservedPattern(name, lower string) bool {
	if strings.HasSuffix(lower, ".lock") {
		return true
	}

	if lower == "desktop.ini" {
		return true
	}

	if strings.HasPrefix(name, "~$") {
		return true
	}

	return strings.Contains(lower, "_vti_")
}

// containsInvalidChars returns true if the name contains characters
// forbidden by OneDrive: " * : < > ? / \ |
func containsInvalidChars(name string) bool {
	for _, c := range name {
		switch c {
		case '"', '*', ':', '<', '>', '?', '/', '\\', '|':
			return true
		}
	}

	return false
}

// itemTypeFromDirEntry maps a DirEntry to the sync engine's ItemType.
func itemTypeFromDirEntry(d fs.DirEntry) ItemType {
	if d.IsDir() {
		return ItemTypeFolder
	}

	return ItemTypeFile
}

// skipEntry returns filepath.SkipDir for directories (to skip the subtree)
// or nil for files (to continue the walk with the next entry).
func skipEntry(d fs.DirEntry) error {
	if d != nil && d.IsDir() {
		return filepath.SkipDir
	}

	return nil
}

// truncateToSeconds truncates nanosecond-precision time to second precision.
// For future mtime optimization where remote timestamps have only second
// precision (deferred to B-031).
func truncateToSeconds(nanos int64) int64 {
	return (nanos / nanosPerSecond) * nanosPerSecond
}
