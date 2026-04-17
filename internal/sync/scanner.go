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
//   - IsAlwaysExcluded:      OneDrive-incompatible name filtering
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
	slashpath "path"
	"path/filepath"
	"sort"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
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

type observedKind uint8

const (
	observedKindUnknown observedKind = iota
	observedKindFile
	observedKindDir
)

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
func (o *LocalObserver) FullScan(ctx context.Context, tree *synctree.Root) (ScanResult, error) {
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
		return ScanResult{}, ErrSyncRootMissing
	}

	// Guard: abort if .nosync file is present (sync dir may be unmounted).
	if _, err := tree.Stat(nosyncFileName); err == nil {
		o.Logger.Warn("nosync guard file detected, aborting scan",
			slog.String("sync_root", syncRoot))
		return ScanResult{}, ErrNosyncGuard
	}

	// Phase 1: Walk — collect observed paths, folder events, hash jobs, and skipped items.
	var events []ChangeEvent
	var jobs []hashJob
	var skipped []SkippedItem
	var skippedEntries atomic.Int64
	observed := make(map[string]bool)
	currentRows := make(map[string]LocalStateRow)
	scanStartNano := time.Now().UnixNano()
	dirStack := rootObservedDirStack(syncRoot, o.Logger)

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
			return ScanResult{}, fmt.Errorf("sync: local scan canceled: %w", ctx.Err())
		}

		return ScanResult{}, fmt.Errorf("sync: walking %s: %w", syncRoot, err)
	}

	if n := skippedEntries.Load(); n > 0 {
		o.Logger.Warn("full scan: skipped entries due to walk errors",
			slog.Int64("count", n),
			slog.String("sync_root", syncRoot))
	}

	// Phase 2: Hash — parallel file hashing. Panics in hash goroutines are
	// recovered and converted to SkippedItems (defensive coding).
	if len(jobs) > 0 {
		hashEvents, hashRows, hashSkipped, err := o.hashPhase(ctx, jobs)
		if err != nil {
			return ScanResult{}, err
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
	var caseSkipped []SkippedItem
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

	return ScanResult{
		Events:  events,
		Rows:    sortedLocalStateRows(currentRows),
		Skipped: skipped,
	}, nil
}

// hashPhase runs hash jobs in parallel using errgroup with checkWorkers limit.
// Returns the resulting change events (creates and modifies with hashes), plus
// any skipped items from panics in hash goroutines. Panics are recovered and
// converted to SkippedItem entries — a single corrupted file cannot crash the
// entire scan (defensive coding per eng philosophy).
func (o *LocalObserver) hashPhase(ctx context.Context, jobs []hashJob) ([]ChangeEvent, []LocalStateRow, []SkippedItem, error) {
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
	var events []ChangeEvent
	var rows []LocalStateRow
	var skipped []SkippedItem

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
					skipped = append(skipped, SkippedItem{
						Path:   job.dbRelPath,
						Reason: IssueHashPanic,
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
				if existing, found := o.Baseline.GetByPath(job.dbRelPath); found && hash == existing.LocalHash {
					mu.Lock()
					rows = append(rows, LocalStateRow{
						Path:            job.dbRelPath,
						ItemType:        ItemTypeFile,
						Hash:            hash,
						Size:            job.size,
						Mtime:           job.mtime,
						ContentIdentity: hash,
					})
					mu.Unlock()
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
			rows = append(rows, LocalStateRow{
				Path:            job.dbRelPath,
				ItemType:        itemType,
				Hash:            hash,
				Size:            job.size,
				Mtime:           job.mtime,
				ContentIdentity: hash,
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
func (o *LocalObserver) makeWalkFunc(
	ctx context.Context, tree *synctree.Root, observed map[string]bool, currentRows map[string]LocalStateRow,
	events *[]ChangeEvent, jobs *[]hashJob, skipped *[]SkippedItem,
	skippedEntries *atomic.Int64, scanStartNano int64, dirStack map[string]struct{},
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

		if d.Type()&fs.ModeSymlink != 0 {
			if o.filterConfig.SkipSymlinks {
				o.rememberExcludedSymlink(dbRelPath)
				observed[dbRelPath] = true
				o.Logger.Debug("skipping symlink", slog.String("path", dbRelPath))
				return SkipEntry(d)
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
		if skipItem := shouldObserveWithFilter(name, dbRelPath, dirEntryKind(d), o.filterConfig, o.observationRules); skipItem != nil {
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

		return o.processEntry(fsPath, dbRelPath, name, d, observed, currentRows, events, jobs, skipped, scanStartNano)
	}
}

// processEntry reads file info, marks the path as observed, and classifies
// the local change against the baseline. Folder events are appended to
// events directly; files that need hashing are appended to jobs for phase 2.
// Stage 2 observation filter: file size > 250GB is checked here (after stat).
func (o *LocalObserver) processEntry(
	fsPath, dbRelPath, name string, d fs.DirEntry, observed map[string]bool,
	currentRows map[string]LocalStateRow,
	events *[]ChangeEvent, jobs *[]hashJob, skipped *[]SkippedItem, scanStartNano int64,
) error {
	info, err := d.Info()
	if err != nil {
		// File disappeared between readdir and stat — skip and continue.
		o.Logger.Warn("stat failed (file may have disappeared)",
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

func (o *LocalObserver) processObservedInfo(
	fsPath, dbRelPath, name string,
	info fs.FileInfo,
	kind observedKind,
	observed map[string]bool,
	currentRows map[string]LocalStateRow,
	events *[]ChangeEvent,
	jobs *[]hashJob,
	skipped *[]SkippedItem,
	scanStartNano int64,
) error {
	// Stage 2 observation filter: file size check (requires stat, hence here).
	// FullScan records SkippedItems for oversized files; watch handlers don't
	// (the safety scan catches them).
	if kind == observedKindFile && o.IsOversizedFile(info.Size(), dbRelPath) {
		*skipped = append(*skipped, SkippedItem{
			Path:     dbRelPath,
			Reason:   IssueFileTooLarge,
			Detail:   fmt.Sprintf("file size %d bytes exceeds 250 GB limit", info.Size()),
			FileSize: info.Size(),
		})
		return nil
	}

	observed[dbRelPath] = true
	row := LocalStateRow{
		Path:       dbRelPath,
		Mtime:      info.ModTime().UnixNano(),
		ObservedAt: 0,
	}
	switch kind {
	case observedKindDir:
		row.ItemType = ItemTypeFolder
		row.Size = info.Size()
		currentRows[dbRelPath] = row
	default:
		row.ItemType = ItemTypeFile
	}

	return o.classifyObservedInfo(fsPath, dbRelPath, name, info, kind, currentRows, events, jobs, scanStartNano)
}

// classifyObservedInfo determines the change type for a single observed local
// entry by comparing it against the baseline. Folder events go directly to
// events; files that need hashing are appended to jobs for the parallel hash
// phase.
func (o *LocalObserver) classifyObservedInfo(
	fsPath, dbRelPath, name string,
	info fs.FileInfo,
	kind observedKind,
	currentRows map[string]LocalStateRow,
	events *[]ChangeEvent,
	jobs *[]hashJob,
	scanStartNano int64,
) error {
	var existing *BaselineEntry
	if baselineEntry, found := o.Baseline.GetByPath(dbRelPath); found {
		existing = baselineEntry
	}

	// No baseline entry — this is a new item.
	if existing == nil {
		if kind == observedKindDir {
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
	if kind == observedKindDir {
		return nil
	}

	return o.classifyFileChange(fsPath, dbRelPath, name, info, existing, currentRows, jobs, scanStartNano)
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
	currentRows map[string]LocalStateRow,
	jobs *[]hashJob, scanStartNano int64,
) error {
	currentMtime := info.ModTime().UnixNano()
	currentSize := info.Size()

	if CanReuseBaselineHash(info, base, scanStartNano) {
		o.Logger.Debug("fast path: mtime+size match, skipping hash",
			slog.String("path", dbRelPath))
		currentRows[dbRelPath] = LocalStateRow{
			Path:            dbRelPath,
			ItemType:        ItemTypeFile,
			Hash:            base.LocalHash,
			Size:            currentSize,
			Mtime:           currentMtime,
			ContentIdentity: base.LocalHash,
		}

		return nil
	}

	if base.LocalSizeKnown && currentSize == base.LocalSize && sameOneDriveComparableMtime(currentMtime, base.LocalMtime) {
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

func sortedLocalStateRows(current map[string]LocalStateRow) []LocalStateRow {
	paths := make([]string, 0, len(current))
	for path := range current {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	rows := make([]LocalStateRow, 0, len(paths))
	for _, path := range paths {
		rows = append(rows, current[path])
	}

	return rows
}

// DetectCaseCollisions finds events where two paths in the same directory
// differ only in case. Both colliders are removed from events and returned
// as SkippedItems. OneDrive uses a case-insensitive namespace — uploading
// both would cause one to silently overwrite the other (R-2.12.1).
//
// O(n) time, O(n) memory. Pure function — no side effects.
func DetectCaseCollisions(
	events []ChangeEvent, baseline *Baseline,
) (clean []ChangeEvent, collisions []SkippedItem) {
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
	clean = make([]ChangeEvent, 0, len(events)-len(colliderSet))
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
	events []ChangeEvent,
	groups map[caseGroupKey][]int,
	baseline *Baseline,
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
	events []ChangeEvent, colliderSet map[int]struct{},
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
	events []ChangeEvent,
	groups map[caseGroupKey][]int,
	colliderSet, childColliderSet map[int]struct{},
	collidingDirPrefixes []string,
	baseline *Baseline,
) []SkippedItem {
	collisions := make([]SkippedItem, 0, len(colliderSet))

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

		collisions = append(collisions, SkippedItem{
			Path:   ev.Path,
			Reason: IssueCaseCollision,
			Detail: fmt.Sprintf("parent directory %q has a case collision",
				filepath.Base(parentDir)),
		})
	}

	return collisions
}

// appendSingleGroupCollision handles SkippedItem construction for a group with
// exactly one event (flagged by baseline cross-check).
func appendSingleGroupCollision(
	collisions []SkippedItem,
	events []ChangeEvent,
	indices []int,
	colliderSet, childColliderSet map[int]struct{},
	baseline *Baseline,
) []SkippedItem {
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
			return append(collisions, SkippedItem{
				Path:   ev.Path,
				Reason: IssueCaseCollision,
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
	collisions []SkippedItem,
	events []ChangeEvent,
	indices []int,
	childColliderSet map[int]struct{},
) []SkippedItem {
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

		collisions = append(collisions, SkippedItem{
			Path:   events[idx].Path,
			Reason: IssueCaseCollision,
			Detail: fmt.Sprintf("conflicts with %s", strings.Join(others, ", ")),
		})
	}

	return collisions
}

// detectDeletions finds baseline entries that were not observed during the
// walk, emitting ChangeDelete events for each.
func (o *LocalObserver) detectDeletions(observed map[string]bool) []ChangeEvent {
	var events []ChangeEvent

	o.Baseline.ForEachPath(func(path string, entry *BaselineEntry) {
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

		events = append(events, ChangeEvent{
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

func (o *LocalObserver) shouldSuppressDeleteForExcludedPath(path string, entry *BaselineEntry) bool {
	if o.hasExcludedSymlinkAncestor(path) {
		return true
	}

	skip := shouldObserveWithFilter(
		filepath.Base(path),
		path,
		observedKindFromItemType(entry.ItemType),
		o.filterConfig,
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
		return "", ErrFileChangedDuringHash
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

func shouldObserveWithFilter(
	name, path string,
	kind observedKind,
	filter LocalFilterConfig,
	rules LocalObservationRules,
) *SkippedItem {
	if IsAlwaysExcluded(name) {
		return &SkippedItem{} // internal exclusion, not reportable
	}

	if shouldSkipConfiguredPath(name, path, kind, filter) {
		return &SkippedItem{}
	}

	if reason, detail := validateObservedName(name, path, rules); reason != "" {
		return &SkippedItem{Path: path, Reason: reason, Detail: detail}
	}

	if len(path) > MaxOneDrivePathLength {
		return &SkippedItem{
			Path:   path,
			Reason: IssuePathTooLong,
			Detail: fmt.Sprintf("path length %d exceeds %d-character limit", len(path), MaxOneDrivePathLength),
		}
	}

	return nil
}

func dirEntryKind(d fs.DirEntry) observedKind {
	if d.IsDir() {
		return observedKindDir
	}

	return observedKindFile
}

func shouldSkipConfiguredPath(
	name, path string,
	kind observedKind,
	filter LocalFilterConfig,
) bool {
	parts := observedPathParts(path)
	if len(parts) == 0 {
		return false
	}

	if filter.SkipDotfiles && hasDotfileComponent(parts) {
		return true
	}

	if hasSkippedParentDir(parts, filter.SkipDirs) {
		return true
	}

	if kind == observedKindDir && matchesConfiguredDir(name, filter.SkipDirs) {
		return true
	}

	if kind == observedKindFile && matchesConfiguredFile(name, path, filter.SkipFiles) {
		return true
	}

	return false
}

func observedPathParts(path string) []string {
	return strings.Split(path, "/")
}

func hasDotfileComponent(parts []string) bool {
	for _, part := range parts {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}

	return false
}

func hasSkippedParentDir(parts, skipDirs []string) bool {
	if len(skipDirs) == 0 || len(parts) < 2 {
		return false
	}

	for _, part := range parts[:len(parts)-1] {
		if matchesConfiguredDir(part, skipDirs) {
			return true
		}
	}

	return false
}

func matchesConfiguredDir(name string, skipDirs []string) bool {
	for _, skipDir := range skipDirs {
		if name == skipDir {
			return true
		}
	}

	return false
}

func matchesConfiguredFile(name, path string, skipFiles []string) bool {
	for _, pattern := range skipFiles {
		normalized := strings.TrimPrefix(filepath.ToSlash(pattern), "/")
		if normalized == "" {
			continue
		}

		if strings.Contains(normalized, "/") {
			if matched, err := slashpath.Match(normalized, path); err == nil && matched {
				return true
			}

			continue
		}

		if matched, err := slashpath.Match(normalized, name); err == nil && matched {
			return true
		}
	}

	return false
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
		return IssueInvalidFilename, "empty filename"
	}

	if name[len(name)-1] == '.' {
		return IssueInvalidFilename, fmt.Sprintf("filename %q ends with a period", name)
	}

	if name[len(name)-1] == ' ' {
		return IssueInvalidFilename, fmt.Sprintf("filename %q ends with a space", name)
	}

	if name[0] == ' ' {
		return IssueInvalidFilename, fmt.Sprintf("filename %q starts with a space", name)
	}

	if len(name) > maxComponentLength {
		return IssueInvalidFilename, fmt.Sprintf("filename %q exceeds %d-character component limit", name, maxComponentLength)
	}

	lower := strings.ToLower(name)

	if isReservedDeviceName(lower) {
		return IssueInvalidFilename, fmt.Sprintf("filename %q is a reserved Windows device name", name)
	}

	if isReservedPattern(name, lower) {
		return IssueInvalidFilename, fmt.Sprintf("filename %q matches a reserved OneDrive pattern", name)
	}

	if containsInvalidChars(name) {
		return IssueInvalidFilename, fmt.Sprintf("filename %q contains characters forbidden by OneDrive", name)
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

// IsAlwaysExcluded returns true for file patterns that must never enter local
// or remote snapshot state. These are symmetric junk/temp exclusions applied
// before snapshot persistence on either side.
//
// Called on every fsnotify event and every file during FullScan, so we use
// AsciiLower to avoid the heap allocation that strings.ToLower incurs per call.
// Suffixes are inlined as explicit checks — no slice allocation, no mutable
// package-level state, and the compiler inlines the string constants.
func IsAlwaysExcluded(name string) bool {
	lower := AsciiLower(name)

	// OS junk and archive detritus.
	if lower == ".ds_store" || lower == "thumbs.db" || lower == "__macosx" {
		return true
	}

	// Extension-based: partial downloads and editor temps.
	if strings.HasSuffix(lower, ".partial") ||
		strings.HasSuffix(lower, ".tmp") ||
		strings.HasSuffix(lower, ".swp") ||
		strings.HasSuffix(lower, ".crdownload") {
		return true
	}

	// Prefix-based: editor backup files (~file) and LibreOffice locks (.~lock).
	if strings.HasPrefix(name, ".~") {
		return true
	}

	if strings.HasPrefix(name, "._") {
		return true
	}

	if strings.HasPrefix(name, "~") && !strings.HasPrefix(name, "~$") {
		return true
	}

	return false
}

func validateObservedName(name, path string, rules LocalObservationRules) (reason, detail string) {
	if reason, detail := ValidateOneDriveName(name); reason != "" {
		return reason, detail
	}

	if rules.RejectSharePointRootForms && isSharePointRootForms(name, path) {
		return IssueInvalidFilename, fmt.Sprintf("name %q is reserved at the root of a SharePoint library", name)
	}

	return "", ""
}

func isSharePointRootForms(name, path string) bool {
	return path == name && strings.EqualFold(name, "forms")
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
