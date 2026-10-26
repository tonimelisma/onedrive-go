package driveops

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	gosync "sync"
	"sync/atomic"
)

// TransferManager provides unified download/upload with resume, shared between
// the CLI (files.go) and the sync engine (executor_transfer.go). Handles
// .partial files, range-based resume, hash verification with retry, session
// persistence for uploads, disk space pre-checks, and atomic rename.
type TransferManager struct {
	downloads        Downloader
	uploads          Uploader
	sessionStore     *SessionStore // nil = no session persistence for uploads
	logger           *slog.Logger
	hashFunc         func(string) (string, error)
	sessionMountID   string
	sessionLocalRoot string

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
		hashFunc:     ComputeQuickXorHash,
	}

	for _, opt := range opts {
		opt(tm)
	}

	return tm
}

// Binary size units for formatSize. OneDrive's own limits (250 GB) are
// binary, so the same units keep messages consistent with them.
const (
	sizeKB = 1 << 10
	sizeMB = 1 << 20
	sizeGB = 1 << 30
	sizeTB = 1 << 40
)

// formatSize renders a byte count in human units ("1.5 GB") for error and
// warning text a user reads at default verbosity.
func formatSize(bytes int64) string {
	switch {
	case bytes >= sizeTB:
		return fmt.Sprintf("%.1f TB", float64(bytes)/float64(sizeTB))
	case bytes >= sizeGB:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(sizeGB))
	case bytes >= sizeMB:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(sizeMB))
	case bytes >= sizeKB:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(sizeKB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// clampToInt64 converts a statfs byte count for formatSize. Free space beyond
// int64 range (8 EiB) does not occur on real filesystems.
func clampToInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}

	return int64(v)
}
