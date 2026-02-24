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
	"time"

	"github.com/fsnotify/fsnotify"

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
	safetyScanInterval     = 5 * time.Minute
)

// FsWatcher abstracts filesystem event monitoring. Satisfied by
// *fsnotify.Watcher; tests inject a mock implementation.
type FsWatcher interface {
	Add(name string) error
	Remove(name string) error
	Close() error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
}

// fsnotifyWrapper adapts *fsnotify.Watcher to the FsWatcher interface.
// fsnotify exposes Events and Errors as public fields, not methods.
type fsnotifyWrapper struct {
	w *fsnotify.Watcher
}

func (fw *fsnotifyWrapper) Add(name string) error         { return fw.w.Add(name) }
func (fw *fsnotifyWrapper) Remove(name string) error      { return fw.w.Remove(name) }
func (fw *fsnotifyWrapper) Close() error                  { return fw.w.Close() }
func (fw *fsnotifyWrapper) Events() <-chan fsnotify.Event { return fw.w.Events }
func (fw *fsnotifyWrapper) Errors() <-chan error          { return fw.w.Errors }

// LocalObserver walks the local filesystem and produces []ChangeEvent by
// comparing each entry against the in-memory baseline. Stateless — syncRoot
// is a parameter of FullScan, allowing reuse across cycles.
type LocalObserver struct {
	baseline       *Baseline
	logger         *slog.Logger
	watcherFactory func() (FsWatcher, error)
	sleepFunc      func(ctx context.Context, d time.Duration) error
}

// NewLocalObserver creates a LocalObserver. The baseline must be loaded (from
// BaselineManager.Load); it is read-only during observation.
func NewLocalObserver(baseline *Baseline, logger *slog.Logger) *LocalObserver {
	return &LocalObserver{
		baseline:  baseline,
		logger:    logger,
		sleepFunc: timeSleep,
		watcherFactory: func() (FsWatcher, error) {
			w, err := fsnotify.NewWatcher()
			if err != nil {
				return nil, err
			}
			return &fsnotifyWrapper{w: w}, nil
		},
	}
}

// FullScan walks the sync root directory and returns change events for all
// local changes (creates, modifies, deletes) relative to the baseline.
func (o *LocalObserver) FullScan(ctx context.Context, syncRoot string) ([]ChangeEvent, error) {
	o.logger.Info("local observer starting full scan",
		slog.String("sync_root", syncRoot),
		slog.Int("baseline_entries", o.baseline.Len()),
	)

	// Guard: abort if .nosync file is present (sync dir may be unmounted).
	if _, err := os.Stat(filepath.Join(syncRoot, nosyncFileName)); err == nil {
		o.logger.Warn("nosync guard file detected, aborting scan",
			slog.String("sync_root", syncRoot))
		return nil, ErrNosyncGuard
	}

	var events []ChangeEvent
	observed := make(map[string]bool)
	scanStartNano := time.Now().UnixNano()

	walkFn := o.makeWalkFunc(ctx, syncRoot, observed, &events, scanStartNano)
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
		slog.Int("baseline_entries", o.baseline.Len()),
		slog.Int("observed", len(observed)),
	)

	o.logger.Info("local observer completed full scan",
		slog.Int("events", len(events)),
		slog.Int("observed", len(observed)),
	)

	return events, nil
}

// Watch monitors the local filesystem for changes using fsnotify and sends
// events to the provided channel. It blocks until the context is canceled,
// returning nil. A periodic safety scan (every 5 minutes) catches any events
// that fsnotify may miss (e.g., during brief watcher gaps or platform edge
// cases). Returns ErrNosyncGuard if the .nosync guard file is present.
func (o *LocalObserver) Watch(ctx context.Context, syncRoot string, events chan<- ChangeEvent) error {
	o.logger.Info("local observer starting watch",
		slog.String("sync_root", syncRoot),
	)

	// Guard: abort if .nosync file is present.
	if _, err := os.Stat(filepath.Join(syncRoot, nosyncFileName)); err == nil {
		o.logger.Warn("nosync guard file detected, aborting watch",
			slog.String("sync_root", syncRoot))

		return ErrNosyncGuard
	}

	watcher, err := o.watcherFactory()
	if err != nil {
		return fmt.Errorf("sync: creating filesystem watcher: %w", err)
	}
	defer watcher.Close()

	// Walk the sync root to add watches on all existing directories.
	if walkErr := o.addWatchesRecursive(watcher, syncRoot); walkErr != nil {
		return fmt.Errorf("sync: adding initial watches: %w", walkErr)
	}

	return o.watchLoop(ctx, watcher, syncRoot, events)
}

// addWatchesRecursive walks the sync root and adds a watch on every directory.
func (o *LocalObserver) addWatchesRecursive(watcher FsWatcher, syncRoot string) error {
	return filepath.WalkDir(syncRoot, func(fsPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			o.logger.Warn("walk error during watch setup",
				slog.String("path", fsPath), slog.String("error", walkErr.Error()))

			return skipEntry(d)
		}

		if !d.IsDir() {
			return nil
		}

		name := d.Name()
		if fsPath != syncRoot && (isAlwaysExcluded(name) || !isValidOneDriveName(name)) {
			return filepath.SkipDir
		}

		if addErr := watcher.Add(fsPath); addErr != nil {
			o.logger.Warn("failed to add watch",
				slog.String("path", fsPath), slog.String("error", addErr.Error()))
		}

		return nil
	})
}

// watchLoop is the main select loop for Watch(). It processes fsnotify events,
// watcher errors, safety scan ticks, and context cancellation.
func (o *LocalObserver) watchLoop(
	ctx context.Context, watcher FsWatcher, syncRoot string, events chan<- ChangeEvent,
) error {
	safetyTicker := time.NewTicker(safetyScanInterval)
	defer safetyTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case fsEvent, ok := <-watcher.Events():
			if !ok {
				return nil
			}

			o.handleFsEvent(ctx, fsEvent, watcher, syncRoot, events)

		case watchErr, ok := <-watcher.Errors():
			if !ok {
				return nil
			}

			o.logger.Warn("filesystem watcher error",
				slog.String("error", watchErr.Error()))

		case <-safetyTicker.C:
			o.runSafetyScan(ctx, syncRoot, events)
		}
	}
}

// handleFsEvent processes a single fsnotify event and sends the appropriate
// ChangeEvent to the output channel.
func (o *LocalObserver) handleFsEvent(
	ctx context.Context, fsEvent fsnotify.Event, watcher FsWatcher,
	syncRoot string, events chan<- ChangeEvent,
) {
	// Ignore chmod events — mode changes are not synced.
	if fsEvent.Has(fsnotify.Chmod) && !fsEvent.Has(fsnotify.Create) && !fsEvent.Has(fsnotify.Write) {
		return
	}

	relPath, err := filepath.Rel(syncRoot, fsEvent.Name)
	if err != nil {
		o.logger.Warn("failed to compute relative path",
			slog.String("path", fsEvent.Name), slog.String("error", err.Error()))

		return
	}

	dbRelPath := nfcNormalize(filepath.ToSlash(relPath))
	name := nfcNormalize(filepath.Base(fsEvent.Name))

	if isAlwaysExcluded(name) || !isValidOneDriveName(name) {
		return
	}

	switch {
	case fsEvent.Has(fsnotify.Create):
		o.handleCreate(ctx, fsEvent.Name, dbRelPath, name, watcher, events)

	case fsEvent.Has(fsnotify.Write):
		o.handleWrite(ctx, fsEvent.Name, dbRelPath, name, events)

	case fsEvent.Has(fsnotify.Remove) || fsEvent.Has(fsnotify.Rename):
		o.handleDelete(ctx, dbRelPath, name, events)
	}
}

// handleCreate processes a Create event: stat, hash (files), add watch (dirs).
func (o *LocalObserver) handleCreate(
	ctx context.Context, fsPath, dbRelPath, name string,
	watcher FsWatcher, events chan<- ChangeEvent,
) {
	info, err := os.Stat(fsPath)
	if err != nil {
		// File may have been removed immediately after creation.
		o.logger.Debug("stat failed for created path",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))

		return
	}

	ev := ChangeEvent{
		Source: SourceLocal,
		Type:   ChangeCreate,
		Path:   dbRelPath,
		Name:   name,
		Size:   info.Size(),
		Mtime:  info.ModTime().UnixNano(),
	}

	if info.IsDir() {
		ev.ItemType = ItemTypeFolder

		if addErr := watcher.Add(fsPath); addErr != nil {
			o.logger.Warn("failed to add watch on new directory",
				slog.String("path", dbRelPath), slog.String("error", addErr.Error()))
		}
	} else {
		ev.ItemType = ItemTypeFile

		hash, hashErr := computeQuickXorHash(fsPath)
		if hashErr != nil {
			o.logger.Warn("hash failed for new file",
				slog.String("path", dbRelPath), slog.String("error", hashErr.Error()))

			return
		}

		ev.Hash = hash
	}

	select {
	case events <- ev:
	case <-ctx.Done():
	}
}

// handleWrite processes a Write event: classify against baseline.
func (o *LocalObserver) handleWrite(
	ctx context.Context, fsPath, dbRelPath, name string, events chan<- ChangeEvent,
) {
	info, err := os.Stat(fsPath)
	if err != nil {
		o.logger.Debug("stat failed for modified path",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))

		return
	}

	// Ignore directory write events — folder mtime changes are noise.
	if info.IsDir() {
		return
	}

	hash, err := computeQuickXorHash(fsPath)
	if err != nil {
		o.logger.Warn("hash failed for modified file",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))

		return
	}

	// Check baseline — if hash matches, the write was a no-op.
	if existing, ok := o.baseline.GetByPath(dbRelPath); ok && existing.LocalHash == hash {
		return
	}

	ev := ChangeEvent{
		Source:   SourceLocal,
		Type:     ChangeModify,
		Path:     dbRelPath,
		Name:     name,
		ItemType: ItemTypeFile,
		Size:     info.Size(),
		Hash:     hash,
		Mtime:    info.ModTime().UnixNano(),
	}

	select {
	case events <- ev:
	case <-ctx.Done():
	}
}

// handleDelete processes a Remove/Rename event.
func (o *LocalObserver) handleDelete(
	ctx context.Context, dbRelPath, name string, events chan<- ChangeEvent,
) {
	itemType := ItemTypeFile

	if existing, ok := o.baseline.GetByPath(dbRelPath); ok {
		itemType = existing.ItemType
	}

	ev := ChangeEvent{
		Source:    SourceLocal,
		Type:      ChangeDelete,
		Path:      dbRelPath,
		Name:      name,
		ItemType:  itemType,
		IsDeleted: true,
	}

	select {
	case events <- ev:
	case <-ctx.Done():
	}
}

// runSafetyScan performs a full filesystem scan as a safety net, sending any
// detected changes to the events channel. This catches events that fsnotify
// may have missed.
func (o *LocalObserver) runSafetyScan(ctx context.Context, syncRoot string, events chan<- ChangeEvent) {
	o.logger.Debug("running safety scan")

	scanEvents, err := o.FullScan(ctx, syncRoot)
	if err != nil {
		o.logger.Warn("safety scan failed", slog.String("error", err.Error()))
		return
	}

	for i := range scanEvents {
		select {
		case events <- scanEvents[i]:
		case <-ctx.Done():
			return
		}
	}

	o.logger.Debug("safety scan complete", slog.Int("events", len(scanEvents)))
}

// makeWalkFunc returns a WalkDirFunc that classifies filesystem entries
// against the baseline and appends change events.
func (o *LocalObserver) makeWalkFunc(
	ctx context.Context, syncRoot string, observed map[string]bool, events *[]ChangeEvent,
	scanStartNano int64,
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

		return o.processEntry(fsPath, dbRelPath, name, d, observed, events, scanStartNano)
	}
}

// processEntry reads file info, marks the path as observed, and classifies
// the local change against the baseline.
func (o *LocalObserver) processEntry(
	fsPath, dbRelPath, name string, d fs.DirEntry, observed map[string]bool, events *[]ChangeEvent,
	scanStartNano int64,
) error {
	info, err := d.Info()
	if err != nil {
		// File disappeared between readdir and stat — skip and continue.
		o.logger.Warn("stat failed (file may have disappeared)",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))
		return nil
	}

	observed[dbRelPath] = true

	ev, err := o.classifyLocalChange(fsPath, dbRelPath, name, d, info, scanStartNano)
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
	scanStartNano int64,
) (*ChangeEvent, error) {
	existing, _ := o.baseline.GetByPath(dbRelPath)

	// No baseline entry — this is a new item.
	if existing == nil {
		return o.buildCreateEvent(fsPath, dbRelPath, name, d, info)
	}

	// Existing folder — OS-level mtime changes (e.g. adding a file) are noise;
	// the contained files generate their own events.
	if d.IsDir() {
		return nil, nil
	}

	return o.classifyFileChange(fsPath, dbRelPath, name, info, existing, scanStartNano)
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
// content modifications. Uses mtime+size as a fast path — only computes
// the content hash when metadata suggests a change. This is the industry
// standard (rsync, rclone, Syncthing, Git all use this pattern). Includes
// a racily-clean guard: files whose mtime is within 1 second of scan
// start are always hashed, because they may have been modified in the
// same clock tick as the last sync (Git's "racily clean" problem).
func (o *LocalObserver) classifyFileChange(
	fsPath, dbRelPath, name string, info fs.FileInfo, base *BaselineEntry,
	scanStartNano int64,
) (*ChangeEvent, error) {
	currentMtime := info.ModTime().UnixNano()
	currentSize := info.Size()

	// Fast path: skip hashing when size and mtime both match the baseline.
	if currentSize == base.Size && currentMtime == base.Mtime {
		// Racily-clean guard: if the file's mtime is within 1 second of
		// scan start, the file may have been modified in the same clock
		// tick as the last sync. Force a hash check to be safe.
		if scanStartNano-currentMtime >= nanosPerSecond {
			o.logger.Debug("fast path: mtime+size match, skipping hash",
				slog.String("path", dbRelPath))

			return nil, nil //nolint:nilnil
		}

		o.logger.Debug("racily clean file, forcing hash check",
			slog.String("path", dbRelPath))
	}

	// Slow path: metadata differs (or racily clean) — compute hash.
	hash, err := computeQuickXorHash(fsPath)
	if err != nil {
		o.logger.Warn("hash computation failed, skipping file",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))

		return nil, nil //nolint:nilnil
	}

	// Hash matches baseline — file is unchanged despite metadata difference.
	if hash == base.LocalHash {
		return nil, nil //nolint:nilnil
	}

	return &ChangeEvent{
		Source:   SourceLocal,
		Type:     ChangeModify,
		Path:     dbRelPath,
		Name:     name,
		ItemType: ItemTypeFile,
		Size:     currentSize,
		Hash:     hash,
		Mtime:    currentMtime,
	}, nil
}

// detectDeletions finds baseline entries that were not observed during the
// walk, emitting ChangeDelete events for each.
func (o *LocalObserver) detectDeletions(observed map[string]bool) []ChangeEvent {
	var events []ChangeEvent

	o.baseline.ForEachPath(func(path string, entry *BaselineEntry) {
		if path == "" {
			return
		}

		if entry.ItemType == ItemTypeRoot {
			return
		}

		if observed[path] {
			return
		}

		events = append(events, ChangeEvent{
			Source:    SourceLocal,
			Type:      ChangeDelete,
			Path:      path,
			Name:      filepath.Base(path),
			ItemType:  entry.ItemType,
			Size:      entry.Size,
			Mtime:     entry.Mtime,
			IsDeleted: true,
		})
	})

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
// Available for cross-platform mtime comparison where remote timestamps
// have coarser granularity (e.g., 1-second resolution on some backends).
func truncateToSeconds(nanos int64) int64 {
	return (nanos / nanosPerSecond) * nanosPerSecond
}
