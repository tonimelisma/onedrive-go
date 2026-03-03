package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

// ErrSyncRootMissing is returned when the sync root directory does not exist
// or is not a directory. Callers can match with errors.Is.
var ErrSyncRootMissing = errors.New("sync: sync root directory does not exist")

// ErrNosyncGuard is returned when a .nosync guard file is present in the
// sync root, indicating the sync directory may be unmounted or guarded.
var ErrNosyncGuard = errors.New("sync: .nosync guard file present (sync dir may be unmounted)")

// Constants for the local scanner.
const (
	nosyncFileName         = ".nosync"
	nanosPerSecond         = 1_000_000_000
	maxComponentLength     = 255
	deviceNameWithDigitLen = 4 // COM0-COM9, LPT0-LPT9 have exactly 4 characters
)

// defaultCheckWorkers is the default parallel hash goroutine limit when
// checkWorkers is zero (not configured).
const defaultCheckWorkers = 4

// hashJob describes a file that needs hashing during FullScan phase 2.
type hashJob struct {
	fsPath    string
	dbRelPath string
	name      string
	size      int64
	mtime     int64
	isNew     bool // true for creates, false for modifies
}

// resolveCheckWorkers returns the effective check worker count.
func (o *LocalObserver) resolveCheckWorkers() int {
	if o.checkWorkers > 0 {
		return o.checkWorkers
	}

	return defaultCheckWorkers
}

// FullScan walks the sync root directory and returns change events for all
// local changes (creates, modifies, deletes) relative to the baseline.
//
// Three-phase design:
//  1. Walk (sequential): collect observed map, emit folder creates, classify
//     files that need hashing into a hashJob slice.
//  2. Hash (parallel): errgroup.SetLimit(checkWorkers) hashes files concurrently.
//  3. Deletion detection (sequential): compare observed vs baseline.
func (o *LocalObserver) FullScan(ctx context.Context, syncRoot string) ([]ChangeEvent, error) {
	o.logger.Info("local observer starting full scan",
		slog.String("sync_root", syncRoot),
		slog.Int("baseline_entries", o.baseline.Len()),
	)

	// Guard: abort if the sync root directory does not exist. Without this,
	// WalkDir silently succeeds with zero events (walkFn's skipEntry returns
	// filepath.SkipDir for the root error, so WalkDir returns nil).
	if !syncRootExists(syncRoot) {
		o.logger.Warn("sync root missing, aborting scan",
			slog.String("sync_root", syncRoot))
		return nil, ErrSyncRootMissing
	}

	// Guard: abort if .nosync file is present (sync dir may be unmounted).
	if _, err := os.Stat(filepath.Join(syncRoot, nosyncFileName)); err == nil {
		o.logger.Warn("nosync guard file detected, aborting scan",
			slog.String("sync_root", syncRoot))
		return nil, ErrNosyncGuard
	}

	// Phase 1: Walk — collect observed paths, folder events, and hash jobs.
	var events []ChangeEvent
	var jobs []hashJob
	var skippedEntries atomic.Int64
	observed := make(map[string]bool)
	scanStartNano := time.Now().UnixNano()

	walkFn := o.makeWalkFunc(ctx, syncRoot, observed, &events, &jobs, &skippedEntries, scanStartNano)
	if err := filepath.WalkDir(syncRoot, walkFn); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("sync: local scan canceled: %w", ctx.Err())
		}

		return nil, fmt.Errorf("sync: walking %s: %w", syncRoot, err)
	}

	if n := skippedEntries.Load(); n > 0 {
		o.logger.Warn("full scan: skipped entries due to walk errors",
			slog.Int64("count", n),
			slog.String("sync_root", syncRoot))
	}

	// Phase 2: Hash — parallel file hashing.
	if len(jobs) > 0 {
		hashEvents, err := o.hashPhase(ctx, jobs)
		if err != nil {
			return nil, err
		}

		events = append(events, hashEvents...)
	}

	// Phase 3: Deletion detection.
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
		slog.Int("hashed", len(jobs)),
	)

	if len(events) > 0 {
		o.recordActivity()
	}

	return events, nil
}

// hashPhase runs hash jobs in parallel using errgroup with checkWorkers limit.
// Returns the resulting change events (creates and modifies with hashes).
func (o *LocalObserver) hashPhase(ctx context.Context, jobs []hashJob) ([]ChangeEvent, error) {
	workers := o.resolveCheckWorkers()

	o.logger.Debug("starting parallel hash phase",
		slog.Int("jobs", len(jobs)),
		slog.Int("workers", workers),
	)

	var mu stdsync.Mutex
	var events []ChangeEvent

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(workers)

	for _, job := range jobs {
		g.Go(func() error {
			if gCtx.Err() != nil {
				return gCtx.Err()
			}

			hash, err := driveops.ComputeQuickXorHash(job.fsPath)
			if err != nil {
				o.logger.Warn("hash computation failed, emitting event with empty hash",
					slog.String("path", job.dbRelPath), slog.String("error", err.Error()))
			}

			// For modifies: check if hash matches baseline (no real change).
			if !job.isNew && hash != "" {
				existing, _ := o.baseline.GetByPath(job.dbRelPath)
				if existing != nil && hash == existing.LocalHash {
					return nil
				}
			}

			changeType := ChangeCreate
			itemType := ItemTypeFile
			if !job.isNew {
				changeType = ChangeModify
			}

			ev := ChangeEvent{
				Source:   SourceLocal,
				Type:     changeType,
				Path:     job.dbRelPath,
				Name:     job.name,
				ItemType: itemType,
				Size:     job.size,
				Hash:     hash,
				Mtime:    job.mtime,
			}

			mu.Lock()
			events = append(events, ev)
			mu.Unlock()

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("sync: hash phase: %w", err)
	}

	return events, nil
}

// makeWalkFunc returns a WalkDirFunc that classifies filesystem entries
// against the baseline. Folder events are appended to events directly.
// Files that need hashing are appended to jobs for phase 2.
func (o *LocalObserver) makeWalkFunc(
	ctx context.Context, syncRoot string, observed map[string]bool,
	events *[]ChangeEvent, jobs *[]hashJob, skippedEntries *atomic.Int64, scanStartNano int64,
) fs.WalkDirFunc {
	return func(fsPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			o.logger.Warn("walk error", slog.String("path", fsPath), slog.String("error", walkErr.Error()))
			skippedEntries.Add(1)
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

		return o.processEntry(fsPath, dbRelPath, name, d, observed, events, jobs, scanStartNano)
	}
}

// processEntry reads file info, marks the path as observed, and classifies
// the local change against the baseline. Folder events are appended to
// events directly; files that need hashing are appended to jobs for phase 2.
func (o *LocalObserver) processEntry(
	fsPath, dbRelPath, name string, d fs.DirEntry, observed map[string]bool,
	events *[]ChangeEvent, jobs *[]hashJob, scanStartNano int64,
) error {
	info, err := d.Info()
	if err != nil {
		// File disappeared between readdir and stat — skip and continue.
		o.logger.Warn("stat failed (file may have disappeared)",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))
		return nil
	}

	observed[dbRelPath] = true

	return o.classifyLocalChange(fsPath, dbRelPath, name, d, info, events, jobs, scanStartNano)
}

// classifyLocalChange determines the change type for a single local entry
// by comparing it against the baseline. Folder events go directly to events;
// files that need hashing are appended to jobs for the parallel hash phase.
func (o *LocalObserver) classifyLocalChange(
	fsPath, dbRelPath, name string, d fs.DirEntry, info fs.FileInfo,
	events *[]ChangeEvent, jobs *[]hashJob, scanStartNano int64,
) error {
	existing, _ := o.baseline.GetByPath(dbRelPath)

	// No baseline entry — this is a new item.
	if existing == nil {
		if d.IsDir() {
			// Folder creates go directly to events (no hashing needed).
			*events = append(*events, ChangeEvent{
				Source:   SourceLocal,
				Type:     ChangeCreate,
				Path:     dbRelPath,
				Name:     name,
				ItemType: ItemTypeFolder,
				Size:     info.Size(),
				Mtime:    info.ModTime().UnixNano(),
			})
		} else {
			// New file — needs hashing in phase 2.
			*jobs = append(*jobs, hashJob{
				fsPath:    fsPath,
				dbRelPath: dbRelPath,
				name:      name,
				size:      info.Size(),
				mtime:     info.ModTime().UnixNano(),
				isNew:     true,
			})
		}

		return nil
	}

	// Existing folder — OS-level mtime changes (e.g. adding a file) are noise;
	// the contained files generate their own events.
	if d.IsDir() {
		return nil
	}

	return o.classifyFileChange(fsPath, dbRelPath, name, info, existing, jobs, scanStartNano)
}

// classifyFileChange compares a file against its baseline entry to detect
// content modifications. Uses mtime+size as a fast path — only adds a hash
// job when metadata suggests a change. This is the industry standard
// (rsync, rclone, Syncthing, Git all use this pattern). Includes a
// racily-clean guard: files whose mtime is within 1 second of scan start
// are always hashed, because they may have been modified in the same clock
// tick as the last sync (Git's "racily clean" problem).
func (o *LocalObserver) classifyFileChange(
	fsPath, dbRelPath, name string, info fs.FileInfo, base *BaselineEntry,
	jobs *[]hashJob, scanStartNano int64,
) error {
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

			return nil
		}

		o.logger.Debug("racily clean file, forcing hash check",
			slog.String("path", dbRelPath))
	}

	// Slow path: metadata differs (or racily clean) — queue for hash phase.
	*jobs = append(*jobs, hashJob{
		fsPath:    fsPath,
		dbRelPath: dbRelPath,
		name:      name,
		size:      currentSize,
		mtime:     currentMtime,
		isNew:     false,
	})

	return nil
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

// errFileChangedDuringHash is returned when a file's metadata changes between
// pre-hash stat and post-hash stat, indicating active writing (B-119).
var errFileChangedDuringHash = errors.New("sync: file changed during hashing")

// computeStableHash hashes a file and verifies it was not modified during the
// hash operation by comparing pre/post stat results. Returns errFileChangedDuringHash
// if the file changed (B-119). Caller-specific handling: handleWrite skips
// (Write events guarantee a follow-up), handleCreate and scanNewDirectory emit
// with empty hash (Create events and directory scans have no guaranteed follow-up; B-203).
//
// The double os.Stat is intentional: pre-stat captures baseline metadata,
// post-stat detects changes that occurred during hashing. The caller's earlier
// stat cannot substitute because time may pass between the caller's stat and
// the hash operation.
func computeStableHash(fsPath string) (string, error) {
	pre, err := os.Stat(fsPath)
	if err != nil {
		return "", fmt.Errorf("sync: pre-hash stat %s: %w", fsPath, err)
	}

	hash, err := driveops.ComputeQuickXorHash(fsPath)
	if err != nil {
		return "", err
	}

	post, err := os.Stat(fsPath)
	if err != nil {
		return "", fmt.Errorf("sync: post-hash stat %s: %w", fsPath, err)
	}

	if pre.Size() != post.Size() || pre.ModTime() != post.ModTime() {
		return "", errFileChangedDuringHash
	}

	return hash, nil
}

// ---------------------------------------------------------------------------
// Filtering and validation helpers
// ---------------------------------------------------------------------------

// syncRootExists returns true if the sync root directory exists and is a directory.
func syncRootExists(syncRoot string) bool {
	info, err := os.Stat(syncRoot)
	return err == nil && info.IsDir()
}

// isAlwaysExcluded returns true for file patterns that must never be synced.
// These are S7 safety invariants: partial downloads, editor temporaries,
// and SQLite database files (which corrupt if synced mid-transaction).
//
// Called on every fsnotify event and every file during FullScan, so we use
// asciiLower to avoid the heap allocation that strings.ToLower incurs per call.
func isAlwaysExcluded(name string) bool {
	lower := asciiLower(name)

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

// asciiLower returns s with ASCII uppercase letters converted to lowercase.
// Unlike strings.ToLower, this avoids heap allocation when s is already
// lowercase (the common case for filenames). Non-ASCII bytes are passed through
// unchanged, which is correct for file extension matching.
func asciiLower(s string) string {
	for i := range len(s) {
		if s[i] >= 'A' && s[i] <= 'Z' {
			// Found an uppercase letter — allocate and convert.
			buf := make([]byte, len(s))
			copy(buf, s[:i])

			for j := i; j < len(s); j++ {
				if s[j] >= 'A' && s[j] <= 'Z' {
					buf[j] = s[j] + ('a' - 'A')
				} else {
					buf[j] = s[j]
				}
			}

			return string(buf)
		}
	}

	// No uppercase letters found — return the original string (zero alloc).
	return s
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

// skipEntry returns filepath.SkipDir for directories (to skip the subtree)
// or nil for files (to continue the walk with the next entry).
func skipEntry(d fs.DirEntry) error {
	if d != nil && d.IsDir() {
		return filepath.SkipDir
	}

	return nil
}
