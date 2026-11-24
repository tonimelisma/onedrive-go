// The full scan's hash phase and the stable (double-stat) hash it shares
// with the watch-mode handlers.

package syncobserve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	stdsync "sync"

	"golang.org/x/sync/errgroup"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// defaultCheckWorkers is the default parallel hash goroutine limit when
// checkWorkers is zero (not configured).
const defaultCheckWorkers = 4

// hashJob describes a file that needs hashing during FullScan phase 2.
type hashJob struct {
	fsPath           string
	dbRelPath        string
	size             int64
	mtime            int64
	ctime            int64
	localDevice      uint64
	localInode       uint64
	localBirthTime   int64
	localHasIdentity bool
	isNew            bool // true for creates, false for modifies
}

// resolveCheckWorkers returns the effective check worker count.
func (o *LocalObserver) resolveCheckWorkers() int {
	if o.checkWorkers > 0 {
		return o.checkWorkers
	}

	return defaultCheckWorkers
}

// hashPhase runs hash jobs in parallel using errgroup with checkWorkers limit.
// Returns a row for every hashed file, an event for each one whose content
// differs from the baseline, plus any skipped items from panics in hash
// goroutines. Panics are recovered and
// converted to SkippedItem entries — a single corrupted file cannot crash the
// entire scan (defensive coding per eng philosophy).
//
//nolint:funlen // The hashing pipeline keeps local-scan concurrency and panic recovery in one explicit owner.
func (o *LocalObserver) hashPhase(
	ctx context.Context, jobs []hashJob,
) ([]syncdomain.LocalEntry, []syncdomain.LocalStateRow, []syncdomain.SkippedItem, error) {
	workers := o.resolveCheckWorkers()

	o.logger.Debug("starting parallel hash phase",
		slog.Int("jobs", len(jobs)),
		slog.Int("workers", workers),
	)

	hashFn := o.hooks.HashFile
	if hashFn == nil {
		hashFn = driveops.ComputeQuickXorHashContext
	}

	// mu guards the three result slices below. The walk fans out to workers
	// bounded by g.SetLimit, and each appends its own findings through skip and
	// keep, the only holders of mu, which release it with defer: a worker's
	// panic boundary records the file through skip, and would wait forever on
	// a lock a panic left held.
	var mu stdsync.Mutex
	var events []syncdomain.LocalEntry
	var rows []syncdomain.LocalStateRow
	var skipped []syncdomain.SkippedItem
	skip := func(item syncdomain.SkippedItem) {
		mu.Lock()
		defer mu.Unlock()
		skipped = append(skipped, item)
	}
	keep := func(ev *syncdomain.LocalEntry, row *syncdomain.LocalStateRow) {
		mu.Lock()
		defer mu.Unlock()
		if ev != nil {
			events = append(events, *ev)
		}
		rows = append(rows, *row)
	}

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(workers)
	recorder := newHashCacheRecorder(o.hashCache, o.logger)
	defer recorder.flush(ctx)

	for _, job := range jobs {
		g.Go(func() (retErr error) {
			// Recover from panics in hash computation (e.g., corrupt file
			// triggering a nil dereference in the hash library). Convert to
			// SkippedItem so the rest of the scan completes normally.
			defer func() {
				if r := recover(); r != nil {
					panicErr := panicguard.Recovered(o.logger, "while hashing "+job.dbRelPath, r,
						slog.String("path", job.dbRelPath))
					skip(syncdomain.SkippedItem{
						Path:   job.dbRelPath,
						Reason: syncdomain.IssueHashError,
						Detail: panicErr.Error(),
					})
				}
			}()

			if gCtx.Err() != nil {
				return gCtx.Err()
			}

			hash, err := hashFn(gCtx, job.fsPath)
			if err != nil {
				// Debug: this repeats on every safety scan for a file that
				// stats but cannot be opened. An unreadable file is recorded
				// as a skipped item instead, reported once and in `status`.
				o.logger.Debug("hash computation failed, emitting event with empty hash",
					slog.String("path", job.dbRelPath), slog.Any("error", err))
				switch {
				case errors.Is(err, os.ErrPermission):
					skip(syncdomain.SkippedItem{
						Path:   job.dbRelPath,
						Reason: syncdomain.IssueLocalReadDenied,
						Detail: "file not readable (check filesystem permissions)",
					})
				case !errors.Is(err, os.ErrNotExist):
					// An I/O error is not a content change: without the
					// issue the empty hash would read as "changed", and an
					// upload that failed the same way would be planned on
					// every pass (D-005). The issue makes the path
					// unavailable truth.
					skip(syncdomain.SkippedItem{
						Path:   job.dbRelPath,
						Reason: syncdomain.IssueHashError,
						Detail: fmt.Sprintf("could not read the file to check its content (%v)", err),
					})
				}
			}

			row := syncdomain.LocalStateRow{
				Path:             job.dbRelPath,
				ItemType:         syncdomain.ItemTypeFile,
				Hash:             hash,
				Size:             job.size,
				Mtime:            job.mtime,
				Ctime:            job.ctime,
				LocalDevice:      job.localDevice,
				LocalInode:       job.localInode,
				LocalBirthTime:   job.localBirthTime,
				LocalHasIdentity: job.localHasIdentity,
			}

			// For modifies: check if hash matches baseline (no real change).
			if !job.isNew && hash != "" {
				if existing, found := o.baseline.GetByPath(job.dbRelPath); found && hash == existing.LocalHash {
					keep(nil, &row)
					return nil
				}
			}

			recorder.addJob(gCtx, &job, hash)

			keep(&syncdomain.LocalEntry{Path: job.dbRelPath, ItemType: syncdomain.ItemTypeFile}, &row)

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, nil, fmt.Errorf("syncobserve: hash phase: %w", err)
	}

	return events, rows, skipped, nil
}

// withoutDroppedHashJobs removes the jobs of twins admitSyncedEntry set
// aside: a file twin's own job and every job below a folder twin.
func withoutDroppedHashJobs(jobs []hashJob, availability *scanAvailability) []hashJob {
	if len(availability.droppedHashJobs) == 0 && len(availability.droppedTrees) == 0 {
		return jobs
	}
	kept := jobs[:0]
	for i := range jobs {
		if !availability.hashJobDropped(jobs[i].fsPath) {
			kept = append(kept, jobs[i])
		}
	}

	return kept
}

// ---------------------------------------------------------------------------
// File hashing
// ---------------------------------------------------------------------------

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
func computeStableHashWith(
	ctx context.Context,
	fsPath string,
	hashFunc func(ctx context.Context, path string) (string, error),
) (string, error) {
	pre, err := trustedStat(fsPath)
	if err != nil {
		return "", fmt.Errorf("stat before hashing: %w", err)
	}

	if hashFunc == nil {
		hashFunc = driveops.ComputeQuickXorHashContext
	}

	hash, err := hashFunc(ctx, fsPath)
	if err != nil {
		return "", fmt.Errorf("compute quickxor hash: %w", err)
	}

	post, err := trustedStat(fsPath)
	if err != nil {
		return "", fmt.Errorf("stat after hashing: %w", err)
	}

	// ctime too: an in-place rewrite that restores the old mtime during the
	// read leaves size and mtime equal, and the hash would describe neither
	// version (R-2.2.4).
	if pre.Size() != post.Size() || pre.ModTime() != post.ModTime() || syncdomain.FileChangeTime(pre) != syncdomain.FileChangeTime(post) {
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

// errFileChangedDuringHash is returned when a file's metadata changes between
// pre-hash stat and post-hash stat, indicating active writing (B-119).
var errFileChangedDuringHash = errors.New("file changed while it was being hashed")
