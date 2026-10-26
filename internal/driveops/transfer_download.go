package driveops

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// defaultMaxHashRetries is the default number of additional download attempts
// when the content hash doesn't match the remote hash.
const defaultMaxHashRetries = 2

// maxSaneRetries caps MaxHashRetries to prevent integer overflow in the
// `range maxRetries + 1` loop. Any value above this is almost certainly a bug.
const maxSaneRetries = 100

// Download temp files may contain credentials or personal data, so both the
// partial file and its parent directory stay owner-only.
const (
	downloadTempDirPerms  = 0o700
	downloadTempFilePerms = 0o600
	downloadPartialPrefix = ".onedrive-go."
	downloadPartialSuffix = ".partial"
)

const heicMetadataMismatchMessage = "known OneDrive/iOS metadata mismatch, accepting download"

// resolveMaxRetries returns the effective max hash retries, applying the
// default and overflow-safe upper bound.
func resolveMaxRetries(configured int) int {
	if configured <= 0 {
		return defaultMaxHashRetries
	}

	if configured > maxSaneRetries {
		return maxSaneRetries
	}

	return configured
}

func isHEICMetadataMismatch(targetPath, remoteHash string, hashVerified bool, remoteSize, localSize int64) bool {
	if !strings.EqualFold(filepath.Ext(targetPath), ".heic") {
		return false
	}

	hashMismatchAccepted := remoteHash != "" && !hashVerified
	sizeMismatch := remoteSize > 0 && remoteSize != localSize

	return hashMismatchAccepted || sizeMismatch
}

// isKnownHEICServedSmaller reports whether a just-downloaded file shows the
// documented OneDrive/iOS HEIC quirk: the API metadata describes the original
// upload, but OneDrive serves a post-processed, different-size file. Hash
// retries cannot help there (the server serves the same bytes every time),
// so the retry loop accepts the first download instead of fetching it again.
// A same-size mismatch is not this quirk and keeps the normal retries.
func isKnownHEICServedSmaller(targetPath string, remoteSize, localSize int64) bool {
	return strings.EqualFold(filepath.Ext(targetPath), ".heic") && remoteSize > 0 && remoteSize != localSize
}

// DownloadOpts configures a single download operation.
type DownloadOpts struct {
	RemoteHash                 string // expected hash; empty = skip verification
	RemoteMtime                int64  // nanoseconds; 0 = don't set (not Unix epoch)
	RemoteSize                 int64  // expected size; 0 = don't validate
	MaxHashRetries             int    // 0 = use default (2 retries, meaning 3 total download attempts)
	ValidateTargetBeforeRename func() error
}

// DownloadResult reports the outcome of a successful download.
type DownloadResult struct {
	LocalHash           string
	Size                int64
	EffectiveRemoteHash string // remote hash after possible exhaustion override
	HashVerified        bool   // false when hash retries exhausted and mismatch accepted
}

func (tm *TransferManager) logHEICMetadataMismatch(
	targetPath, localHash, remoteHash string, localSize, remoteSize int64, hashVerified bool,
) {
	attrs := []any{
		slog.String("target", targetPath),
		slog.Bool("hash_verified", hashVerified),
		slog.String("local_hash", localHash),
	}

	if remoteHash != "" {
		attrs = append(attrs, slog.String("remote_hash", remoteHash))
	}

	if remoteSize > 0 {
		attrs = append(attrs,
			slog.Int64("local_size", localSize),
			slog.Int64("remote_size", remoteSize),
		)
	}

	// Debug only: this is a documented OneDrive bug that affects every
	// iOS-uploaded photo and recurs on every re-download. The user cannot do
	// anything about it, and a photo library holds thousands of them.
	tm.logger.Debug(heicMetadataMismatchMessage, attrs...)
}

// DownloadToFile downloads a remote file to targetPath with a namespaced
// partial file in the target directory: write to partial, optionally resume
// from existing partial, verify hash with retry, set mtime, atomic rename to
// target. A write that runs out of local space (ENOSPC, EDQUOT, no free
// inodes) returns ErrDiskFull like the pre-check does, so a full disk is one
// condition whichever step found it.
func (tm *TransferManager) DownloadToFile(
	ctx context.Context, driveID driveid.ID, itemID, targetPath string, opts DownloadOpts,
) (*DownloadResult, error) {
	result, err := tm.downloadToFile(ctx, driveID, itemID, targetPath, opts)
	if err != nil {
		return nil, wrapLocalWriteError(filepath.Dir(targetPath), err)
	}

	return result, nil
}

func (tm *TransferManager) downloadToFile(
	ctx context.Context, driveID driveid.ID, itemID, targetPath string, opts DownloadOpts,
) (*DownloadResult, error) {
	startedAt := time.Now()

	if targetPath == "" {
		return nil, fmt.Errorf("download: target path must not be empty")
	}

	if itemID == "" {
		return nil, fmt.Errorf("download: item ID must not be empty")
	}

	tm.logger.Debug("DownloadToFile",
		slog.String("drive_id", driveID.String()),
		slog.String("target", targetPath),
		slog.String("item_id", itemID),
	)

	// Disk space pre-check (R-6.2.6, R-2.10.43, R-2.10.44).
	// Runs before directory creation or download to avoid partial writes.
	if err := tm.checkDiskSpace(ctx, targetPath, opts.RemoteSize); err != nil {
		return nil, err
	}

	// Ensure parent directory exists.
	if err := localpath.MkdirAll(filepath.Dir(targetPath), downloadTempDirPerms); err != nil {
		return nil, fmt.Errorf("creating parent dir for %s: %w", targetPath, err)
	}

	partialPath := downloadPartialPath(targetPath)
	localHash, size, remoteHash, hashVerified, err := tm.downloadToVerifiedPartial(
		ctx, driveID, itemID, partialPath, targetPath, opts,
	)
	if err != nil {
		return nil, err
	}

	// Warn if downloaded size doesn't match expected remote size.
	if isHEICMetadataMismatch(targetPath, opts.RemoteHash, hashVerified, opts.RemoteSize, size) {
		tm.logHEICMetadataMismatch(targetPath, localHash, opts.RemoteHash, size, opts.RemoteSize, hashVerified)
	} else if opts.RemoteSize > 0 && size != opts.RemoteSize {
		// A verified hash proves the content is right and the size metadata
		// is what is stale, so that case is Debug. Without verification the
		// mismatch is the only integrity signal, so it stays a warning.
		level := slog.LevelWarn
		if hashVerified {
			level = slog.LevelDebug
		}
		tm.logger.Log(ctx, level, "downloaded file size differs from OneDrive's",
			slog.String("target", targetPath),
			slog.String("local_size", formatSize(size)),
			slog.String("remote_size", formatSize(opts.RemoteSize)),
		)
	}

	// Set mtime on the partial file before atomic rename.
	if opts.RemoteMtime != 0 {
		mtime := time.Unix(0, opts.RemoteMtime)
		if err := localpath.Chtimes(partialPath, mtime, mtime); err != nil {
			tm.logger.Warn("couldn't set the modification time of the downloaded file",
				slog.String("path", targetPath),
				slog.String("error", err.Error()),
			)
		}
	}

	if err := validateDownloadTargetBeforeRename(opts); err != nil {
		return nil, err
	}

	// Atomic rename: .partial -> target. On failure the .partial file is
	// intentionally preserved so the next attempt can resume from it rather
	// than re-downloading the entire file (B-207).
	if err := localpath.Rename(partialPath, targetPath); err != nil {
		return nil, fmt.Errorf("renaming partial to %s: %w", targetPath, err)
	}

	tm.logger.Debug("download complete",
		slog.String("target", targetPath),
		slog.Int64("size", size),
	)
	if collector := perf.FromContext(ctx); collector != nil {
		collector.RecordTransfer(perf.TransferKindDownload, size, time.Since(startedAt))
		collector.AddCommandItems(1)
		collector.AddCommandBytes(size)
	}

	return &DownloadResult{
		LocalHash:           localHash,
		Size:                size,
		EffectiveRemoteHash: remoteHash,
		HashVerified:        hashVerified,
	}, nil
}

func (tm *TransferManager) downloadToVerifiedPartial(
	ctx context.Context,
	driveID driveid.ID,
	itemID string,
	partialPath string,
	targetPath string,
	opts DownloadOpts,
) (localHash string, size int64, remoteHash string, hashVerified bool, err error) {
	remoteHash = opts.RemoteHash
	hashVerified = remoteHash != "" // no verification possible without a remote hash (B-021)

	// Fast path: no remote hash means no verification — download once, skip retry loop.
	if remoteHash == "" {
		localHash, size, err = tm.downloadToPartial(ctx, driveID, itemID, partialPath)
		if err != nil {
			return "", 0, "", false, err
		}

		// Debug only: some SharePoint files have no hash at all, and the
		// user cannot do anything about it. DownloadResult.HashVerified
		// carries the fact to callers that need it.
		tm.logger.Debug("remote item has no content hash, skipping verification",
			slog.String("target", targetPath),
		)
		return localHash, size, remoteHash, hashVerified, nil
	}

	return tm.downloadWithHashRetry(
		ctx, driveID, itemID, partialPath, targetPath, remoteHash, opts.RemoteSize, opts.MaxHashRetries,
	)
}

// DownloadPartialPath is where an interrupted download of targetPath keeps
// its bytes, so callers can point users at it.
func DownloadPartialPath(targetPath string) string {
	return downloadPartialPath(targetPath)
}

// maxLocalNameBytes is NAME_MAX on Linux and macOS filesystems.
const maxLocalNameBytes = 255

func downloadPartialPath(targetPath string) string {
	return filepath.Join(filepath.Dir(targetPath), downloadPartialName(filepath.Base(targetPath)))
}

// downloadPartialName namespaces a target's partial file. The prefix and
// suffix add 21 bytes, so a name the filesystem accepts (up to 255 bytes)
// could have no partial at all: every download of a 235-255 byte name failed
// with "file name too long". Such names keep a readable head plus a hash of
// the full name, which stays deterministic so an interrupted download still
// resumes from the same partial.
func downloadPartialName(base string) string {
	name := downloadPartialPrefix + base + downloadPartialSuffix
	if len(name) <= maxLocalNameBytes {
		return name
	}

	sum := sha256.Sum256([]byte(base))
	tag := "~" + hex.EncodeToString(sum[:8])
	room := maxLocalNameBytes - len(downloadPartialPrefix) - len(tag) - len(downloadPartialSuffix)
	head := base[:room]
	for !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}

	return downloadPartialPrefix + head + tag + downloadPartialSuffix
}

// IsOwnedTransferArtifactName reports whether a local basename belongs to
// onedrive-go's transfer machinery. It intentionally matches only the
// namespaced form used by this client, not arbitrary user-created *.partial
// files.
func IsOwnedTransferArtifactName(name string) bool {
	return strings.HasPrefix(name, downloadPartialPrefix) && strings.HasSuffix(name, downloadPartialSuffix)
}

// downloadWithHashRetry downloads a file and retries on hash mismatch. On
// mismatch we discard and re-download the entire file. If the first attempt
// was a resume, the resume bytes are wasted — acceptable because mismatches
// are rare and correctness trumps transfer savings.
func (tm *TransferManager) downloadWithHashRetry(
	ctx context.Context, driveID driveid.ID, itemID, partialPath, targetPath, remoteHash string,
	remoteSize int64, maxHashRetries int,
) (localHash string, size int64, effectiveRemoteHash string, hashVerified bool, err error) {
	effectiveRemoteHash = remoteHash
	hashVerified = true

	// Go 1.22 range-over-int: `range N` iterates 0..N-1, so `range maxRetries+1`
	// gives exactly maxRetries+1 iterations (1 initial + maxRetries retries) (B-221).
	maxRetries := resolveMaxRetries(maxHashRetries)

	for attempt := range maxRetries + 1 {
		localHash, size, err = tm.downloadToPartial(ctx, driveID, itemID, partialPath)
		if err != nil {
			return "", 0, "", false, err
		}

		// Hash matches — verification succeeded.
		if localHash == effectiveRemoteHash {
			return localHash, size, effectiveRemoteHash, true, nil
		}

		// Known OneDrive/iOS HEIC quirk: re-downloading returns the same
		// post-processed bytes, so accept now instead of downloading the
		// photo two more times. DownloadToFile logs the acceptance.
		if isKnownHEICServedSmaller(targetPath, remoteSize, size) {
			return localHash, size, localHash, false, nil
		}

		if attempt < maxRetries {
			if cleanupErr := localpath.Remove(partialPath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				tm.logger.Warn("couldn't remove a leftover download file",
					slog.String("path", partialPath),
					slog.String("error", cleanupErr.Error()))
			}
			// Debug only: a retry that will probably succeed. Only the final
			// outcome below is worth a warning.
			tm.logger.Debug("download hash mismatch, retrying",
				slog.String("target", targetPath),
				slog.Int("attempt", attempt+1),
				slog.String("local_hash", localHash),
				slog.String("remote_hash", effectiveRemoteHash),
			)

			continue
		}

		// All hash retries exhausted — accept to prevent infinite loop.
		tm.logger.Debug("download hash mismatch after all retries",
			slog.String("target", targetPath),
			slog.String("local_hash", localHash),
			slog.String("remote_hash", effectiveRemoteHash),
		)
		tm.logger.Warn("downloaded file doesn't match OneDrive's checksum; kept it anyway",
			slog.String("path", targetPath),
		)

		effectiveRemoteHash = localHash
		hashVerified = false
	}

	return localHash, size, effectiveRemoteHash, hashVerified, nil
}

// downloadToPartial streams a remote file to a .partial file while computing
// the QuickXorHash. If a .partial file already exists and the downloader
// supports range requests, it resumes from the existing file.
//
// The .partial file is opened before stat to avoid a TOCTOU race where the
// file could be deleted between stat and open (B-211). If open fails with
// ErrNotExist, we fall through to a fresh download.
func (tm *TransferManager) downloadToPartial(
	ctx context.Context, driveID driveid.ID, itemID, partialPath string,
) (string, int64, error) {
	// Attempt resume: open existing .partial, then stat the handle.
	if rd, ok := tm.downloads.(RangeDownloader); ok {
		f, openErr := localpath.OpenFile(partialPath, os.O_APPEND|os.O_WRONLY, downloadTempFilePerms)
		if openErr == nil {
			info, statErr := f.Stat()
			if statErr != nil || info.Size() == 0 {
				// Empty or unreadable — close and start fresh.
				if closeErr := f.Close(); closeErr != nil {
					tm.logger.Warn("failed to close partial file before fresh download",
						slog.String("path", partialPath),
						slog.String("error", closeErr.Error()))
				}
			} else {
				return tm.resumeDownloadFromFile(ctx, driveID, itemID, rd, f, partialPath, info.Size())
			}
		} else if !errors.Is(openErr, os.ErrNotExist) {
			tm.logger.Warn("cannot open partial file for resume, starting fresh",
				slog.String("path", partialPath), slog.String("error", openErr.Error()))
		}
	}

	return tm.freshDownload(ctx, driveID, itemID, partialPath)
}

// removePartialIfNotCanceled removes a .partial file unless the context was
// canceled (preserving the partial for future resume). Extracted to deduplicate
// the identical pattern in freshDownload and resumeDownload.
func (tm *TransferManager) removePartialIfNotCanceled(ctx context.Context, path string) {
	if ctx.Err() == nil {
		if removeErr := localpath.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			tm.logger.Warn("couldn't remove a leftover download file",
				slog.String("path", path),
				slog.String("error", removeErr.Error()),
			)
		}
	}
}

// freshDownload performs a full download to a new .partial file.
func (tm *TransferManager) freshDownload(
	ctx context.Context, driveID driveid.ID, itemID, partialPath string,
) (string, int64, error) {
	f, err := localpath.OpenFile(partialPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, downloadTempFilePerms)
	if err != nil {
		return "", 0, fmt.Errorf("creating partial file %s: %w", partialPath, err)
	}

	h := quickxorhash.New()
	w := io.MultiWriter(f, h)

	size, err := tm.downloads.Download(ctx, driveID, itemID, w)
	if err != nil {
		if closeErr := f.Close(); closeErr != nil {
			tm.logger.Warn("failed to close partial file after download error",
				slog.String("path", partialPath), slog.String("error", closeErr.Error()))
		}

		// Preserve partial on context cancellation so resume can reuse it.
		tm.removePartialIfNotCanceled(ctx, partialPath)

		// Name the file the caller asked for, not the internal partial file.
		return "", 0, fmt.Errorf("downloading %s: %w", targetPathForPartial(partialPath), err)
	}

	if err := f.Close(); err != nil {
		// Close failure is always an error regardless of context — the file is corrupt.
		baseErr := fmt.Errorf("closing partial file %s: %w", partialPath, err)
		if removeErr := localpath.Remove(partialPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return "", 0, errors.Join(baseErr, fmt.Errorf("removing corrupt partial file %s: %w", partialPath, removeErr))
		}

		return "", 0, baseErr
	}

	localHash := base64.StdEncoding.EncodeToString(h.Sum(nil))

	return localHash, size, nil
}

// resumeDownloadFromFile appends bytes to an already-open .partial file using
// Range requests, then hashes the complete file from byte 0. The caller opens
// the file and passes it to avoid a TOCTOU race (B-211).
func (tm *TransferManager) resumeDownloadFromFile(
	ctx context.Context, driveID driveid.ID, itemID string,
	rd RangeDownloader, f *os.File, partialPath string, existingSize int64,
) (string, int64, error) {
	tm.logger.Debug("resuming download from partial file",
		slog.String("path", partialPath),
		slog.Int64("existing_bytes", existingSize),
	)

	n, err := rd.DownloadRange(ctx, driveID, itemID, f, existingSize)

	if closeErr := f.Close(); closeErr != nil {
		tm.logger.Warn("failed to close partial file after range download",
			slog.String("path", partialPath), slog.String("error", closeErr.Error()))

		tm.removePartialIfNotCanceled(ctx, partialPath)

		return tm.freshDownload(ctx, driveID, itemID, partialPath)
	}

	if err != nil {
		// Debug only: a handled fallback. If the fresh download fails too,
		// its error is the report.
		tm.logger.Debug("range download failed, falling back to fresh download",
			slog.String("path", partialPath), slog.String("error", err.Error()))

		tm.removePartialIfNotCanceled(ctx, partialPath)

		return tm.freshDownload(ctx, driveID, itemID, partialPath)
	}

	totalSize := existingSize + n

	localHash, err := tm.hashFunc(partialPath)
	if err != nil {
		tm.removePartialIfNotCanceled(ctx, partialPath)

		return "", 0, fmt.Errorf("hashing resumed partial file %s: %w", partialPath, err)
	}

	return localHash, totalSize, nil
}

func validateDownloadTargetBeforeRename(opts DownloadOpts) error {
	if opts.ValidateTargetBeforeRename == nil {
		return nil
	}

	return opts.ValidateTargetBeforeRename()
}

// checkDiskSpace verifies sufficient disk space before a download (R-6.2.6).
// No-op when diskAvailableFunc is nil or minFreeSpace is 0.
//
// Two-tier check:
//   - Available < minFreeSpace → ErrDiskFull (block scope in sync engine)
//   - Available ≥ minFreeSpace but < fileSize + minFreeSpace → ErrFileTooLargeForSpace (per-file skip)
//
// Fails open on statfs errors: a transient statfs failure should not block
// all downloads.
func (tm *TransferManager) checkDiskSpace(ctx context.Context, targetPath string, fileSize int64) error {
	if tm.diskAvailableFunc == nil || tm.minFreeSpace == 0 {
		return nil
	}

	// Check the filesystem containing the target's parent directory.
	checkPath := filepath.Dir(targetPath)

	available, err := tm.diskAvailableFunc(checkPath)
	if err != nil {
		// Fail open: a transient statfs error should not block downloads.
		// A persistent one (an unusual FUSE mount) fails for every download
		// of every pass, so it is announced once per TransferManager and
		// logged at Debug after that.
		level := slog.LevelDebug
		if tm.diskCheckFailureWarned.CompareAndSwap(false, true) {
			level = slog.LevelWarn
		}
		tm.logger.Log(ctx, level, "couldn't check free disk space, downloading anyway",
			slog.String("path", checkPath),
			slog.String("error", err.Error()),
		)

		return nil
	}

	minFree := uint64(tm.minFreeSpace)
	if available < minFree {
		return fmt.Errorf(
			"%w: %s free, min_free_space requires %s; free up space or lower min_free_space in the config",
			ErrDiskFull, formatSize(clampToInt64(available)), formatSize(tm.minFreeSpace))
	}

	// Per-file check: can this specific file fit within available - minFreeSpace?
	if fileSize > 0 && available < uint64(fileSize)+minFree {
		return fmt.Errorf(
			"%w: the file needs %s but only %s is free after keeping min_free_space (%s) in reserve",
			ErrFileTooLargeForSpace, formatSize(fileSize), formatSize(clampToInt64(available-minFree)),
			formatSize(tm.minFreeSpace))
	}

	return nil
}

// targetPathForPartial recovers the file a namespaced partial download stands
// for, so errors name the file the user asked for rather than the internal
// ".onedrive-go.<name>.partial" temp file.
func targetPathForPartial(partialPath string) string {
	base := filepath.Base(partialPath)
	if !IsOwnedTransferArtifactName(base) {
		return partialPath
	}

	name := strings.TrimSuffix(strings.TrimPrefix(base, downloadPartialPrefix), downloadPartialSuffix)

	return filepath.Join(filepath.Dir(partialPath), name)
}
