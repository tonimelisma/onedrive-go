package driveops

import (
	"context"
	"log/slog"
	"math"
	"os"
	"strings"
	gosync "sync"
	"sync/atomic"
	"time"
)

// TransferManager provides unified download/upload with resume, shared between
// the CLI (files.go) and the sync engine (executor_transfer.go). Handles
// .partial files, range-based resume, hash verification with retry, session
// persistence for uploads, disk space pre-checks, and atomic rename.
type TransferManager struct {
	downloads    Downloader
	uploads      Uploader
	sessionStore *SessionStore // nil = no session persistence for uploads
	logger       *slog.Logger
	// hashFunc hashes a local file before an upload. It takes the upload's
	// context so a canceled upload or a shutdown does not wait out a
	// whole-file read (R-6.3.6).
	hashFunc func(ctx context.Context, fsPath string) (string, error)
	// syncFile flushes a finalized download to disk before it is renamed
	// into place. It is (*os.File).Sync in production; a seam only because
	// TargetFS hands out real files, so nothing else can observe the fsync.
	syncFile         func(*os.File) error
	sessionMountID   string
	sessionLocalRoot string
	// now is the clock a recorded session's expiry is judged on. It is the
	// wall clock unless the owner runs on a clock of its own (WithClock):
	// the sync engine passes its run clock, so the instant a session record
	// expires at moves with the time a test gives the engine.
	now func() time.Time

	// minFreeSpace is the minimum free disk space (bytes) required before
	// downloads. Zero disables the check (R-6.4.7). Configured via WithDiskCheck.
	minFreeSpace int64

	// diskAvailableFunc queries available disk space. nil = skip disk check.
	// Injected via WithDiskCheck to allow testing without real statfs calls.
	diskAvailableFunc func(string) (uint64, error)

	// diskCheckFailureWarned is set once the first failed disk-space check
	// has been warned about, so a persistent statfs failure does not warn on
	// every download. Atomic because concurrent workers share one manager.
	diskCheckFailureWarned atomic.Bool

	// umaskOnce guards umask, the process umask probed on first use for
	// the mode of newly created downloads.
	umaskOnce gosync.Once
	umask     os.FileMode

	// partialsMu guards heldPartials and singleUsePartials. heldPartials
	// names the partial files a download write abandoned on a hung disk
	// still holds (partialSink): no download writes, resumes or truncates
	// one until that write has returned. singleUsePartials numbers the
	// partials of another name the next attempts write instead.
	partialsMu        gosync.Mutex
	heldPartials      map[string]struct{}
	singleUsePartials uint64
}

// Option configures a TransferManager. Pass to NewTransferManager.
type Option func(*TransferManager)

// WithDiskCheck enables disk space pre-checks before each download (R-6.2.6).
// minFreeSpace is the minimum free space in bytes; zero disables the check
// (R-6.4.7). fn queries available disk space for a path — typically DiskAvailable.
func WithDiskCheck(minFreeSpace int64, fn func(string) (uint64, error)) Option {
	return func(tm *TransferManager) {
		tm.minFreeSpace = minFreeSpace
		tm.diskAvailableFunc = fn
	}
}

// WithSessionScope tags newly persisted upload sessions with the mount that
// owns them, allowing lifecycle owners to purge orphaned child sessions without
// touching explicit mounts that happen to target the same drive.
func WithSessionScope(mountID, localRoot string) Option {
	return func(tm *TransferManager) {
		tm.sessionMountID = strings.TrimSpace(mountID)
		tm.sessionLocalRoot = strings.TrimSpace(localRoot)
	}
}

// WithContentHasher replaces the pre-upload content hasher. The sync engine's
// tests inject a hasher that blocks until its context is canceled to prove
// that stopping the worker pool does not wait on a whole-file read.
func WithContentHasher(fn func(ctx context.Context, fsPath string) (string, error)) Option {
	return func(tm *TransferManager) {
		tm.hashFunc = fn
	}
}

// WithClock makes now the clock the manager judges a recorded upload
// session's expiry on. The sync engine passes its run clock.
func WithClock(now func() time.Time) Option {
	return func(tm *TransferManager) {
		tm.now = now
	}
}

// NewTransferManager creates a TransferManager. sessionStore may be nil if
// upload session persistence is not needed (e.g., small-file-only workflows).
// Options configure optional behaviors like disk space checking.
func NewTransferManager(
	dl Downloader, ul Uploader, store *SessionStore, logger *slog.Logger, opts ...Option,
) *TransferManager {
	tm := &TransferManager{
		downloads:    dl,
		uploads:      ul,
		sessionStore: store,
		logger:       logger,
		hashFunc:     ComputeQuickXorHashContext,
		syncFile:     (*os.File).Sync,
		now:          time.Now,
	}

	for _, opt := range opts {
		opt(tm)
	}

	return tm
}

// clampToInt64 converts a statfs byte count for humanunit.Size. Free space beyond
// int64 range (8 EiB) does not occur on real filesystems.
func clampToInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}

	return int64(v)
}
