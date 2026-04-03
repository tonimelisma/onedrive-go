// Package syncobserve watches and scans the local filesystem for sync changes.
//
// Contents:
//   - FullScan:              orchestrates walk + hash phases → ScanResult
//   - hashPhase:             parallel hash computation for discovered files
//   - makeWalkFunc:          builds the filepath.WalkDir callback
//   - classifyLocalChange:   compares local state against baseline
//   - detectDeletions:       finds baseline entries missing from walk
//   - ComputeStableHash:     double-stat hash for actively-written files
//   - ShouldObserve:         unified observation filter (Stage 1: name + path)
//   - IsOversizedFile:       Stage 2 observation filter (file size > 250GB)
//   - ValidateOneDriveName:  returns reason + detail for invalid names
//   - IsAlwaysExcluded:      OneDrive-incompatible name filtering
//
// Related files:
//   - observer_local.go:           LocalObserver struct and Watch() entry point
//   - observer_local_handlers.go:  watch-mode event handlers
package syncobserve

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
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Constants for the local scanner.
const (
	nosyncFileName         = ".nosync"
	nanosPerSecond         = 1_000_000_000
	maxComponentLength     = 255
	deviceNameWithDigitLen = 4 // COM0-COM9, LPT0-LPT9 have exactly 4 characters

	// MaxOneDrivePathLength is the maximum total path length OneDrive allows.
	MaxOneDrivePathLength = 400
	// MaxOneDriveFileSize is the maximum file size OneDrive allows (250 GB).
	MaxOneDriveFileSize = driveops.MaxOneDriveFileSize
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

// FullScan walks the sync root directory and returns a ScanResult containing
// change events for all local changes (creates, modifies, deletes) relative
// to the baseline, plus any skipped items that should be recorded as
// actionable failures.
//
// Three-phase design:
//  1. Walk (sequential): collect observed map, emit folder creates, classify
//     files that need hashing into a hashJob slice. Collects SkippedItems
//     for invalid names, too-long paths, and too-large files.
//  2. Hash (parallel): errgroup.SetLimit(checkWorkers) hashes files concurrently.
//  3. Deletion detection (sequential): compare observed vs baseline.
func (o *LocalObserver) FullScan(ctx context.Context, tree *synctree.Root) (synctypes.ScanResult, error) {
	syncRoot := tree.Path()
	o.Logger.Info("local observer starting full scan",
		slog.String("sync_root", syncRoot),
		slog.Int("baseline_entries", o.Baseline.Len()),
	)

	// Guard: abort if the sync root directory does not exist. Without this,
	// WalkDir silently succeeds with zero events (walkFn's SkipEntry returns
	// filepath.SkipDir for the root error, so WalkDir returns nil).
	if !SyncRootExists(syncRoot) {
		o.Logger.Warn("sync root missing, aborting scan",
			slog.String("sync_root", syncRoot))
		return synctypes.ScanResult{}, synctypes.ErrSyncRootMissing
	}

	// Guard: abort if .nosync file is present (sync dir may be unmounted).
	if _, err := tree.Stat(nosyncFileName); err == nil {
		o.Logger.Warn("nosync guard file detected, aborting scan",
			slog.String("sync_root", syncRoot))
		return synctypes.ScanResult{}, synctypes.ErrNosyncGuard
	}

	// Phase 1: Walk — collect observed paths, folder events, hash jobs, and skipped items.
	var events []synctypes.ChangeEvent
	var jobs []hashJob
	var skipped []synctypes.SkippedItem
	var skippedEntries atomic.Int64
	observed := make(map[string]bool)
	scanStartNano := time.Now().UnixNano()

	walkFn := o.makeWalkFunc(ctx, tree, observed, &events, &jobs, &skipped, &skippedEntries, scanStartNano)
	if err := tree.WalkDir(walkFn); err != nil {
		if ctx.Err() != nil {
			return synctypes.ScanResult{}, fmt.Errorf("sync: local scan canceled: %w", ctx.Err())
		}

		return synctypes.ScanResult{}, fmt.Errorf("sync: walking %s: %w", syncRoot, err)
	}

	if n := skippedEntries.Load(); n > 0 {
		o.Logger.Warn("full scan: skipped entries due to walk errors",
			slog.Int64("count", n),
			slog.String("sync_root", syncRoot))
	}

	// Phase 2: Hash — parallel file hashing. Panics in hash goroutines are
	// recovered and converted to SkippedItems (defensive coding).
	if len(jobs) > 0 {
		hashEvents, hashSkipped, err := o.hashPhase(ctx, jobs)
		if err != nil {
			return synctypes.ScanResult{}, err
		}

		events = append(events, hashEvents...)
		skipped = append(skipped, hashSkipped...)
	}

	// Phase 2.5: Case collision detection — run after hashing (events finalized)
	// but before deletion detection. Colliding files stay in the observed map
	// (set in Phase 1) to prevent Phase 3 from generating spurious ChangeDelete
	// events for files that exist locally but were excluded from events (R-2.12.1).
	var caseSkipped []synctypes.SkippedItem
	events, caseSkipped = DetectCaseCollisions(events, o.Baseline)
	skipped = append(skipped, caseSkipped...)

	// Phase 3: Deletion detection.
	deletions := o.detectDeletions(observed)
	events = append(events, deletions...)

	o.Logger.Debug("deletion detection complete",
		slog.Int("deletions", len(deletions)),
		slog.Int("baseline_entries", o.Baseline.Len()),
		slog.Int("observed", len(observed)),
	)

	o.Logger.Info("local observer completed full scan",
		slog.Int("events", len(events)),
		slog.Int("observed", len(observed)),
		slog.Int("hashed", len(jobs)),
		slog.Int("skipped", len(skipped)),
	)

	if len(events) > 0 {
		o.recordActivity()
	}

	return synctypes.ScanResult{Events: events, Skipped: skipped}, nil
}

// hashPhase runs hash jobs in parallel using errgroup with checkWorkers limit.
// Returns the resulting change events (creates and modifies with hashes), plus
// any skipped items from panics in hash goroutines. Panics are recovered and
// converted to SkippedItem entries — a single corrupted file cannot crash the
// entire scan (defensive coding per eng philosophy).
func (o *LocalObserver) hashPhase(ctx context.Context, jobs []hashJob) ([]synctypes.ChangeEvent, []synctypes.SkippedItem, error) {
	workers := o.resolveCheckWorkers()

	o.Logger.Debug("starting parallel hash phase",
		slog.Int("jobs", len(jobs)),
		slog.Int("workers", workers),
	)

	hashFn := o.HashFunc
	if hashFn == nil {
		hashFn = driveops.ComputeQuickXorHash
	}

	var mu stdsync.Mutex
	var events []synctypes.ChangeEvent
	var skipped []synctypes.SkippedItem

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(workers)

	for _, job := range jobs {
		g.Go(func() (retErr error) {
			// Recover from panics in hash computation (e.g., corrupt file
			// triggering a nil dereference in the hash library). Convert to
			// SkippedItem so the rest of the scan completes normally.
			defer func() {
				if r := recover(); r != nil {
					o.Logger.Error("hash phase: panic in worker",
						slog.String("path", job.dbRelPath),
						slog.Any("panic", r),
					)

					mu.Lock()
					skipped = append(skipped, synctypes.SkippedItem{
						Path:   job.dbRelPath,
						Reason: synctypes.IssueHashPanic,
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
				o.Logger.Warn("hash computation failed, emitting event with empty hash",
					slog.String("path", job.dbRelPath), slog.String("error", err.Error()))
			}

			// For modifies: check if hash matches baseline (no real change).
			if !job.isNew && hash != "" {
				existing, _ := o.Baseline.GetByPath(job.dbRelPath)
				if existing != nil && hash == existing.LocalHash {
					return nil
				}
			}

			changeType := synctypes.ChangeCreate
			itemType := synctypes.ItemTypeFile
			if !job.isNew {
				changeType = synctypes.ChangeModify
			}

			ev := synctypes.ChangeEvent{
				Source:   synctypes.SourceLocal,
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
		return nil, nil, fmt.Errorf("sync: hash phase: %w", err)
	}

	return events, skipped, nil
}

// makeWalkFunc returns a WalkDirFunc that classifies filesystem entries
// against the baseline. Folder events are appended to events directly.
// Files that need hashing are appended to jobs for phase 2. User-actionable
// rejections are appended to skipped for engine recording.
func (o *LocalObserver) makeWalkFunc(
	ctx context.Context, tree *synctree.Root, observed map[string]bool,
	events *[]synctypes.ChangeEvent, jobs *[]hashJob, skipped *[]synctypes.SkippedItem,
	skippedEntries *atomic.Int64, scanStartNano int64,
) fs.WalkDirFunc {
	syncRoot := tree.Path()

	return func(fsPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			o.Logger.Warn("walk error", slog.String("path", fsPath), slog.String("error", walkErr.Error()))
			skippedEntries.Add(1)
			return SkipEntry(d)
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

		// Symlinks are never synced, not a naming issue — skip silently.
		if d.Type()&fs.ModeSymlink != 0 {
			o.Logger.Debug("skipping symlink", slog.String("path", dbRelPath))
			return SkipEntry(d)
		}

		// Stage 1 observation filter: name validation + path length (cheap, no syscall).
		if skipItem := ShouldObserve(name, dbRelPath); skipItem != nil {
			if skipItem.Reason != "" {
				*skipped = append(*skipped, *skipItem)
				o.Logger.Debug("skipping invalid entry",
					slog.String("path", dbRelPath),
					slog.String("reason", skipItem.Reason))
			} else {
				o.Logger.Debug("skipping excluded file", slog.String("name", name))
			}

			return SkipEntry(d)
		}

		return o.processEntry(fsPath, dbRelPath, name, d, observed, events, jobs, skipped, scanStartNano)
	}
}

// processEntry reads file info, marks the path as observed, and classifies
// the local change against the baseline. Folder events are appended to
// events directly; files that need hashing are appended to jobs for phase 2.
// Stage 2 observation filter: file size > 250GB is checked here (after stat).
func (o *LocalObserver) processEntry(
	fsPath, dbRelPath, name string, d fs.DirEntry, observed map[string]bool,
	events *[]synctypes.ChangeEvent, jobs *[]hashJob, skipped *[]synctypes.SkippedItem, scanStartNano int64,
) error {
	info, err := d.Info()
	if err != nil {
		// File disappeared between readdir and stat — skip and continue.
		o.Logger.Warn("stat failed (file may have disappeared)",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))
		return nil
	}

	// Stage 2 observation filter: file size check (requires stat, hence here).
	// FullScan records SkippedItems for oversized files; watch handlers don't
	// (the safety scan catches them).
	if !d.IsDir() && o.IsOversizedFile(info.Size(), dbRelPath) {
		*skipped = append(*skipped, synctypes.SkippedItem{
			Path:     dbRelPath,
			Reason:   synctypes.IssueFileTooLarge,
			Detail:   fmt.Sprintf("file size %d bytes exceeds 250 GB limit", info.Size()),
			FileSize: info.Size(),
		})
		return nil // skip — don't create event or hash job
	}

	observed[dbRelPath] = true

	return o.classifyLocalChange(fsPath, dbRelPath, name, d, info, events, jobs, scanStartNano)
}

// classifyLocalChange determines the change type for a single local entry
// by comparing it against the baseline. Folder events go directly to events;
// files that need hashing are appended to jobs for the parallel hash phase.
func (o *LocalObserver) classifyLocalChange(
	fsPath, dbRelPath, name string, d fs.DirEntry, info fs.FileInfo,
	events *[]synctypes.ChangeEvent, jobs *[]hashJob, scanStartNano int64,
) error {
	existing, _ := o.Baseline.GetByPath(dbRelPath)

	// No baseline entry — this is a new item.
	if existing == nil {
		if d.IsDir() {
			// Folder creates go directly to events (no hashing needed).
			*events = append(*events, synctypes.ChangeEvent{
				Source:   synctypes.SourceLocal,
				Type:     synctypes.ChangeCreate,
				Path:     dbRelPath,
				Name:     name,
				ItemType: synctypes.ItemTypeFolder,
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
	fsPath, dbRelPath, name string, info fs.FileInfo, base *synctypes.BaselineEntry,
	jobs *[]hashJob, scanStartNano int64,
) error {
	currentMtime := info.ModTime().UnixNano()
	currentSize := info.Size()

	if CanReuseBaselineHash(info, base, scanStartNano) {
		o.Logger.Debug("fast path: mtime+size match, skipping hash",
			slog.String("path", dbRelPath))

		return nil
	}

	if currentSize == base.Size && currentMtime == base.Mtime {
		o.Logger.Debug("racily clean file, forcing hash check",
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

// DetectCaseCollisions finds events where two paths in the same directory
// differ only in case. Both colliders are removed from events and returned
// as SkippedItems. OneDrive uses a case-insensitive namespace — uploading
// both would cause one to silently overwrite the other (R-2.12.1).
//
// O(n) time, O(n) memory. Pure function — no side effects.
func DetectCaseCollisions(
	events []synctypes.ChangeEvent, baseline *synctypes.Baseline,
) (clean []synctypes.ChangeEvent, collisions []synctypes.SkippedItem) {
	if len(events) == 0 {
		return nil, nil
	}

	// Group event indices by (directory, lowercase name).
	groups := make(map[caseGroupKey][]int, len(events))
	for i := range events {
		dir := filepath.Dir(events[i].Path)
		lowName := strings.ToLower(filepath.Base(events[i].Path))
		key := caseGroupKey{dir: dir, lowName: lowName}
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
	crossCheckBaseline(events, groups, baseline, colliderSet)

	// Suppress children of colliding directories.
	childColliderSet, collidingDirPrefixes := suppressDirectoryChildren(events, colliderSet)

	if len(colliderSet) == 0 {
		return events, nil
	}

	// Build SkippedItems with Detail naming the other collider(s).
	collisions = buildCollisionSkippedItems(
		events, groups, colliderSet, childColliderSet, collidingDirPrefixes, baseline)

	// Build clean events — those not in the collider set.
	clean = make([]synctypes.ChangeEvent, 0, len(events)-len(colliderSet))
	for i := range events {
		if _, collider := colliderSet[i]; !collider {
			clean = append(clean, events[i])
		}
	}

	return clean, collisions
}

// caseGroupKey groups events by (directory, lowercase name) for case collision detection.
type caseGroupKey struct {
	dir     string
	lowName string
}

// crossCheckBaseline flags single-event groups that collide with already-synced
// baseline entries. A new file whose lowercased name matches a baseline entry
// with different exact casing is a collision (the baseline file produced no event
// because it was unchanged — fast-path skip in classifyFileChange).
func crossCheckBaseline(
	events []synctypes.ChangeEvent,
	groups map[caseGroupKey][]int,
	baseline *synctypes.Baseline,
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
			if v.Path != ev.Path {
				colliderSet[indices[0]] = struct{}{}

				break
			}
		}
	}
}

// suppressDirectoryChildren marks children of colliding directories as colliders.
// They can't be uploaded to a folder that won't exist on OneDrive.
func suppressDirectoryChildren(
	events []synctypes.ChangeEvent, colliderSet map[int]struct{},
) (childColliderSet map[int]struct{}, collidingDirPrefixes []string) {
	childColliderSet = make(map[int]struct{})

	for idx := range colliderSet {
		if events[idx].ItemType == synctypes.ItemTypeFolder {
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
	events []synctypes.ChangeEvent,
	groups map[caseGroupKey][]int,
	colliderSet, childColliderSet map[int]struct{},
	collidingDirPrefixes []string,
	baseline *synctypes.Baseline,
) []synctypes.SkippedItem {
	collisions := make([]synctypes.SkippedItem, 0, len(colliderSet))

	// Event-vs-event and baseline collisions.
	for _, indices := range groups {
		if len(indices) <= 1 {
			collisions = appendSingleGroupCollision(
				collisions, events, indices, colliderSet, childColliderSet, baseline)

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

		collisions = append(collisions, synctypes.SkippedItem{
			Path:   ev.Path,
			Reason: synctypes.IssueCaseCollision,
			Detail: fmt.Sprintf("parent directory %q has a case collision",
				filepath.Base(parentDir)),
		})
	}

	return collisions
}

// appendSingleGroupCollision handles SkippedItem construction for a group with
// exactly one event (flagged by baseline cross-check).
func appendSingleGroupCollision(
	collisions []synctypes.SkippedItem,
	events []synctypes.ChangeEvent,
	indices []int,
	colliderSet, childColliderSet map[int]struct{},
	baseline *synctypes.Baseline,
) []synctypes.SkippedItem {
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
		if v.Path != ev.Path {
			return append(collisions, synctypes.SkippedItem{
				Path:   ev.Path,
				Reason: synctypes.IssueCaseCollision,
				Detail: fmt.Sprintf("conflicts with synced file %s",
					filepath.Base(v.Path)),
			})
		}
	}

	return collisions
}

// appendMultiGroupCollisions handles SkippedItem construction for groups with
// multiple events (event-vs-event collisions).
func appendMultiGroupCollisions(
	collisions []synctypes.SkippedItem,
	events []synctypes.ChangeEvent,
	indices []int,
	childColliderSet map[int]struct{},
) []synctypes.SkippedItem {
	for i, idx := range indices {
		if _, isChild := childColliderSet[idx]; isChild {
			continue
		}

		var others []string
		for j, otherIdx := range indices {
			if j != i {
				others = append(others, filepath.Base(events[otherIdx].Path))
			}
		}

		collisions = append(collisions, synctypes.SkippedItem{
			Path:   events[idx].Path,
			Reason: synctypes.IssueCaseCollision,
			Detail: fmt.Sprintf("conflicts with %s", strings.Join(others, ", ")),
		})
	}

	return collisions
}

// detectDeletions finds baseline entries that were not observed during the
// walk, emitting ChangeDelete events for each.
func (o *LocalObserver) detectDeletions(observed map[string]bool) []synctypes.ChangeEvent {
	var events []synctypes.ChangeEvent

	o.Baseline.ForEachPath(func(path string, entry *synctypes.BaselineEntry) {
		if path == "" {
			return
		}

		if entry.ItemType == synctypes.ItemTypeRoot {
			return
		}

		if observed[path] {
			return
		}

		events = append(events, synctypes.ChangeEvent{
			Source:    synctypes.SourceLocal,
			Type:      synctypes.ChangeDelete,
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

// ComputeStableHash hashes a file and verifies it was not modified during the
// hash operation by comparing pre/post stat results. Returns ErrFileChangedDuringHash
// if the file changed (B-119). Caller-specific handling: handleWrite skips
// (Write events guarantee a follow-up), handleCreate and scanNewDirectory emit
// with empty hash (Create events and directory scans have no guaranteed follow-up; B-203).
//
// The double os.Stat is intentional: pre-stat captures baseline metadata,
// post-stat detects changes that occurred during hashing. The caller's earlier
// stat cannot substitute because time may pass between the caller's stat and
// the hash operation.
func ComputeStableHash(fsPath string) (string, error) {
	return computeStableHashWith(fsPath, driveops.ComputeQuickXorHash)
}

func computeStableHashWith(fsPath string, hashFunc func(string) (string, error)) (string, error) {
	pre, err := trustedStat(fsPath)
	if err != nil {
		return "", fmt.Errorf("sync: pre-hash stat %s: %w", fsPath, err)
	}

	if hashFunc == nil {
		hashFunc = driveops.ComputeQuickXorHash
	}

	hash, err := hashFunc(fsPath)
	if err != nil {
		return "", fmt.Errorf("compute quickxor hash %s: %w", fsPath, err)
	}

	post, err := trustedStat(fsPath)
	if err != nil {
		return "", fmt.Errorf("sync: post-hash stat %s: %w", fsPath, err)
	}

	if pre.Size() != post.Size() || pre.ModTime() != post.ModTime() {
		return "", synctypes.ErrFileChangedDuringHash
	}

	return hash, nil
}

func trustedStat(path string) (os.FileInfo, error) {
	file, err := localpath.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open trusted path %s: %w", path, err)
	}

	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		if closeErr != nil {
			return nil, errors.Join(statErr, closeErr)
		}

		return nil, fmt.Errorf("stat %s: %w", path, statErr)
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
func (o *LocalObserver) IsOversizedFile(size int64, path string) bool {
	if size > MaxOneDriveFileSize {
		o.Logger.Debug("skipping oversized file",
			slog.String("path", path),
			slog.Int64("size", size))
		return true
	}
	return false
}

// ShouldObserve checks whether a local filesystem entry should enter the sync
// pipeline. Returns nil for valid entries (observe). Returns a non-nil
// *SkippedItem for rejected entries: Reason=="" for internal exclusions
// (temp files — not user-actionable), Reason!="" for user-actionable
// rejections that should be recorded.
//
// Called from FullScan walk, watch event handlers, and watch setup.
// Expects NFC-normalized name and path. This is Stage 1 of the two-stage
// observation filter — cheap string checks only (no syscall). Stage 2
// (file size > 250GB) is checked after stat in processEntry/hashAndEmit.
func ShouldObserve(name, path string) *synctypes.SkippedItem {
	if IsAlwaysExcluded(name) {
		return &synctypes.SkippedItem{} // internal exclusion, not reportable
	}

	if reason, detail := ValidateOneDriveName(name); reason != "" {
		return &synctypes.SkippedItem{Path: path, Reason: reason, Detail: detail}
	}

	if len(path) > MaxOneDrivePathLength {
		return &synctypes.SkippedItem{
			Path:   path,
			Reason: synctypes.IssuePathTooLong,
			Detail: fmt.Sprintf("path length %d exceeds %d-character limit", len(path), MaxOneDrivePathLength),
		}
	}

	return nil
}

// ValidateOneDriveName checks whether a filename is valid for OneDrive.
// Returns ("", "") for valid names. For invalid names, returns the issue
// type constant and a human-readable detail string.
//
// Checks (ordered by specificity): empty name, trailing dot/space, leading
// space, component length > 255, reserved device names, reserved patterns,
// invalid characters.
func ValidateOneDriveName(name string) (reason, detail string) {
	if name == "" {
		return synctypes.IssueInvalidFilename, "empty filename"
	}

	if name[len(name)-1] == '.' {
		return synctypes.IssueInvalidFilename, fmt.Sprintf("filename %q ends with a period", name)
	}

	if name[len(name)-1] == ' ' {
		return synctypes.IssueInvalidFilename, fmt.Sprintf("filename %q ends with a space", name)
	}

	if name[0] == ' ' {
		return synctypes.IssueInvalidFilename, fmt.Sprintf("filename %q starts with a space", name)
	}

	if len(name) > maxComponentLength {
		return synctypes.IssueInvalidFilename, fmt.Sprintf("filename %q exceeds %d-character component limit", name, maxComponentLength)
	}

	lower := strings.ToLower(name)

	if isReservedDeviceName(lower) {
		return synctypes.IssueInvalidFilename, fmt.Sprintf("filename %q is a reserved Windows device name", name)
	}

	if isReservedPattern(name, lower) {
		return synctypes.IssueInvalidFilename, fmt.Sprintf("filename %q matches a reserved OneDrive pattern", name)
	}

	if containsInvalidChars(name) {
		return synctypes.IssueInvalidFilename, fmt.Sprintf("filename %q contains characters forbidden by OneDrive", name)
	}

	return "", ""
}

// ---------------------------------------------------------------------------
// Filtering and validation helpers
// ---------------------------------------------------------------------------

// SyncRootExists returns true if the sync root directory exists and is a directory.
func SyncRootExists(syncRoot string) bool {
	tree, err := synctree.Open(syncRoot)
	if err != nil {
		return false
	}

	info, err := tree.Stat(".")
	return err == nil && info.IsDir()
}

// IsAlwaysExcluded returns true for file patterns that must never be synced.
// These are S7 safety invariants: partial downloads and editor temporaries.
//
// Called on every fsnotify event and every file during FullScan, so we use
// AsciiLower to avoid the heap allocation that strings.ToLower incurs per call.
// Suffixes are inlined as explicit checks — no slice allocation, no mutable
// package-level state, and the compiler inlines the string constants.
func IsAlwaysExcluded(name string) bool {
	lower := AsciiLower(name)

	// Extension-based: partial downloads and editor temps.
	if strings.HasSuffix(lower, ".partial") ||
		strings.HasSuffix(lower, ".tmp") ||
		strings.HasSuffix(lower, ".swp") ||
		strings.HasSuffix(lower, ".crdownload") {
		return true
	}

	// Prefix-based: editor backup files (~file) and LibreOffice locks (.~lock).
	if strings.HasPrefix(name, "~") || strings.HasPrefix(name, ".~") {
		return true
	}

	return false
}

// AsciiLower returns s with ASCII uppercase letters converted to lowercase.
// Unlike strings.ToLower, this avoids heap allocation when s is already
// lowercase (the common case for filenames). Non-ASCII bytes are passed through
// unchanged, which is correct for file extension matching.
func AsciiLower(s string) string {
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

// SkipEntry returns filepath.SkipDir for directories (to skip the subtree)
// or nil for files (to continue the walk with the next entry).
func SkipEntry(d fs.DirEntry) error {
	if d != nil && d.IsDir() {
		return filepath.SkipDir
	}

	return nil
}
