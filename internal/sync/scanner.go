// Package sync owns sync-engine runtime, including local observation and
// scanner helpers that the merged engine/executor package now shares directly.
//
// Contents:
//   - FullScan:              orchestrates walk + hash phases → ScanResult
//   - hashPhase:             parallel hash computation for discovered files
//   - makeWalkFunc:          builds the filepath.WalkDir callback
//   - classifyLocalChange:   compares local state against baseline
//   - detectDeletions:       finds baseline entries missing from walk
//   - ComputeStableHash:     double-stat hash for actively-written files
//   - shouldObserveWithFilter: unified observation filter (Stage 1: name + path)
//   - IsOversizedFile:       Stage 2 observation filter (file size > 250GB)
//   - ValidateOneDriveName:  returns reason + detail for invalid names
//
// Related files:
//   - observer_local.go:          LocalObserver struct and Watch() entry point
//   - observer_local_handlers.go: watch-mode event handlers
package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// scanResult is the return type of FullScan. Rows are the direct current local
// snapshot that local_state persists. Events remain observation-local signals
// for watch dirtiness and diagnostics; they are not planner input. Skipped are
// user-actionable rejections (invalid names, path too long, file too large)
// that the engine should record.
type scanResult struct {
	Events  []changeEvent
	Rows    []localStateRow
	Skipped []skippedItem
	// CrossedMounts are mount points this scan crossed under
	// cross_filesystems; the engine remembers them (mount_boundaries).
	CrossedMounts []string
}

// Constants for the local scanner.
const (
	nanosPerSecond         = 1_000_000_000
	maxComponentLength     = 255
	deviceNameWithDigitLen = 4 // COM0-COM9, LPT0-LPT9 have exactly 4 characters

	// maxOneDrivePathLength is the maximum total path length OneDrive allows,
	// in characters as oneDrivePathLength counts them. The server is still the
	// authority: it counts more than the drive-relative path, and its refusal
	// maps to path_too_long (R-2.11.7).
	maxOneDrivePathLength = 400
	// maxOneDriveFileSize is the maximum file size OneDrive allows (250 GB).
	maxOneDriveFileSize = driveops.MaxOneDriveFileSize
	// bytesPerGB renders the oversized-file detail in GB (1024^3 bytes).
	bytesPerGB = 1 << 30
)

// defaultCheckWorkers is the default parallel hash goroutine limit when
// checkWorkers is zero (not configured).
const defaultCheckWorkers = 4

// hashJob describes a file that needs hashing during FullScan phase 2.
type hashJob struct {
	fsPath           string
	dbRelPath        string
	name             string
	size             int64
	mtime            int64
	localDevice      uint64
	localInode       uint64
	localHasIdentity bool
	isNew            bool // true for creates, false for modifies
}

// resolveCheckWorkers returns the effective check worker count.
func (o *localObserver) resolveCheckWorkers() int {
	if o.checkWorkers > 0 {
		return o.checkWorkers
	}

	return defaultCheckWorkers
}

// FullScan walks the sync root directory and returns a ScanResult containing
// change events for all local changes (creates, modifies, deletes) relative
// to the baseline, plus any skipped items that should be recorded as
// observation issues.
//
// Three-phase design:
//  1. Walk (sequential): collect observed map, emit folder creates, classify
//     files that need hashing into a hashJob slice. Collects SkippedItems
//     for invalid names, too-long paths, and too-large files.
//  2. Hash (parallel): errgroup.SetLimit(checkWorkers) hashes files concurrently.
//  3. Deletion detection (sequential): compare observed vs baseline.
func (o *localObserver) FullScan(ctx context.Context, tree *synctree.Root) (scanResult, error) {
	syncRoot := tree.Path()
	o.logger.Info("local observer starting full scan",
		slog.String("sync_root", syncRoot),
		slog.Int("baseline_entries", o.baseline.Len()),
	)

	if err := o.validateFullScanRoot(tree, syncRoot); err != nil {
		return scanResult{}, err
	}
	rootDevice, err := rootDeviceForScan(tree)
	if err != nil {
		return scanResult{}, err
	}
	o.scanAvailability = newScanAvailability(rootDevice)
	o.scanAvailability.tree = tree
	defer func() { o.scanAvailability = nil }()

	// Phase 1: Walk — collect observed paths, folder events, hash jobs, and skipped items.
	var events []changeEvent
	var jobs []hashJob
	var skipped []skippedItem
	var skippedEntries atomic.Int64
	observed := make(map[string]bool)
	currentRows := make(map[string]localStateRow)
	scanStartNano := time.Now().UnixNano()
	dirStack := rootObservedDirStack(syncRoot, o.logger)

	walkFn := o.makeWalkFunc(
		ctx,
		tree,
		observed,
		currentRows,
		&events,
		&jobs,
		&skipped,
		&skippedEntries,
		scanStartNano,
		dirStack,
	)
	if err := tree.WalkDir(walkFn); err != nil {
		if ctx.Err() != nil {
			return scanResult{}, fmt.Errorf("sync: local scan canceled: %w", ctx.Err())
		}

		return scanResult{}, fmt.Errorf("sync: walking %s: %w", syncRoot, err)
	}

	// Debug: unreadable entries are recorded as skipped items, which the
	// engine reports once when first found and `status` lists.
	if n := skippedEntries.Load(); n > 0 {
		o.logger.Debug("full scan: skipped entries due to walk errors",
			slog.Int64("count", n),
			slog.String("sync_root", syncRoot))
	}

	// Phase 2: Hash — parallel file hashing. Panics in hash goroutines are
	// recovered and converted to SkippedItems (defensive coding).
	if len(jobs) > 0 {
		hashEvents, hashRows, hashSkipped, err := o.hashPhase(ctx, jobs)
		if err != nil {
			return scanResult{}, err
		}

		events = append(events, hashEvents...)
		for i := range hashRows {
			currentRows[hashRows[i].Path] = hashRows[i]
		}
		skipped = append(skipped, hashSkipped...)
	}

	// Phase 2.5: Case collision detection — run after hashing (events finalized)
	// but before deletion detection. Colliding files stay in the observed map
	// (set in Phase 1) to prevent Phase 3 from generating spurious ChangeDelete
	// events for files that exist locally but were excluded from events (R-2.12.1).
	var caseSkipped []skippedItem
	events, caseSkipped = detectCaseCollisions(events, o.baseline, observed)
	skipped = append(skipped, caseSkipped...)

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
		slog.Int("skipped", len(skipped)),
	)

	if len(events) > 0 {
		o.recordActivity()
	}

	return scanResult{
		Events:        events,
		Rows:          sortedLocalStateRows(currentRows),
		Skipped:       skipped,
		CrossedMounts: append([]string(nil), o.scanAvailability.crossedMounts...),
	}, nil
}

func (o *localObserver) validateFullScanRoot(tree *synctree.Root, syncRoot string) error {
	// Guard: abort if the sync root directory does not exist. Without this,
	// WalkDir silently succeeds with zero events (walkFn's SkipEntry returns
	// filepath.SkipDir for the root error, so WalkDir returns nil).
	if !syncRootExists(syncRoot) {
		// Debug: the returned error is reported once by the caller.
		o.logger.Debug("sync root missing, aborting scan",
			slog.String("sync_root", syncRoot))
		return errSyncRootMissing
	}
	if err := o.expectedRoot.Validate(tree); err != nil {
		o.logger.Debug("sync root identity changed, aborting scan",
			slog.String("sync_root", syncRoot),
			slog.String("error", err.Error()))
		return err
	}
	return nil
}

// hashPhase runs hash jobs in parallel using errgroup with checkWorkers limit.
// Returns the resulting change events (creates and modifies with hashes), plus
// any skipped items from panics in hash goroutines. Panics are recovered and
// converted to SkippedItem entries — a single corrupted file cannot crash the
// entire scan (defensive coding per eng philosophy).
//
//nolint:funlen // The hashing pipeline keeps local-scan concurrency and panic recovery in one explicit owner.
func (o *localObserver) hashPhase(ctx context.Context, jobs []hashJob) ([]changeEvent, []localStateRow, []skippedItem, error) {
	workers := o.resolveCheckWorkers()

	o.logger.Debug("starting parallel hash phase",
		slog.Int("jobs", len(jobs)),
		slog.Int("workers", workers),
	)

	hashFn := o.hashFunc
	if hashFn == nil {
		hashFn = driveops.ComputeQuickXorHash
	}

	// mu guards the three result slices below. The walk fans out to workers
	// bounded by g.SetLimit, and each appends its own findings.
	var mu stdsync.Mutex
	var events []changeEvent
	var rows []localStateRow
	var skipped []skippedItem

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(workers)

	for _, job := range jobs {
		g.Go(func() (retErr error) {
			// Recover from panics in hash computation (e.g., corrupt file
			// triggering a nil dereference in the hash library). Convert to
			// SkippedItem so the rest of the scan completes normally.
			defer func() {
				if r := recover(); r != nil {
					o.logger.Error("hash phase: panic in worker",
						slog.String("path", job.dbRelPath),
						slog.Any("panic", r),
						slog.String("stack", string(debug.Stack())),
					)

					mu.Lock()
					skipped = append(skipped, skippedItem{
						Path:   job.dbRelPath,
						Reason: issueHashPanic,
						Detail: fmt.Sprintf("panic: %v", r),
					})
					mu.Unlock()
				}
			}()

			if gCtx.Err() != nil {
				return gCtx.Err()
			}

			hash, err := hashFn(job.fsPath)
			if err != nil {
				// Debug: this repeats on every safety scan for a file that
				// stats but cannot be opened. An unreadable file is recorded
				// as a skipped item instead, reported once and in `status`.
				o.logger.Debug("hash computation failed, emitting event with empty hash",
					slog.String("path", job.dbRelPath), slog.String("error", err.Error()))
				switch {
				case errors.Is(err, os.ErrPermission):
					mu.Lock()
					skipped = append(skipped, skippedItem{
						Path:   job.dbRelPath,
						Reason: issueLocalReadDenied,
						Detail: "file not readable (check filesystem permissions)",
					})
					mu.Unlock()
				case !errors.Is(err, os.ErrNotExist):
					// An I/O error is not a content change: without the
					// issue the empty hash read as "changed", and an upload
					// that failed the same way was planned on every pass
					// (D-005). The issue makes the path unavailable truth.
					mu.Lock()
					skipped = append(skipped, skippedItem{
						Path:   job.dbRelPath,
						Reason: issueHashPanic,
						Detail: fmt.Sprintf("could not read the file to check its content (%v)", err),
					})
					mu.Unlock()
				}
			}

			// For modifies: check if hash matches baseline (no real change).
			if !job.isNew && hash != "" {
				if existing, found := o.baseline.GetByPath(job.dbRelPath); found && hash == existing.LocalHash {
					mu.Lock()
					rows = append(rows, localStateRow{
						Path:             job.dbRelPath,
						ItemType:         ItemTypeFile,
						Hash:             hash,
						Size:             job.size,
						Mtime:            job.mtime,
						LocalDevice:      job.localDevice,
						LocalInode:       job.localInode,
						LocalHasIdentity: job.localHasIdentity,
					})
					mu.Unlock()
					return nil
				}
			}

			changeType := changeCreate
			itemType := ItemTypeFile
			if !job.isNew {
				changeType = ChangeModify
			}

			ev := changeEvent{
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
			rows = append(rows, localStateRow{
				Path:             job.dbRelPath,
				ItemType:         itemType,
				Hash:             hash,
				Size:             job.size,
				Mtime:            job.mtime,
				LocalDevice:      job.localDevice,
				LocalInode:       job.localInode,
				LocalHasIdentity: job.localHasIdentity,
			})
			mu.Unlock()

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, nil, fmt.Errorf("sync: hash phase: %w", err)
	}

	return events, rows, skipped, nil
}

// makeWalkFunc returns a WalkDirFunc that classifies filesystem entries
// against the baseline. Folder events are appended to events directly.
// Files that need hashing are appended to jobs for phase 2. User-actionable
// rejections are appended to skipped for engine recording.
func (o *localObserver) makeWalkFunc(
	ctx context.Context, tree *synctree.Root, observed map[string]bool, currentRows map[string]localStateRow,
	events *[]changeEvent, jobs *[]hashJob, skipped *[]skippedItem,
	skippedEntries *atomic.Int64, scanStartNano int64, dirStack map[string]struct{},
) fs.WalkDirFunc {
	syncRoot := tree.Path()

	return func(fsPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil && fsPath == syncRoot {
			// The sync folder itself could not be read (EIO on a dying disk,
			// a stale network mount). A read boundary would be recorded at
			// ".", which no path lies under, so every synced file would look
			// deleted. Nothing about the tree is known: stop the scan (D-005).
			return fmt.Errorf("%w: reading the sync folder: %w", ErrMountRootUnavailable, walkErr)
		}
		if walkErr != nil {
			// Debug: a persistent unreadable folder would warn on every
			// scan; the skipped item below is the single user-facing report.
			o.logger.Debug("walk error", slog.String("path", fsPath), slog.String("error", walkErr.Error()))
			// Any unreadable entry other than one that vanished mid-walk is a
			// read boundary: an I/O error must not make content look deleted
			// any more than a permission error does.
			if relPath, err := tree.Rel(fsPath); err == nil {
				recordUnreadableObservedBoundary(skipped, nfcNormalize(filepath.ToSlash(relPath)), walkErr, unreadableDirectoryDetail)
			}
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

		relPath, err := tree.Rel(fsPath)
		if err != nil {
			return fmt.Errorf("sync: computing relative path for %s: %w", fsPath, err)
		}

		// Normalize: forward slashes for cross-platform consistency + NFC Unicode.
		dbRelPath := nfcNormalize(filepath.ToSlash(relPath))
		name := nfcNormalize(d.Name())

		if d.Type()&fs.ModeSymlink != 0 {
			if !newContentFilter(o.filterConfig).ShouldFollowSymlinks() {
				o.rememberExcludedSymlink(dbRelPath)
				o.recordSyncedPathReplaced(skipped, dbRelPath, "a symbolic link")
				observed[dbRelPath] = true
				o.logger.Debug("skipping symlink", slog.String("path", dbRelPath))
				return skipEntry(d)
			}

			return o.processSymlinkPath(
				ctx,
				fsPath,
				dbRelPath,
				name,
				observed,
				currentRows,
				events,
				jobs,
				skipped,
				scanStartNano,
				dirStack,
			)
		}

		o.forgetExcludedSymlink(dbRelPath)

		// Stage 1 observation filter: name validation + path length (cheap, no syscall).
		if skipItem := shouldObserveWithFilter(
			name,
			dbRelPath,
			dirEntryKind(d),
			o.filterConfig,
			o.protectedRoots,
			o.observationRules,
		); skipItem != nil {
			if skipItem.Reason != "" {
				*skipped = append(*skipped, *skipItem)
				o.logger.Debug("skipping invalid entry",
					slog.String("path", dbRelPath),
					slog.String("reason", skipItem.Reason))
			} else {
				o.logger.Debug("skipping excluded file", slog.String("name", name))
			}
			if synctree.IsDetachArtifactName(name) {
				// A file set aside by a removal a crash interrupted: user
				// content left under a hidden name. Report it every scan
				// until the user deals with it; never delete it (P-4).
				*skipped = append(*skipped, skippedItem{
					Path:   dbRelPath,
					Reason: issueLocalInterruptedRemoval,
					Detail: "sync set this file aside while removing it and was interrupted; " +
						"rename it back to keep it, or delete it",
				})
			}
			// A synced item renamed to this skipped name (invalid, too long,
			// excluded) is still the synced item: keep its synced path
			// unavailable instead of letting it read as deleted.
			o.recordSyncedItemMovedOutOfReach(skipped, o.scanAvailability, dbRelPath, d)

			return skipEntry(d)
		}

		return o.processEntry(fsPath, dbRelPath, name, d, observed, currentRows, events, jobs, skipped, scanStartNano)
	}
}

// processEntry reads file info, marks the path as observed, and classifies
// the local change against the baseline. Folder events are appended to
// events directly; files that need hashing are appended to jobs for phase 2.
// Stage 2 observation filter: file size > 250GB is checked here (after stat).
func (o *localObserver) processEntry(
	fsPath, dbRelPath, name string, d fs.DirEntry, observed map[string]bool,
	currentRows map[string]localStateRow,
	events *[]changeEvent, jobs *[]hashJob, skipped *[]skippedItem, scanStartNano int64,
) error {
	info, err := d.Info()
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			*skipped = append(*skipped, skippedItem{
				Path:   dbRelPath,
				Reason: issueLocalReadDenied,
				Detail: "file not accessible (check filesystem permissions)",
			})
		} else {
			// Anything but "it vanished" (an I/O error, a stale network
			// handle) is unknown, not absent (D-005).
			recordUnreadableEntry(skipped, dbRelPath, err)
		}
		// File disappeared between readdir and stat (an expected race), or
		// is unreadable (recorded above) -- skip and continue.
		o.logger.Debug("stat failed (file may have disappeared)",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))
		return nil
	}

	return o.processObservedInfo(
		fsPath,
		dbRelPath,
		name,
		info,
		dirEntryKind(d),
		observed,
		currentRows,
		events,
		jobs,
		skipped,
		scanStartNano,
	)
}

func (o *localObserver) processObservedInfo(
	fsPath, dbRelPath, name string,
	info fs.FileInfo,
	kind observedKind,
	observed map[string]bool,
	currentRows map[string]localStateRow,
	events *[]changeEvent,
	jobs *[]hashJob,
	skipped *[]skippedItem,
	scanStartNano int64,
) error {
	if protectedRoot, ok := protectedRootIdentityReservation(dbRelPath, info, o.protectedRoots); ok {
		o.reportProtectedRootEvent(protectedRootEvent{
			Type:         protectedRootEventIdentityMatch,
			Path:         dbRelPath,
			ReservedPath: protectedRoot.Path,
			MountID:      protectedRoot.MountID,
			BindingID:    protectedRoot.BindingID,
		})
		if kind == observedKindDir {
			identity := synctree.FileIdentity{Device: protectedRoot.Device, Inode: protectedRoot.Inode}
			if currentIdentity, hasIdentity := synctree.IdentityFromFileInfo(info); hasIdentity {
				identity = currentIdentity
			}
			observed[dbRelPath] = true
			currentRows[dbRelPath] = localStateRow{
				Path:             dbRelPath,
				ItemType:         ItemTypeFolder,
				Size:             info.Size(),
				Mtime:            info.ModTime().UnixNano(),
				LocalDevice:      identity.Device,
				LocalInode:       identity.Inode,
				LocalHasIdentity: true,
			}
		}
		o.logger.Debug("skipping protected root identity match",
			slog.String("path", dbRelPath),
			slog.String("reserved_path", protectedRoot.Path))
		return nil
	}

	// FIFOs, sockets and devices have no content to sync, and opening a FIFO
	// to hash it would block until a writer appears. Marking the path
	// observed keeps deletion detection from treating it as removed.
	if isSpecialFile(info) {
		o.recordSyncedPathReplaced(skipped, dbRelPath, "a FIFO, socket or device file")
		observed[dbRelPath] = true
		o.logger.Debug("skipping special file",
			slog.String("path", dbRelPath),
			slog.String("mode", info.Mode().Type().String()))
		return nil
	}

	// Stage 2 observation filter: file size check (requires stat, hence here).
	// FullScan records SkippedItems for oversized files; watch handlers don't
	// (the safety scan catches them).
	if kind == observedKindFile && o.IsOversizedFile(info.Size(), dbRelPath) {
		*skipped = append(*skipped, skippedItem{
			Path:     dbRelPath,
			Reason:   issueFileTooLarge,
			Detail:   fmt.Sprintf("file is %d GB; OneDrive allows at most 250 GB", info.Size()/bytesPerGB),
			FileSize: info.Size(),
		})
		return nil
	}

	if kind == observedKindDir {
		identity, hasIdentity := synctree.IdentityFromFileInfo(info)
		if o.isForeignMountPoint(o.scanAvailability, dbRelPath, identity, hasIdentity) {
			*skipped = append(*skipped, mountBoundarySkippedItem(dbRelPath))
			observed[dbRelPath] = true
			o.logger.Debug("not crossing into another filesystem", slog.String("path", dbRelPath))
			return filepath.SkipDir
		}
		if o.flaggedMountIsGone(o.scanAvailability, dbRelPath) {
			*skipped = append(*skipped, vanishedMountSkippedItem(dbRelPath))
			observed[dbRelPath] = true
			o.logger.Debug("a crossed mount point is no longer mounted", slog.String("path", dbRelPath))
			return filepath.SkipDir
		}
	}

	observed[dbRelPath] = true
	row := localStateRow{
		Path:  dbRelPath,
		Mtime: info.ModTime().UnixNano(),
	}
	if identity, ok := synctree.IdentityFromFileInfo(info); ok {
		row.LocalDevice = identity.Device
		row.LocalInode = identity.Inode
		row.LocalHasIdentity = true
	}
	switch kind {
	case observedKindDir:
		row.ItemType = ItemTypeFolder
		row.Size = info.Size()
		currentRows[dbRelPath] = row
	case observedKindFile:
		row.ItemType = ItemTypeFile
		row.Size = info.Size()
		currentRows[dbRelPath] = row
	case observedKindUnknown:
		return fmt.Errorf("walk observed entry %s: unknown observed kind", dbRelPath)
	}

	return o.classifyObservedInfo(fsPath, dbRelPath, name, info, kind, currentRows, events, jobs, scanStartNano)
}

// classifyObservedInfo determines the change type for a single observed local
// entry by comparing it against the baseline. Folder events go directly to
// events; files that need hashing are appended to jobs for the parallel hash
// phase.
func (o *localObserver) classifyObservedInfo(
	fsPath, dbRelPath, name string,
	info fs.FileInfo,
	kind observedKind,
	currentRows map[string]localStateRow,
	events *[]changeEvent,
	jobs *[]hashJob,
	scanStartNano int64,
) error {
	var existing *BaselineEntry
	if baselineEntry, found := o.baseline.GetByPath(dbRelPath); found {
		existing = baselineEntry
	}

	// No baseline entry — this is a new item.
	if existing == nil {
		observedRow := currentRows[dbRelPath]
		if kind == observedKindDir {
			// Folder creates go directly to events (no hashing needed).
			*events = append(*events, changeEvent{
				Source:   SourceLocal,
				Type:     changeCreate,
				Path:     dbRelPath,
				Name:     name,
				ItemType: ItemTypeFolder,
				Size:     info.Size(),
				Mtime:    info.ModTime().UnixNano(),
			})
		} else {
			// New file — needs hashing in phase 2.
			*jobs = append(*jobs, hashJob{
				fsPath:           fsPath,
				dbRelPath:        dbRelPath,
				name:             name,
				size:             info.Size(),
				mtime:            info.ModTime().UnixNano(),
				localDevice:      observedRow.LocalDevice,
				localInode:       observedRow.LocalInode,
				localHasIdentity: observedRow.LocalHasIdentity,
				isNew:            true,
			})
		}

		return nil
	}

	// Existing folder — OS-level mtime changes (e.g. adding a file) are noise;
	// the contained files generate their own events.
	if kind == observedKindDir {
		return nil
	}

	return o.detectFileContentChange(fsPath, dbRelPath, name, info, existing, currentRows, jobs, scanStartNano)
}

// detectFileContentChange compares a file against its baseline entry to detect
// content modifications. Uses mtime+size as a fast path — only adds a hash
// job when metadata suggests a change. This is the industry standard
// (rsync, rclone, Syncthing, Git all use this pattern). Includes a
// racily-clean guard: files whose mtime is within 1 second of scan start
// are always hashed, because they may have been modified in the same clock
// tick as the last sync (Git's "racily clean" problem).
func (o *localObserver) detectFileContentChange(
	fsPath, dbRelPath, name string, info fs.FileInfo, base *BaselineEntry,
	currentRows map[string]localStateRow,
	jobs *[]hashJob, scanStartNano int64,
) error {
	currentMtime := info.ModTime().UnixNano()
	currentSize := info.Size()
	observedRow := currentRows[dbRelPath]

	if canReuseBaselineHash(info, base, scanStartNano) {
		o.logger.Debug("fast path: mtime+size match, skipping hash",
			slog.String("path", dbRelPath))
		currentRows[dbRelPath] = localStateRow{
			Path:             dbRelPath,
			ItemType:         ItemTypeFile,
			Hash:             base.LocalHash,
			Size:             currentSize,
			Mtime:            currentMtime,
			LocalDevice:      observedRow.LocalDevice,
			LocalInode:       observedRow.LocalInode,
			LocalHasIdentity: observedRow.LocalHasIdentity,
		}

		return nil
	}

	if base.LocalSizeKnown && currentSize == base.LocalSize && sameOneDriveComparableMtime(currentMtime, base.LocalMtime) {
		o.logger.Debug("racily clean file, forcing hash check",
			slog.String("path", dbRelPath))
	}

	// Slow path: metadata differs (or racily clean) — queue for hash phase.
	*jobs = append(*jobs, hashJob{
		fsPath:           fsPath,
		dbRelPath:        dbRelPath,
		name:             name,
		size:             currentSize,
		mtime:            currentMtime,
		localDevice:      observedRow.LocalDevice,
		localInode:       observedRow.LocalInode,
		localHasIdentity: observedRow.LocalHasIdentity,
		isNew:            false,
	})

	return nil
}

func sortedLocalStateRows(current map[string]localStateRow) []localStateRow {
	paths := make([]string, 0, len(current))
	for path := range current {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	rows := make([]localStateRow, 0, len(paths))
	for _, path := range paths {
		rows = append(rows, current[path])
	}

	return rows
}

// detectCaseCollisions finds events where two paths in the same directory
// differ only in case. Both colliders are removed from events and returned
// as SkippedItems. OneDrive uses a case-insensitive namespace — uploading
// both would cause one to silently overwrite the other (R-2.12.1).
//
// observed is the set of paths this scan found on disk. A baseline variant
// that is not on disk any more is the old name of a case-only rename (or a
// local delete), not a collision; the planner orders the remote side so the
// old item is gone before the new name is created (R-2.12.1). A nil observed
// set treats every baseline variant as present.
//
// O(n) time, O(n) memory. Pure function — no side effects.
func detectCaseCollisions(
	events []changeEvent, baseline *Baseline, observed map[string]bool,
) (clean []changeEvent, collisions []skippedItem) {
	if len(events) == 0 {
		return nil, nil
	}

	// Group event indices by (directory, OneDrive namespace key): case,
	// Unicode form and ignored emoji all make two local names one remote one.
	groups := make(map[caseGroupKey][]int, len(events))
	for i := range events {
		key := caseGroupKey{dir: filepath.Dir(events[i].Path), nameKey: namekey.Key(filepath.Base(events[i].Path))}
		groups[key] = append(groups[key], i)
	}

	// Build the collider set — all indices that participate in a collision.
	colliderSet := make(map[int]struct{})
	for _, indices := range groups {
		if len(indices) > 1 {
			for _, idx := range indices {
				colliderSet[idx] = struct{}{}
			}
		}
	}

	// Cross-check single-event groups against baseline.
	crossCheckBaseline(events, groups, baseline, observed, colliderSet)

	// Suppress children of colliding directories.
	childColliderSet, collidingDirPrefixes := suppressDirectoryChildren(events, colliderSet)

	if len(colliderSet) == 0 {
		return events, nil
	}

	// Build SkippedItems with Detail naming the other collider(s).
	collisions = buildCollisionSkippedItems(
		events, groups, colliderSet, childColliderSet, collidingDirPrefixes, baseline, observed)

	// Build clean events — those not in the collider set.
	clean = make([]changeEvent, 0, len(events)-len(colliderSet))
	for i := range events {
		if _, collider := colliderSet[i]; !collider {
			clean = append(clean, events[i])
		}
	}

	return clean, collisions
}

// caseGroupKey groups events by (directory, namespace key) for collision detection.
type caseGroupKey struct {
	dir     string
	nameKey string
}

// crossCheckBaseline flags single-event groups that collide with already-synced
// baseline entries. A new file whose lowercased name matches a baseline entry
// with different exact casing is a collision (the baseline file produced no event
// because it was unchanged by the fast-path content-change check).
func crossCheckBaseline(
	events []changeEvent,
	groups map[caseGroupKey][]int,
	baseline *Baseline,
	observed map[string]bool,
	colliderSet map[int]struct{},
) {
	if baseline == nil {
		return
	}

	for key, indices := range groups {
		if len(indices) != 1 {
			continue
		}

		if _, already := colliderSet[indices[0]]; already {
			continue
		}

		ev := &events[indices[0]]
		variants := baseline.GetCaseVariants(key.dir, filepath.Base(ev.Path))

		for _, v := range variants {
			if v.Path != ev.Path && baselineVariantPresent(v, observed) {
				colliderSet[indices[0]] = struct{}{}

				break
			}
		}
	}
}

// baselineVariantPresent reports whether a baseline case variant still exists
// on disk in this scan. Without an observed set every variant counts.
func baselineVariantPresent(v *BaselineEntry, observed map[string]bool) bool {
	return observed == nil || observed[v.Path]
}

// suppressDirectoryChildren marks children of colliding directories as colliders.
// They can't be uploaded to a folder that won't exist on OneDrive.
func suppressDirectoryChildren(
	events []changeEvent, colliderSet map[int]struct{},
) (childColliderSet map[int]struct{}, collidingDirPrefixes []string) {
	childColliderSet = make(map[int]struct{})

	for idx := range colliderSet {
		if events[idx].ItemType == ItemTypeFolder {
			collidingDirPrefixes = append(collidingDirPrefixes, events[idx].Path+"/")
		}
	}

	for i := range events {
		if _, already := colliderSet[i]; already {
			continue
		}

		for _, prefix := range collidingDirPrefixes {
			if strings.HasPrefix(events[i].Path, prefix) {
				colliderSet[i] = struct{}{}
				childColliderSet[i] = struct{}{}

				break
			}
		}
	}

	return childColliderSet, collidingDirPrefixes
}

// buildCollisionSkippedItems constructs SkippedItems with Detail messages for
// event-vs-event collisions, baseline cross-check collisions, and child collisions.
func buildCollisionSkippedItems(
	events []changeEvent,
	groups map[caseGroupKey][]int,
	colliderSet, childColliderSet map[int]struct{},
	collidingDirPrefixes []string,
	baseline *Baseline,
	observed map[string]bool,
) []skippedItem {
	collisions := make([]skippedItem, 0, len(colliderSet))

	// Event-vs-event and baseline collisions.
	for _, indices := range groups {
		if len(indices) <= 1 {
			collisions = appendSingleGroupCollision(
				collisions, events, indices, colliderSet, childColliderSet, baseline, observed)

			continue
		}

		collisions = appendMultiGroupCollisions(
			collisions, events, indices, childColliderSet)
	}

	// Child collisions — distinct Detail indicating the parent directory collision.
	for idx := range childColliderSet {
		ev := &events[idx]

		parentDir := ""
		for _, prefix := range collidingDirPrefixes {
			if strings.HasPrefix(ev.Path, prefix) {
				parentDir = strings.TrimSuffix(prefix, "/")

				break
			}
		}

		collisions = append(collisions, skippedItem{
			Path:   ev.Path,
			Reason: issueCaseCollision,
			Detail: fmt.Sprintf("parent directory %q has a case collision",
				filepath.Base(parentDir)),
		})
	}

	return collisions
}

// appendSingleGroupCollision handles SkippedItem construction for a group with
// exactly one event (flagged by baseline cross-check).
func appendSingleGroupCollision(
	collisions []skippedItem,
	events []changeEvent,
	indices []int,
	colliderSet, childColliderSet map[int]struct{},
	baseline *Baseline,
	observed map[string]bool,
) []skippedItem {
	idx := indices[0]

	if _, flagged := colliderSet[idx]; !flagged {
		return collisions
	}

	if _, isChild := childColliderSet[idx]; isChild {
		return collisions // handled in child pass
	}

	ev := &events[idx]

	if baseline == nil {
		return collisions
	}

	variants := baseline.GetCaseVariants(filepath.Dir(ev.Path), filepath.Base(ev.Path))
	for _, v := range variants {
		if v.Path != ev.Path && baselineVariantPresent(v, observed) {
			return append(collisions, skippedItem{
				Path:   ev.Path,
				Reason: issueCaseCollision,
				Detail: collisionDetail("conflicts with synced file", filepath.Base(ev.Path), []string{filepath.Base(v.Path)}),
			})
		}
	}

	return collisions
}

// collisionDetail names the other colliding file(s). When the names differ in
// characters OneDrive ignores when comparing names (emoji and anything else
// beyond U+FFFF), the visible difference would leave the user puzzled about
// why `a.txt` and `a😀.txt` collide, so the detail names both and says why.
func collisionDetail(prefix, name string, others []string) string {
	detail := prefix + " " + strings.Join(others, ", ")
	for _, other := range others {
		if namekey.DiffersInIgnoredCharacters(name, other) {
			return fmt.Sprintf("%s: OneDrive ignores emoji and other characters outside the Basic Multilingual Plane "+
				"when it compares names, so %q and %q are one name there; rename one of them", detail, name, other)
		}
	}

	return detail
}

// appendMultiGroupCollisions handles SkippedItem construction for groups with
// multiple events (event-vs-event collisions).
func appendMultiGroupCollisions(
	collisions []skippedItem,
	events []changeEvent,
	indices []int,
	childColliderSet map[int]struct{},
) []skippedItem {
	seen := make(map[string]bool, len(indices))
	for i, idx := range indices {
		if _, isChild := childColliderSet[idx]; isChild {
			continue
		}

		var others []string
		sameForm := false
		for j, otherIdx := range indices {
			if j == i {
				continue
			}
			if events[otherIdx].Path == events[idx].Path {
				sameForm = true
				continue
			}
			others = append(others, filepath.Base(events[otherIdx].Path))
		}

		if seen[events[idx].Path] {
			continue // the same normalized path is reported once
		}
		seen[events[idx].Path] = true

		detail := collisionDetail("conflicts with", filepath.Base(events[idx].Path), others)
		if sameForm {
			// Paths are NFC-normalized, so two files whose names differ only
			// in Unicode form (composed "é" vs "e" + combining accent) arrive
			// with identical paths: naming the other one would print the same
			// name twice.
			detail = "another file here has the same name in a different Unicode form (for example é typed as e plus an accent); rename one of them"
			if len(others) > 0 {
				detail += "; it also conflicts with " + strings.Join(others, ", ")
			}
		}

		collisions = append(collisions, skippedItem{
			Path:   events[idx].Path,
			Reason: issueCaseCollision,
			Detail: detail,
		})
	}

	return collisions
}

// detectDeletions finds baseline entries that were not observed during the
// walk, emitting ChangeDelete events for each.
func (o *localObserver) detectDeletions(observed map[string]bool) []changeEvent {
	var events []changeEvent

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

		if o.shouldSuppressDeleteForExcludedPath(path, entry) {
			return
		}

		events = append(events, changeEvent{
			Source:    SourceLocal,
			Type:      ChangeDelete,
			Path:      path,
			Name:      filepath.Base(path),
			ItemType:  entry.ItemType,
			Size:      entry.LocalSize,
			Mtime:     entry.LocalMtime,
			IsDeleted: true,
		})
	})

	return events
}

func (o *localObserver) shouldSuppressDeleteForExcludedPath(path string, entry *BaselineEntry) bool {
	if o.hasExcludedSymlinkAncestor(path) {
		return true
	}

	skip := shouldObserveWithFilter(
		filepath.Base(path),
		path,
		observedKindFromItemType(entry.ItemType),
		o.filterConfig,
		o.protectedRoots,
		o.observationRules,
	)

	return skip != nil && skip.Reason == ""
}

func observedKindFromItemType(itemType ItemType) observedKind {
	if itemType == ItemTypeFolder || itemType == ItemTypeRoot {
		return observedKindDir
	}

	return observedKindFile
}

// ---------------------------------------------------------------------------
// File hashing
// ---------------------------------------------------------------------------

// computeStableHash hashes a file and verifies it was not modified during the
// hash operation by comparing pre/post stat results. Returns ErrFileChangedDuringHash
// if the file changed (B-119). Caller-specific handling: handleWrite skips
// (Write events guarantee a follow-up), handleCreate and scanNewDirectory emit
// with empty hash (Create events and directory scans have no guaranteed follow-up; B-203).
//
// The double os.Stat is intentional: pre-stat captures baseline metadata,
// post-stat detects changes that occurred during hashing. The caller's earlier
// stat cannot substitute because time may pass between the caller's stat and
// the hash operation.
func computeStableHash(fsPath string) (string, error) {
	return computeStableHashWith(fsPath, driveops.ComputeQuickXorHash)
}

func computeStableHashWith(fsPath string, hashFunc func(string) (string, error)) (string, error) {
	pre, err := trustedStat(fsPath)
	if err != nil {
		return "", fmt.Errorf("stat before hashing: %w", err)
	}

	if hashFunc == nil {
		hashFunc = driveops.ComputeQuickXorHash
	}

	hash, err := hashFunc(fsPath)
	if err != nil {
		return "", fmt.Errorf("compute quickxor hash: %w", err)
	}

	post, err := trustedStat(fsPath)
	if err != nil {
		return "", fmt.Errorf("stat after hashing: %w", err)
	}

	if pre.Size() != post.Size() || pre.ModTime() != post.ModTime() {
		return "", errFileChangedDuringHash
	}

	return hash, nil
}

func trustedStat(path string) (os.FileInfo, error) {
	file, err := localpath.OpenRegular(path)
	if err != nil {
		// No path here: the *os.PathError already names it.
		return nil, fmt.Errorf("open trusted path: %w", err)
	}

	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		if closeErr != nil {
			return nil, errors.Join(statErr, closeErr)
		}

		return nil, fmt.Errorf("stat trusted path: %w", statErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("closing %s: %w", path, closeErr)
	}

	return info, nil
}

// ---------------------------------------------------------------------------
// Unified observation filter
// ---------------------------------------------------------------------------

// IsOversizedFile returns true if the file exceeds the OneDrive 250 GB size
// limit. Logs a debug message when skipping. This is Stage 2 of the two-stage
// observation filter — requires a stat result, so it runs after stat.
func (o *localObserver) IsOversizedFile(size int64, path string) bool {
	if size > maxOneDriveFileSize {
		o.logger.Debug("skipping oversized file",
			slog.String("path", path),
			slog.Int64("size", size))
		return true
	}
	return false
}

func shouldObserveWithFilter(
	name, path string,
	kind observedKind,
	filter ContentFilterConfig,
	protectedRoots []ProtectedRoot,
	rules LocalObservationRules,
) *skippedItem {
	normalizedPath := strings.TrimPrefix(filepath.ToSlash(path), "/")
	if _, found := protectedRootPathReservation(normalizedPath, protectedRoots); found {
		return &skippedItem{}
	}

	if !newContentFilter(filter).ShouldObserveLocalPath(path, kind) {
		return &skippedItem{}
	}

	if reason, detail := validateObservedName(name, path, rules); reason != "" {
		return &skippedItem{Path: path, Reason: reason, Detail: detail}
	}

	if pathLength := oneDrivePathLength(path); pathLength > maxOneDrivePathLength {
		return &skippedItem{
			Path:   path,
			Reason: issuePathTooLong,
			Detail: fmt.Sprintf("path length %d exceeds %d-character limit", pathLength, maxOneDrivePathLength),
		}
	}

	return nil
}

// isSpecialFile reports a non-directory that is not a regular file: a FIFO,
// socket or device. Symlinks are classified before this, on the link itself.
func isSpecialFile(info fs.FileInfo) bool {
	return !info.IsDir() && !info.Mode().IsRegular()
}

func dirEntryKind(d fs.DirEntry) observedKind {
	if d.IsDir() {
		return observedKindDir
	}

	return observedKindFile
}

// validateOneDriveName checks whether a filename is valid for OneDrive.
// Returns ("", "") for valid names. For invalid names, returns the issue
// type constant and a human-readable detail string. The detail never repeats
// the name: every caller prints it next to the path ("Not syncing a/CON: the
// name is reserved by Windows"), and %q-quoting the name again doubled any
// backslash in it.
//
// Checks (ordered by specificity): empty name, trailing dot/space, leading
// space, component length > 255, reserved device names, reserved patterns,
// invalid characters.
func validateOneDriveName(name string) (reason, detail string) {
	return validateOneDriveNameFor(name, false)
}

// validateOneDriveNameFor applies the name rules of the target drive type.
// personal relaxes the Business/SharePoint-only reservations (lockfiles,
// desktop.ini, "~$" owner files) that OneDrive Personal accepts.
func validateOneDriveNameFor(name string, personal bool) (reason, detail string) {
	if name == "" {
		return IssueInvalidFilename, "the name is empty"
	}

	// Graph would reinterpret invalid bytes (0xFF arrives as "ÿ"), and the
	// renamed remote item would then rename the local file to match.
	if !utf8.ValidString(name) {
		return IssueInvalidFilename, "the name is not valid UTF-8"
	}

	if containsControlChar(name) {
		return IssueInvalidFilename, "the name contains a control character (such as a tab or newline)"
	}

	if name[len(name)-1] == '.' {
		return IssueInvalidFilename, "the name ends with a period"
	}

	if name[len(name)-1] == ' ' {
		return IssueInvalidFilename, "the name ends with a space"
	}

	if name[0] == ' ' {
		return IssueInvalidFilename, "the name starts with a space"
	}

	if len(name) > maxComponentLength {
		return IssueInvalidFilename, fmt.Sprintf("the name is longer than %d characters", maxComponentLength)
	}

	lower := strings.ToLower(name)

	if isReservedDeviceName(lower) {
		return IssueInvalidFilename, "the name is reserved by Windows"
	}

	if strings.Contains(lower, "_vti_") {
		return IssueInvalidFilename, `OneDrive reserves names containing "_vti_"`
	}

	if !personal && isReservedPattern(name, lower) {
		return IssueInvalidFilename, "OneDrive for work or school reserves this name"
	}

	if containsInvalidChars(name) {
		return IssueInvalidFilename, `the name contains a character OneDrive does not allow (" * : < > ? / \ |)`
	}

	return "", ""
}

// ---------------------------------------------------------------------------
// Filtering and validation helpers
// ---------------------------------------------------------------------------

// syncRootExists returns true if the sync root directory exists and is a directory.
func syncRootExists(syncRoot string) bool {
	tree, err := synctree.Open(syncRoot)
	if err != nil {
		return false
	}

	info, err := tree.Stat(".")
	return err == nil && info.IsDir()
}

func validateObservedName(name, path string, rules LocalObservationRules) (reason, detail string) {
	if reason, detail := validateOneDriveNameFor(name, rules.PersonalNameRules); reason != "" {
		return reason, detail
	}

	if rules.RejectSharePointRootForms && isSharePointRootForms(name, path) {
		return IssueInvalidFilename, "the name is reserved at the root of a SharePoint library"
	}

	return "", ""
}

func isSharePointRootForms(name, path string) bool {
	return path == name && strings.EqualFold(name, "forms")
}

// isReservedDeviceName returns true for Windows reserved device names
// (case-insensitive): CON, PRN, AUX, NUL, COM0-COM9, LPT0-LPT9, with or
// without an extension. OneDrive rejects "con.txt" and "nul.tar.gz" exactly
// like "con", so only the part before the first period matters.
func isReservedDeviceName(lower string) bool {
	if base, _, found := strings.Cut(lower, "."); found {
		lower = base
	}

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

// containsControlChar reports C0 control characters and DEL, which Graph
// rejects in names.
func containsControlChar(name string) bool {
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return true
		}
	}

	return false
}

// isReservedPattern returns true for the names OneDrive for work or school
// reserves: .lock extension, desktop.ini, ~$ prefix (Office temp). "_vti_" is
// checked separately because every drive type rejects it.
func isReservedPattern(name, lower string) bool {
	if strings.HasSuffix(lower, ".lock") {
		return true
	}

	if lower == "desktop.ini" {
		return true
	}

	return strings.HasPrefix(name, "~$")
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

// buildLocalStateRows converts an observation scan into the rows local_state
// persists. It belongs to observation rather than the store: observation is
// what produces the snapshot, and putting the conversion in the store made the
// store's file family depend on an observation type.
func buildLocalStateRows(result scanResult) []localStateRow {
	rows := make([]localStateRow, len(result.Rows))
	copy(rows, result.Rows)
	return rows
}

// oneDrivePathLength counts a path the way OneDrive's 400-character limit
// does: in UTF-16 code units, the unit its error messages and its 260-unit
// emoji names measure (L-05). Counting UTF-8 bytes skipped valid paths: a
// 150-character CJK path is 450 bytes, and 300 "ä" are 600.
func oneDrivePathLength(path string) int {
	return len(utf16.Encode([]rune(path)))
}
