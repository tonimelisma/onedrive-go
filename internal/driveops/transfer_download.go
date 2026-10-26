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
	"github.com/tonimelisma/onedrive-go/internal/graph"
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

// isKnownHEICServedSmaller reports whether a just-downloaded file shows the
// documented OneDrive/iOS HEIC quirk: the API metadata describes the original
// upload, but OneDrive serves a post-processed, different-size file. Hash
// retries cannot help there (the server serves the same bytes every time),
// so the retry loop accepts the first download instead of fetching it again.
// A same-size mismatch is not this quirk and keeps the normal retries.
func isKnownHEICServedSmaller(targetPath string, remoteSize, localSize int64) bool {
	return strings.EqualFold(filepath.Ext(targetPath), ".heic") && remoteSize > 0 && remoteSize != localSize
}

// ErrDownloadIntegrity reports a download whose bytes could not be shown to
// be the remote file: every attempt disagreed with OneDrive's hash in
// different ways, or with its size, or came back as an HTML/JSON error
// document. The partial is discarded and the existing local file is left
// alone. Accepting whatever arrived after three tries replaced good local
// files with error pages, and re-downloaded them on every pass (D-038).
var ErrDownloadIntegrity = errors.New("the downloaded file doesn't match OneDrive's copy")

// DownloadOpts configures a single download operation.
type DownloadOpts struct {
	RemoteHash      string // expected QuickXorHash; empty = no hash verification
	RemoteMtime     int64  // nanoseconds; 0 = don't set (not Unix epoch)
	RemoteSize      int64  // expected size; a positive value is always known
	RemoteSizeKnown bool   // set for a known size of zero
	// RemoteETag identifies the remote version for a hashless item, so an
	// interrupted download resumes only against the same version.
	RemoteETag                 string
	MaxHashRetries             int // 0 = use default (2 retries, meaning 3 total download attempts)
	ValidateTargetBeforeRename func() error
}

// DownloadOptsForItem is the verification and version binding a download of
// item needs: its QuickXorHash, its size (always known for a Graph file
// item) and its eTag.
func DownloadOptsForItem(item *graph.Item) DownloadOpts {
	return DownloadOpts{
		RemoteHash:      SelectHash(item),
		RemoteSize:      item.Size,
		RemoteSizeKnown: true,
		RemoteETag:      item.ETag,
	}
}

// DownloadResult reports the outcome of a successful download.
type DownloadResult struct {
	LocalHash string
	Size      int64
	// EffectiveRemoteHash is the remote hash to record for the item: the one
	// OneDrive reports, even when the bytes it serves hash differently.
	// Recording the local hash as the remote one made every later pass see
	// a remote change and download the file again.
	EffectiveRemoteHash string
	HashVerified        bool // the bytes matched OneDrive's hash
	// AcceptedMismatch marks accepted server-transformed content: the bytes
	// differ from OneDrive's metadata in a known, deterministic way (the
	// iOS HEIC quirk, or the same wrong hash at the reported size on every
	// attempt), so they are what OneDrive serves for this version.
	AcceptedMismatch bool
}

// downloadVerdict is what one attempt's bytes prove.
type downloadVerdict int

const (
	downloadVerified downloadVerdict = iota
	downloadUnverifiable
	downloadKnownQuirk
	downloadMismatch
)

// classifyDownload judges one attempt. With a hash, only the hash proves the
// content (it covers the length). Without one the size is the only evidence,
// so a size mismatch (an empty or short body included) is a mismatch.
func classifyDownload(opts DownloadOpts, targetPath, localHash string, size int64) downloadVerdict {
	sizeDiffers := opts.remoteSizeKnown() && size != opts.RemoteSize

	switch {
	case opts.RemoteHash != "" && localHash == opts.RemoteHash:
		return downloadVerified
	case sizeDiffers && isKnownHEICServedSmaller(targetPath, opts.RemoteSize, size):
		return downloadKnownQuirk
	case opts.RemoteHash == "" && !sizeDiffers:
		return downloadUnverifiable
	default:
		return downloadMismatch
	}
}

// downloadAttempt is one mismatching attempt's evidence.
type downloadAttempt struct {
	localHash     string
	size          int64
	errorDocument bool
}

// acceptsServerTransform reports whether a run of mismatching attempts is
// the deterministic server transform P-10 accepts: every attempt returned
// the same bytes, none was an error document, the item has a hash to differ
// from, and the size is the reported one. Anything else is an integrity
// failure.
func acceptsServerTransform(opts DownloadOpts, attempts []downloadAttempt) bool {
	if opts.RemoteHash == "" || len(attempts) == 0 {
		return false
	}

	first := attempts[0]
	for _, attempt := range attempts {
		if attempt.errorDocument || attempt.localHash != first.localHash || attempt.size != first.size {
			return false
		}
	}

	return !opts.remoteSizeKnown() || first.size == opts.RemoteSize
}

func (opts DownloadOpts) remoteSizeKnown() bool {
	return opts.RemoteSizeKnown || opts.RemoteSize > 0
}

func (tm *TransferManager) logHEICMetadataMismatch(targetPath, localHash, remoteHash string, localSize, remoteSize int64) {
	// Debug only: this is a documented OneDrive bug that affects every
	// iOS-uploaded photo and recurs on every re-download. The user cannot do
	// anything about it, and a photo library holds thousands of them.
	tm.logger.Debug(heicMetadataMismatchMessage,
		slog.String("target", targetPath),
		slog.String("local_hash", localHash),
		slog.String("remote_hash", remoteHash),
		slog.Int64("local_size", localSize),
		slog.Int64("remote_size", remoteSize),
	)
}

// DownloadToFile downloads a remote file to targetPath with a namespaced
// partial file in the target directory: write to partial, optionally resume
// from an existing partial of the same remote version, verify, set mtime,
// atomic rename to target. A write that runs out of local space (ENOSPC,
// EDQUOT, no free inodes) returns ErrDiskFull like the pre-check does, so a
// full disk is one condition whichever step found it.
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

	partialPath := DownloadPartialPath(targetPath, opts)
	result, err := tm.downloadToVerifiedPartial(ctx, driveID, itemID, partialPath, targetPath, opts)
	if err != nil {
		return nil, err
	}

	if result.HashVerified && opts.remoteSizeKnown() && result.Size != opts.RemoteSize {
		// The verified hash proves the content; the size metadata is stale.
		tm.logger.Debug("downloaded file size differs from OneDrive's",
			slog.String("target", targetPath),
			slog.String("local_size", formatSize(result.Size)),
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
		slog.Int64("size", result.Size),
	)
	if collector := perf.FromContext(ctx); collector != nil {
		collector.RecordTransfer(perf.TransferKindDownload, result.Size, time.Since(startedAt))
		collector.AddCommandItems(1)
		collector.AddCommandBytes(result.Size)
	}

	return result, nil
}

// downloadToVerifiedPartial downloads until the partial holds bytes it can
// stand behind. A mismatch discards the partial and downloads the whole file
// again (if the first attempt was a resume, its bytes are wasted: mismatches
// are rare and correctness trumps transfer savings). After the last attempt
// only a deterministic server transform is accepted; anything else fails
// with ErrDownloadIntegrity and leaves no partial behind.
func (tm *TransferManager) downloadToVerifiedPartial(
	ctx context.Context, driveID driveid.ID, itemID, partialPath, targetPath string, opts DownloadOpts,
) (*DownloadResult, error) {
	// Go 1.22 range-over-int: `range N` iterates 0..N-1, so `range maxRetries+1`
	// gives exactly maxRetries+1 iterations (1 initial + maxRetries retries) (B-221).
	maxRetries := resolveMaxRetries(opts.MaxHashRetries)
	mismatches := make([]downloadAttempt, 0, maxRetries+1)

	for attempt := range maxRetries + 1 {
		localHash, size, err := tm.downloadToPartial(ctx, driveID, itemID, partialPath, opts, attempt == 0)
		if err != nil {
			if !errors.Is(err, graph.ErrUnexpectedDownloadBody) {
				return nil, err
			}

			mismatches = append(mismatches, downloadAttempt{errorDocument: true})
			tm.logger.Debug("download returned an error document, retrying",
				slog.String("target", targetPath),
				slog.Int("attempt", attempt+1),
				slog.String("error", err.Error()),
			)

			continue
		}

		result := &DownloadResult{LocalHash: localHash, Size: size, EffectiveRemoteHash: opts.RemoteHash}

		switch classifyDownload(opts, targetPath, localHash, size) {
		case downloadVerified:
			result.HashVerified = true
			return result, nil
		case downloadUnverifiable:
			// Debug only: some SharePoint files have no hash at all, and
			// the user cannot do anything about it.
			tm.logger.Debug("remote item has no content hash, verified by size only",
				slog.String("target", targetPath),
			)

			return result, nil
		case downloadKnownQuirk:
			// Re-downloading returns the same post-processed bytes, so the
			// first download is accepted instead of fetching the photo again.
			tm.logHEICMetadataMismatch(targetPath, localHash, opts.RemoteHash, size, opts.RemoteSize)
			result.AcceptedMismatch = true

			return result, nil
		case downloadMismatch:
		}

		mismatches = append(mismatches, downloadAttempt{localHash: localHash, size: size})
		// Debug only: a retry that will probably succeed. Only the final
		// outcome is reported.
		tm.logger.Debug("download doesn't match OneDrive's metadata",
			slog.String("target", targetPath),
			slog.Int("attempt", attempt+1),
			slog.String("local_hash", localHash),
			slog.String("remote_hash", opts.RemoteHash),
			slog.Int64("local_size", size),
			slog.Int64("remote_size", opts.RemoteSize),
		)

		if attempt == maxRetries && acceptsServerTransform(opts, mismatches) {
			// Same bytes every time at the reported size: this is what
			// OneDrive serves for the version (an AIP-protected or
			// server-rewritten file). The baseline records the remote
			// hash OneDrive reports, so the next pass is quiet.
			tm.logger.Info("OneDrive serves this file with a different checksum than it reports; keeping the download",
				slog.String("path", targetPath),
			)
			result.AcceptedMismatch = true

			return result, nil
		}

		tm.removePartial(partialPath)
	}

	return nil, fmt.Errorf("downloading %s: %w (%s)", targetPath, ErrDownloadIntegrity, describeMismatches(opts, mismatches))
}

// describeMismatches says in plain words why the download was refused.
func describeMismatches(opts DownloadOpts, attempts []downloadAttempt) string {
	last := attempts[len(attempts)-1]
	switch {
	case last.errorDocument:
		return "OneDrive answered with an error page instead of the file"
	case opts.remoteSizeKnown() && last.size != opts.RemoteSize:
		return fmt.Sprintf("received %s, OneDrive reports %s", formatSize(last.size), formatSize(opts.RemoteSize))
	default:
		return "the checksum differs on every attempt"
	}
}

func (tm *TransferManager) removePartial(partialPath string) {
	if err := localpath.Remove(partialPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		tm.logger.Warn("couldn't remove a leftover download file",
			slog.String("path", partialPath),
			slog.String("error", err.Error()))
	}
}

// DownloadPartialPath is where an interrupted download of targetPath keeps
// its bytes, so callers can point users at it. The name carries the remote
// version (its QuickXorHash, or its eTag for a hashless item), so a partial
// is only ever resumed against the version it was started from.
func DownloadPartialPath(targetPath string, opts DownloadOpts) string {
	return filepath.Join(filepath.Dir(targetPath), downloadPartialName(filepath.Base(targetPath), partialVersionTag(opts)))
}

// partialVersionTagPrefix starts the version tag in a partial's name.
const partialVersionTagPrefix = "~v"

// partialVersionTagHexLen is how many hex digits of the version hash a
// partial name carries: enough to tell versions of one file apart.
const partialVersionTagHexLen = 12

// downloadVersion is what a resumed range is bound to.
func downloadVersion(opts DownloadOpts) graph.DownloadVersion {
	return graph.DownloadVersion{ETag: opts.RemoteETag, QuickXorHash: opts.RemoteHash}
}

func partialVersionTag(opts DownloadOpts) string {
	key := opts.RemoteHash
	if key == "" {
		key = opts.RemoteETag
	}

	if key == "" {
		return ""
	}

	sum := sha256.Sum256([]byte(key))

	return partialVersionTagPrefix + hex.EncodeToString(sum[:])[:partialVersionTagHexLen]
}

// maxLocalNameBytes is NAME_MAX on Linux and macOS filesystems.
const maxLocalNameBytes = 255

// downloadPartialName namespaces a target's partial file and tags it with
// the remote version. The prefix and suffix add 21 bytes, so a name the
// filesystem accepts (up to 255 bytes) could have no partial at all: every
// download of a 235-255 byte name failed with "file name too long". Such
// names keep a readable head plus a hash of the full name, which stays
// deterministic so an interrupted download still resumes from the same
// partial.
func downloadPartialName(base, versionTag string) string {
	name := downloadPartialPrefix + base + versionTag + downloadPartialSuffix
	if len(name) <= maxLocalNameBytes {
		return name
	}

	sum := sha256.Sum256([]byte(base))
	tag := "~" + hex.EncodeToString(sum[:8])
	room := maxLocalNameBytes - len(downloadPartialPrefix) - len(tag) - len(versionTag) - len(downloadPartialSuffix)
	head := base[:room]
	for !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}

	return downloadPartialPrefix + head + tag + versionTag + downloadPartialSuffix
}

// IsOwnedTransferArtifactName reports whether a local basename belongs to
// onedrive-go's transfer machinery. It intentionally matches only the
// namespaced form used by this client, not arbitrary user-created *.partial
// files.
func IsOwnedTransferArtifactName(name string) bool {
	return strings.HasPrefix(name, downloadPartialPrefix) && strings.HasSuffix(name, downloadPartialSuffix)
}

// downloadToPartial streams a remote file to a .partial file while computing
// the QuickXorHash. When allowResume is set, the partial already holds bytes
// of this remote version (its name is version-tagged) and the downloader
// supports range requests, it resumes from the existing file. A partial with
// no version to bind to is never resumed: for a hashless item nothing would
// catch a splice of two versions.
//
// The .partial file is opened before stat to avoid a TOCTOU race where the
// file could be deleted between stat and open (B-211). If open fails with
// ErrNotExist, we fall through to a fresh download.
func (tm *TransferManager) downloadToPartial(
	ctx context.Context, driveID driveid.ID, itemID, partialPath string, opts DownloadOpts, allowResume bool,
) (string, int64, error) {
	version := downloadVersion(opts)
	rd, canRange := tm.downloads.(RangeDownloader)

	if allowResume && canRange && (version.QuickXorHash != "" || version.ETag != "") {
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
				return tm.resumeDownloadFromFile(ctx, driveID, itemID, rd, f, partialPath, info.Size(), opts)
			}
		} else if !errors.Is(openErr, os.ErrNotExist) {
			tm.logger.Warn("cannot open partial file for resume, starting fresh",
				slog.String("path", partialPath), slog.String("error", openErr.Error()))
		}
	}

	return tm.freshDownload(ctx, driveID, itemID, partialPath, opts)
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
	ctx context.Context, driveID driveid.ID, itemID, partialPath string, opts DownloadOpts,
) (string, int64, error) {
	f, err := localpath.OpenFile(partialPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, downloadTempFilePerms)
	if err != nil {
		return "", 0, fmt.Errorf("creating partial file %s: %w", partialPath, err)
	}

	h := quickxorhash.New()
	w := io.MultiWriter(f, h)

	size, err := tm.downloads.Download(ctx, driveID, itemID, w)
	if errors.Is(err, graph.ErrNoDownloadURL) && opts.RemoteSize == 0 {
		// Business omits the download URL of some empty files. An empty
		// file has nothing to fetch, so the truncated partial is the file
		// (RCL-053); without this every such item failed forever.
		size, err = 0, nil
	}
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
	rd RangeDownloader, f *os.File, partialPath string, existingSize int64, opts DownloadOpts,
) (string, int64, error) {
	tm.logger.Debug("resuming download from partial file",
		slog.String("path", partialPath),
		slog.Int64("existing_bytes", existingSize),
	)

	n, err := rd.DownloadRange(ctx, driveID, itemID, f, existingSize, downloadVersion(opts))

	if closeErr := f.Close(); closeErr != nil {
		tm.logger.Warn("failed to close partial file after range download",
			slog.String("path", partialPath), slog.String("error", closeErr.Error()))

		tm.removePartialIfNotCanceled(ctx, partialPath)

		return tm.freshDownload(ctx, driveID, itemID, partialPath, opts)
	}

	if err != nil {
		// Debug only: a handled fallback. If the fresh download fails too,
		// its error is the report.
		tm.logger.Debug("range download failed, falling back to fresh download",
			slog.String("path", partialPath), slog.String("error", err.Error()))

		tm.removePartialIfNotCanceled(ctx, partialPath)

		return tm.freshDownload(ctx, driveID, itemID, partialPath, opts)
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
	if tagAt := strings.LastIndex(name, partialVersionTagPrefix); tagAt >= 0 &&
		len(name)-tagAt == len(partialVersionTagPrefix)+partialVersionTagHexLen {
		name = name[:tagAt]
	}

	return filepath.Join(filepath.Dir(partialPath), name)
}
