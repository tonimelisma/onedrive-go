package sync

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// ErrNosyncGuard is returned when a .nosync guard file is found at the sync root.
// This prevents syncing against an empty or unmounted volume, which could cause
// mass deletions (B-018).
var ErrNosyncGuard = errors.New("sync halted: .nosync guard file found")

// nosyncFileName is the sentinel guard file name checked at the sync root.
const nosyncFileName = ".nosync"

// maxPathChars is the OneDrive maximum total path length in characters.
const maxPathChars = 400

// Scanner walks the local filesystem to detect changes relative to the state database.
// It implements sync-algorithm.md sections 4.1-4.5.
type Scanner struct {
	store        ScannerStore
	filter       Filter
	logger       *slog.Logger
	driveID      string // injected by engine; set on new items so the reconciler can build correct actions
	skipSymlinks bool
	visited      map[string]bool // DB paths visited during current scan (for orphan detection)
}

// NewScanner creates a Scanner with the given dependencies.
// driveID is set on new items so the reconciler can build actions with correct drive identity.
func NewScanner(driveID string, store ScannerStore, filter Filter, skipSymlinks bool, logger *slog.Logger) *Scanner {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Scanner{
		store:        store,
		filter:       filter,
		logger:       logger,
		driveID:      driveID,
		skipSymlinks: skipSymlinks,
	}
}

// Scan walks syncRoot, detecting new, changed, and deleted local files.
// It returns ErrNosyncGuard if a .nosync file is present at the root (B-018).
func (s *Scanner) Scan(ctx context.Context, syncRoot string) error {
	s.logger.Info("scanner: starting local scan", "sync_root", syncRoot)

	// B-018: guard file check prevents syncing against empty/unmounted volume
	if err := s.checkNosyncGuard(syncRoot); err != nil {
		return err
	}

	// Track NFC paths visited during walk for robust orphan detection.
	// On Linux, NFC-normalized DB paths may differ from NFD filesystem paths,
	// so os.Stat alone in orphan detection would produce false positives.
	s.visited = make(map[string]bool)

	if err := s.walkDir(ctx, syncRoot, "", ""); err != nil {
		return fmt.Errorf("scanner: walk failed: %w", err)
	}

	if err := s.detectOrphans(ctx, syncRoot); err != nil {
		return fmt.Errorf("scanner: orphan detection failed: %w", err)
	}

	if err := s.detectFolderOrphans(ctx); err != nil {
		return fmt.Errorf("scanner: folder orphan detection failed: %w", err)
	}

	s.logger.Info("scanner: local scan complete", "sync_root", syncRoot)

	return nil
}

// checkNosyncGuard returns ErrNosyncGuard if a .nosync file exists at the root.
func (s *Scanner) checkNosyncGuard(syncRoot string) error {
	guardPath := filepath.Join(syncRoot, nosyncFileName)

	_, err := os.Stat(guardPath)
	if err == nil {
		s.logger.Warn("scanner: .nosync guard file found, halting sync", "path", guardPath)
		return ErrNosyncGuard
	}

	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("scanner: checking .nosync: %w", err)
	}

	return nil
}

// walkDir performs a depth-first traversal of the directory.
// fsRelPath uses original filesystem names for I/O; dbRelPath uses NFC-normalized names for DB storage.
func (s *Scanner) walkDir(ctx context.Context, fsRoot, fsRelPath, dbRelPath string) error {
	fullPath := filepath.Join(fsRoot, fsRelPath)

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		return fmt.Errorf("scanner: reading directory %q: %w", fullPath, err)
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := s.processEntry(ctx, fsRoot, fsRelPath, dbRelPath, entry); err != nil {
			return err
		}
	}

	return nil
}

// processEntry handles a single directory entry during the walk.
// fsRelPath/dbRelPath track the parent's filesystem and NFC-normalized paths respectively.
func (s *Scanner) processEntry(
	ctx context.Context, fsRoot, fsRelPath, dbRelPath string, entry os.DirEntry,
) error {
	originalName := entry.Name()
	// B-019: NFC normalize to handle macOS NFD filenames.
	// Use original name for filesystem I/O, normalized name for DB storage.
	normalizedName := norm.NFC.String(originalName)

	fsEntryRelPath := joinRelPath(fsRelPath, originalName)
	dbEntryRelPath := joinRelPath(dbRelPath, normalizedName)

	if err := s.validateEntry(normalizedName, dbEntryRelPath, entry); err != nil {
		return nil // validation failures are logged and skipped, not propagated
	}

	// Handle symlinks: resolve or skip depending on configuration (uses filesystem path)
	resolvedEntry, err := s.resolveSymlink(fsRoot, fsEntryRelPath, entry)
	if err != nil || resolvedEntry == nil {
		return nil // broken symlink or skip-symlinks mode
	}

	if resolvedEntry.IsDir() {
		return s.processDirectoryEntry(ctx, fsRoot, fsEntryRelPath, dbEntryRelPath)
	}

	return s.processFileEntry(ctx, fsRoot, fsEntryRelPath, dbEntryRelPath)
}

// processDirectoryEntry tracks a directory in the state DB so the reconciler
// knows whether a folder exists locally. Without this, the reconciler's
// localExists check (via LocalMtime != nil) would always be false for folders.
// Uses NowNano() instead of filesystem mtime because directory mtime changes
// whenever contents change, which is not meaningful for existence tracking.
func (s *Scanner) processDirectoryEntry(
	ctx context.Context, fsRoot, fsRelPath, dbRelPath string,
) error {
	// Mark directory as visited for folder orphan detection
	s.visited[dbRelPath] = true

	existing, err := s.store.GetItemByPath(ctx, dbRelPath)
	if err != nil {
		return fmt.Errorf("scanner: store lookup for dir %q: %w", dbRelPath, err)
	}

	if err := s.upsertDirectoryItem(ctx, dbRelPath, existing); err != nil {
		return err
	}

	return s.walkDir(ctx, fsRoot, fsRelPath, dbRelPath)
}

// upsertDirectoryItem creates or updates the DB entry for a directory found on disk.
// New directories get a fresh item; tombstoned and remote-only directories get LocalMtime set.
func (s *Scanner) upsertDirectoryItem(ctx context.Context, relPath string, existing *Item) error {
	if existing == nil {
		return s.handleNewDirectory(ctx, relPath)
	}

	if existing.IsDeleted {
		return s.handleResurrectedDirectory(ctx, relPath, existing)
	}

	// Remote-only folder now found locally: set LocalMtime so reconciler sees localExists=true
	if existing.LocalMtime == nil {
		return s.handleRemoteOnlyDirectory(ctx, relPath, existing)
	}

	return nil
}

// handleNewDirectory creates a folder item for a directory not yet tracked in the store.
func (s *Scanner) handleNewDirectory(ctx context.Context, relPath string) error {
	now := NowNano()
	item := &Item{
		DriveID:    s.driveID,
		ItemID:     localItemID(relPath),
		Path:       relPath,
		Name:       filepath.Base(relPath),
		ItemType:   ItemTypeFolder,
		LocalMtime: Int64Ptr(now),
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	s.logger.Debug("scanner: new local directory", "path", relPath)

	return s.store.UpsertItem(ctx, item)
}

// handleResurrectedDirectory clears the tombstone on a directory that reappeared locally.
func (s *Scanner) handleResurrectedDirectory(ctx context.Context, relPath string, existing *Item) error {
	existing.IsDeleted = false
	existing.DeletedAt = nil
	existing.LocalMtime = Int64Ptr(NowNano())
	existing.UpdatedAt = NowNano()

	s.logger.Debug("scanner: resurrected tombstoned directory", "path", relPath)

	return s.store.UpsertItem(ctx, existing)
}

// handleRemoteOnlyDirectory sets LocalMtime on a remote-only folder now found on disk.
func (s *Scanner) handleRemoteOnlyDirectory(ctx context.Context, relPath string, existing *Item) error {
	existing.LocalMtime = Int64Ptr(NowNano())
	existing.UpdatedAt = NowNano()

	s.logger.Debug("scanner: remote-only directory now found locally", "path", relPath)

	return s.store.UpsertItem(ctx, existing)
}

// validateEntry checks filter, name validity, and UTF-8 encoding. Returns non-nil to skip.
func (s *Scanner) validateEntry(name, entryRelPath string, entry os.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		s.logger.Warn("scanner: cannot stat entry, skipping", "path", entryRelPath, "error", err)
		return err
	}

	result := s.filter.ShouldSync(entryRelPath, entry.IsDir(), info.Size())
	if !result.Included {
		s.logger.Debug("scanner: excluded by filter", "path", entryRelPath, "reason", result.Reason)
		return errors.New("excluded")
	}

	if valid, reason := isValidOneDriveName(name); !valid {
		s.logger.Warn("scanner: invalid OneDrive name, skipping", "path", entryRelPath, "name", name, "reason", reason)
		return errors.New("invalid name: " + reason)
	}

	if !utf8.ValidString(name) {
		s.logger.Warn("scanner: invalid UTF-8 filename, skipping", "path", entryRelPath)
		return errors.New("invalid utf8")
	}

	if len(entryRelPath) > maxPathChars {
		s.logger.Warn("scanner: path exceeds OneDrive limit, skipping",
			"path", entryRelPath, "length", len(entryRelPath), "max", maxPathChars)
		return errors.New("path too long")
	}

	return nil
}

// resolveSymlink handles symlink entries. Returns nil entry when the entry should be skipped.
func (s *Scanner) resolveSymlink(fsRoot, entryRelPath string, entry os.DirEntry) (os.FileInfo, error) {
	if entry.Type()&os.ModeSymlink == 0 {
		// Not a symlink — return the entry's own info
		return entry.Info()
	}

	if s.skipSymlinks {
		s.logger.Debug("scanner: skipping symlink", "path", entryRelPath)
		return nil, nil //nolint:nilnil // nil,nil signals "skip this entry"
	}

	fullPath := filepath.Join(fsRoot, entryRelPath)

	target, err := os.Stat(fullPath) // follows symlink
	if err != nil {
		s.logger.Warn("scanner: broken symlink, skipping", "path", entryRelPath, "error", err)
		return nil, nil //nolint:nilnil // nil,nil signals "skip this entry"
	}

	return target, nil
}

// processFileEntry handles a file discovered during the walk.
// fsRelPath is the original filesystem path for I/O; dbRelPath is the NFC-normalized path for DB storage.
func (s *Scanner) processFileEntry(ctx context.Context, fsRoot, fsRelPath, dbRelPath string) error {
	fullPath := filepath.Join(fsRoot, fsRelPath)

	info, err := os.Stat(fullPath)
	if err != nil {
		s.logger.Warn("scanner: cannot stat file, skipping", "path", dbRelPath, "error", err)
		return nil
	}

	// Mark as visited for robust orphan detection (NFC/NFD mismatch on Linux)
	s.visited[dbRelPath] = true

	existing, err := s.store.GetItemByPath(ctx, dbRelPath)
	if err != nil {
		return fmt.Errorf("scanner: store lookup for %q: %w", dbRelPath, err)
	}

	if existing == nil {
		return s.handleNewFile(ctx, fullPath, dbRelPath, info)
	}

	if existing.IsDeleted {
		return s.handleResurrectedFile(ctx, fullPath, dbRelPath, info, existing)
	}

	return s.handleExistingFile(ctx, fullPath, dbRelPath, info, existing)
}

// handleNewFile creates a new item for a file not yet tracked in the store.
func (s *Scanner) handleNewFile(ctx context.Context, fullPath, relPath string, info os.FileInfo) error {
	hash, err := computeHash(fullPath)
	if err != nil {
		s.logger.Warn("scanner: hash failed for new file", "path", relPath, "error", err)
		return nil
	}

	now := NowNano()
	item := &Item{
		DriveID:    s.driveID,
		ItemID:     localItemID(relPath),
		Path:       relPath,
		Name:       filepath.Base(relPath),
		ItemType:   ItemTypeFile,
		LocalSize:  Int64Ptr(info.Size()),
		LocalMtime: Int64Ptr(ToUnixNano(info.ModTime().UTC())),
		LocalHash:  hash,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	s.logger.Debug("scanner: new local file", "path", relPath, "size", info.Size())

	return s.store.UpsertItem(ctx, item)
}

// handleResurrectedFile updates a tombstoned item that reappeared locally.
func (s *Scanner) handleResurrectedFile(
	ctx context.Context, fullPath, relPath string, info os.FileInfo, existing *Item,
) error {
	hash, err := computeHash(fullPath)
	if err != nil {
		s.logger.Warn("scanner: hash failed for resurrected file", "path", relPath, "error", err)
		return nil
	}

	existing.LocalSize = Int64Ptr(info.Size())
	existing.LocalMtime = Int64Ptr(ToUnixNano(info.ModTime().UTC()))
	existing.LocalHash = hash
	existing.IsDeleted = false
	existing.DeletedAt = nil
	existing.UpdatedAt = NowNano()

	s.logger.Debug("scanner: resurrected tombstoned file", "path", relPath)

	return s.store.UpsertItem(ctx, existing)
}

// handleExistingFile applies the mtime fast-path / hash slow-path for a known file.
func (s *Scanner) handleExistingFile(
	ctx context.Context, fullPath, relPath string, info os.FileInfo, existing *Item,
) error {
	localMtime := ToUnixNano(info.ModTime().UTC())

	// Fast path: mtime unchanged means content unchanged (section 4.2)
	if existing.LocalMtime != nil && TruncateToSeconds(localMtime) == TruncateToSeconds(*existing.LocalMtime) {
		return nil
	}

	// Slow path: mtime changed, compute hash to verify
	hash, err := computeHash(fullPath)
	if err != nil {
		s.logger.Warn("scanner: hash failed for existing file", "path", relPath, "error", err)
		return nil
	}

	existing.LocalSize = Int64Ptr(info.Size())
	existing.LocalMtime = Int64Ptr(localMtime)
	existing.LocalHash = hash
	existing.UpdatedAt = NowNano()

	s.logger.Debug("scanner: file changed", "path", relPath, "size", info.Size())

	return s.store.UpsertItem(ctx, existing)
}

// detectOrphans finds items in the store whose files no longer exist locally.
// Only items with a confirmed synced hash are treated as deletions (S1 safety).
func (s *Scanner) detectOrphans(ctx context.Context, syncRoot string) error {
	syncedItems, err := s.store.ListSyncedItems(ctx)
	if err != nil {
		return fmt.Errorf("scanner: listing synced items: %w", err)
	}

	for _, item := range syncedItems {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := s.checkOrphan(ctx, syncRoot, item); err != nil {
			return err
		}
	}

	return nil
}

// checkOrphan handles a single synced item during orphan detection.
func (s *Scanner) checkOrphan(ctx context.Context, syncRoot string, item *Item) error {
	// If this NFC path was visited during the walk, the file exists on disk.
	// This handles Linux where NFC DB paths differ from NFD filesystem paths.
	if s.visited[item.Path] {
		return nil
	}

	fullPath := filepath.Join(syncRoot, item.Path)

	_, err := os.Stat(fullPath)
	if err == nil {
		return nil // file still exists
	}

	if !errors.Is(err, os.ErrNotExist) {
		s.logger.Warn("scanner: cannot stat for orphan check", "path", item.Path, "error", err)
		return nil
	}

	// S1 safety: only treat as deletion if previously synced
	if item.SyncedHash == "" {
		s.logger.Debug("scanner: unsynced item missing locally, ignoring", "path", item.Path)
		return nil
	}

	// Confirmed synced and now missing: mark local fields as cleared
	item.LocalHash = ""
	item.LocalSize = nil
	item.LocalMtime = nil
	item.UpdatedAt = NowNano()

	s.logger.Debug("scanner: orphan detected (local deletion)", "path", item.Path)

	return s.store.UpsertItem(ctx, item)
}

// detectFolderOrphans finds folder items in the store that are no longer present
// on the local filesystem. Unlike file orphan detection (which uses ListSyncedItems),
// folder orphan detection checks all active folder items with LocalMtime set, because
// folders don't have a SyncedHash to gate on.
func (s *Scanner) detectFolderOrphans(ctx context.Context) error {
	allItems, err := s.store.ListAllActiveItems(ctx)
	if err != nil {
		return fmt.Errorf("scanner: listing active items for folder orphans: %w", err)
	}

	for i := range allItems {
		if err := ctx.Err(); err != nil {
			return err
		}

		item := allItems[i]
		if item.ItemType != ItemTypeFolder || item.LocalMtime == nil {
			continue
		}

		if s.visited[item.Path] {
			continue
		}

		// Folder had LocalMtime but was not visited during walk: local deletion
		item.LocalMtime = nil
		item.UpdatedAt = NowNano()

		s.logger.Debug("scanner: folder orphan detected (local deletion)", "path", item.Path)

		if err := s.store.UpsertItem(ctx, item); err != nil {
			return fmt.Errorf("scanner: upserting folder orphan %q: %w", item.Path, err)
		}
	}

	return nil
}

// computeHash streams a file through QuickXorHash and returns the base64 digest.
func computeHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening file for hash: %w", err)
	}
	defer f.Close()

	h := quickxorhash.New()

	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing file: %w", err)
	}

	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// localItemID generates a unique temporary ItemID for scanner-created items.
// The "local:" prefix prevents collision with server-assigned IDs (which use
// the drive's hex ID prefix, e.g., "BD50CF43646E28E6!sXXX"). The path suffix
// makes each ID unique within a drive. The executor replaces this with the
// server-assigned ID on upload (B-050 cleanup).
func localItemID(relPath string) string {
	return "local:" + relPath
}

// joinRelPath builds a relative path from a parent and child component.
// If parent is empty (root level), returns just the child.
func joinRelPath(parent, child string) string {
	if parent == "" {
		return child
	}

	return parent + "/" + child
}
