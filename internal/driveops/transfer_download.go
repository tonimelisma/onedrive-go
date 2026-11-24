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
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/humanunit"
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

// A partial file may hold personal data before the rename publishes it, so
// it stays owner-only while it is written; finalizePartial gives it its
// final mode.
const (
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

// isHEICServedSmaller reports the shape of the documented OneDrive/iOS HEIC
// quirk: the API metadata describes the original upload, but OneDrive serves
// a post-processed, smaller file. Hash retries cannot help there (the server
// serves the same bytes every time), so the retry loop accepts the first
// download instead of fetching it again. The shape alone does not identify
// the quirk: an error page served with HTTP 200, or an empty or truncated
// body, is smaller too, and accepting it put the page on disk as the photo
// and recorded it as synced. The served bytes must also be a HEIF file
// (hasISOBMFFSignature). A same-size or larger mismatch is not this quirk
// and keeps the normal retries.
func isHEICServedSmaller(targetPath string, remoteSize, localSize int64) bool {
	return strings.EqualFold(filepath.Ext(targetPath), ".heic") && remoteSize > 0 && localSize > 0 && localSize < remoteSize
}

// An ISO BMFF file (HEIF, HEIC, MP4) starts with a box whose 4-byte size is
// followed by its 4-byte type; the first box is "ftyp".
const (
	isoBMFFBoxTypeOffset = 4
	isoBMFFBoxTypeLen    = 4
	isoBMFFFileTypeBox   = "ftyp"
)

// hasISOBMFFSignature reports whether the file at name starts like a HEIF
// container. A body too short to hold the signature is not one; only a read
// failure is an error.
func hasISOBMFFSignature(target TargetFS, name string) (bool, error) {
	f, err := target.Open(name)
	if err != nil {
		return false, err //nolint:wrapcheck // the target names the open and the path
	}
	defer f.Close() //nolint:errcheck // read-only; nothing buffered to flush

	var header [isoBMFFBoxTypeOffset + isoBMFFBoxTypeLen]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}

		return false, fmt.Errorf("reading the start: %w", err)
	}

	return string(header[isoBMFFBoxTypeOffset:]) == isoBMFFFileTypeBox, nil
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
	RemoteHash      string    // expected QuickXorHash; empty = no hash verification
	RemoteMtime     time.Time // the modification time to give the file; the zero time sets none
	RemoteSize      int64     // expected size; a positive value is always known
	RemoteSizeKnown bool      // set for a known size of zero
	// RemoteETag identifies the remote version for a hashless item, so an
	// interrupted download resumes only against the same version.
	RemoteETag                 string
	MaxHashRetries             int // 0 = use default (2 retries, meaning 3 total download attempts)
	ValidateTargetBeforeRename func() error
	// Target is the filesystem the download writes through, resolving the
	// target path. nil means LocalTarget: absolute local paths, missing
	// parents created.
	Target TargetFS
}

func (opts DownloadOpts) target() TargetFS {
	if opts.Target == nil {
		return LocalTarget()
	}

	return opts.Target
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
// so a size mismatch (an empty or short body included) is a mismatch. The
// HEIC quirk needs all of: a .heic served smaller than reported and served
// bytes that are a HEIF file (servedHEIF, read from the partial by the
// caller); an error document never reaches this judgement.
func classifyDownload(opts DownloadOpts, targetPath, localHash string, size int64, servedHEIF bool) downloadVerdict {
	sizeDiffers := opts.remoteSizeKnown() && size != opts.RemoteSize

	switch {
	case opts.RemoteHash != "" && localHash == opts.RemoteHash:
		return downloadVerified
	case sizeDiffers && servedHEIF && isHEICServedSmaller(targetPath, opts.RemoteSize, size):
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
// the deterministic server transform R-5.8.2 accepts: every attempt returned
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
		return nil, errors.New("download: target path must not be empty")
	}

	if itemID == "" {
		return nil, errors.New("download: item ID must not be empty")
	}

	tm.logger.Debug("DownloadToFile",
		slog.String("drive_id", driveID.String()),
		slog.String("target", targetPath),
		slog.String("item_id", itemID),
	)

	target := opts.target()
	parentDir := filepath.Dir(targetPath)

	// Disk space pre-check (R-6.2.6, R-2.10.43, R-2.10.44).
	// Runs before directory creation or download to avoid partial writes.
	if err := tm.checkDiskSpace(ctx, target, targetPath, opts.RemoteSize); err != nil {
		return nil, err
	}

	if err := target.PrepareParent(parentDir); err != nil {
		return nil, fmt.Errorf("preparing the folder: %w", err)
	}

	partialPath, resumable := tm.partialPathFor(targetPath, opts)
	result, err := tm.downloadToVerifiedPartial(ctx, driveID, itemID, partialPath, targetPath, resumable, opts)
	if err != nil {
		return nil, err
	}

	if result.HashVerified && opts.remoteSizeKnown() && result.Size != opts.RemoteSize {
		// The verified hash proves the content; the size metadata is stale.
		tm.logger.Debug("downloaded file size differs from OneDrive's",
			slog.String("target", targetPath),
			slog.String("local_size", humanunit.Size(result.Size)),
			slog.String("remote_size", humanunit.Size(opts.RemoteSize)),
		)
	}

	if err := tm.publishPartial(target, partialPath, targetPath, parentDir, opts); err != nil {
		return nil, err
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

// publishPartial makes the verified partial the target: final mode and
// fsync, mtime, the caller's last precondition, the atomic rename and the
// directory sync.
func (tm *TransferManager) publishPartial(target TargetFS, partialPath, targetPath, parentDir string, opts DownloadOpts) error {
	// Set mtime on the partial file before its fsync, so the flush makes the
	// time durable together with the bytes.
	if !opts.RemoteMtime.IsZero() {
		if err := target.Chtimes(partialPath, opts.RemoteMtime, opts.RemoteMtime); err != nil {
			tm.logger.Warn("couldn't set the modification time of the downloaded file",
				slog.String("path", targetPath),
				slog.Any("error", err),
			)
		}
	}

	if err := tm.finalizePartial(target, partialPath, targetPath); err != nil {
		return err
	}

	if err := validateDownloadTargetBeforeRename(opts); err != nil {
		return err
	}

	// Atomic rename: .partial -> target. On failure the .partial file is
	// intentionally preserved so the next attempt can resume from it rather
	// than re-downloading the entire file (B-207).
	if err := target.Rename(partialPath, targetPath); err != nil {
		return fmt.Errorf("moving the download into place: %w", err)
	}

	// The rename is only durable once the directory entry is: without this
	// a power cut could lose it after the baseline recorded the download
	// (D-042). The bytes are already on disk, so a failure here is reported
	// rather than undone.
	if err := target.SyncDir(parentDir); err != nil {
		if !localpath.IsDirSyncUnsupported(err) {
			return fmt.Errorf("syncing the folder: %w", err)
		}

		// Some filesystems (FUSE, SMB, some network mounts) cannot fsync a
		// directory. The file is already in place and its bytes on disk,
		// so this is best effort there.
		tm.logger.Debug("the folder could not be synced; the filesystem does not support it",
			slog.String("path", targetPath),
			slog.Any("error", err),
		)
	}

	return nil
}

// finalizePartial gives the verified partial its final mode and makes its
// bytes durable before the rename publishes them: the replaced file's
// permission bits, or 0666 minus the umask for a new file (D-044), and an
// fsync (D-042). Without the fsync a power cut after the rename and the
// baseline commit could leave a zero-filled or empty file that the next pass
// trusts, or uploads over the good remote copy.
func (tm *TransferManager) finalizePartial(target TargetFS, partialPath, targetPath string) error {
	mode, err := tm.downloadedFileMode(target, targetPath)
	if err != nil {
		return err
	}

	f, err := target.OpenFile(partialPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("reopening the download: %w", err)
	}

	if err := f.Chmod(mode); err != nil {
		return errors.Join(fmt.Errorf("setting the mode of the download: %w", err), f.Close())
	}

	if err := tm.syncFile(f); err != nil {
		return errors.Join(fmt.Errorf("flushing the download to disk: %w", err), f.Close())
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("closing the download: %w", err)
	}

	return nil
}

// downloadToVerifiedPartial downloads until the partial holds bytes it can
// stand behind. A mismatch discards the partial and downloads the whole file
// again (if the first attempt was a resume, its bytes are wasted: mismatches
// are rare and correctness trumps transfer savings). After the last attempt
// only a deterministic server transform is accepted; anything else fails
// with ErrDownloadIntegrity and leaves no partial behind.
//
// resumable is false for a single-use partial (partialPathFor), which no
// attempt resumes.
func (tm *TransferManager) downloadToVerifiedPartial(
	ctx context.Context, driveID driveid.ID, itemID, partialPath, targetPath string, resumable bool, opts DownloadOpts,
) (*DownloadResult, error) {
	// Go 1.22 range-over-int: `range N` iterates 0..N-1, so `range maxRetries+1`
	// gives exactly maxRetries+1 iterations (1 initial + maxRetries retries) (B-221).
	maxRetries := resolveMaxRetries(opts.MaxHashRetries)
	mismatches := make([]downloadAttempt, 0, maxRetries+1)

	for attempt := range maxRetries + 1 {
		localHash, size, err := tm.downloadToPartial(ctx, driveID, itemID, partialPath, opts, resumable, attempt == 0)
		if err != nil {
			if !errors.Is(err, graph.ErrUnexpectedDownloadBody) {
				return nil, err
			}

			mismatches = append(mismatches, downloadAttempt{errorDocument: true})
			tm.logger.Debug("download returned an error document, retrying",
				slog.String("target", targetPath),
				slog.Int("attempt", attempt+1),
				slog.Any("error", err),
			)

			continue
		}

		result := &DownloadResult{LocalHash: localHash, Size: size, EffectiveRemoteHash: opts.RemoteHash}

		// Only a candidate for the HEIC quirk is inspected: the signature
		// read costs an open per attempt, and nothing else needs it.
		servedHEIF := false
		if isHEICServedSmaller(targetPath, opts.RemoteSize, size) {
			servedHEIF, err = hasISOBMFFSignature(opts.target(), partialPath)
			if err != nil {
				return nil, fmt.Errorf("inspecting the download: %w", err)
			}
		}

		switch classifyDownload(opts, targetPath, localHash, size, servedHEIF) {
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
			// hash OneDrive reports, so the next pass is quiet. Business
			// and SharePoint rewrite uploads routinely, so there this is
			// INFO; nothing legitimately rewrites bytes on a personal
			// drive, so there the user is warned, once per file.
			level := slog.LevelWarn
			if isBusinessLikeDriveID(driveID) {
				level = slog.LevelInfo
			}
			tm.logger.Log(ctx, level, "OneDrive serves this file with a different checksum than it reports; keeping the download",
				slog.String("path", targetPath),
			)
			result.AcceptedMismatch = true

			return result, nil
		}

		tm.removePartial(opts.target(), partialPath)
	}

	return nil, fmt.Errorf("%w (%s)", ErrDownloadIntegrity, describeMismatches(opts, mismatches))
}

// describeMismatches says in plain words why the download was refused.
func describeMismatches(opts DownloadOpts, attempts []downloadAttempt) string {
	last := attempts[len(attempts)-1]
	switch {
	case last.errorDocument:
		return "OneDrive answered with an error page instead of the file"
	case opts.remoteSizeKnown() && last.size != opts.RemoteSize:
		return fmt.Sprintf("received %s, OneDrive reports %s", humanunit.Size(last.size), humanunit.Size(opts.RemoteSize))
	default:
		return "the checksum differs on every attempt"
	}
}

func (tm *TransferManager) removePartial(target TargetFS, partialPath string) {
	if err := target.Remove(partialPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		tm.logger.Warn("couldn't remove a leftover download file",
			slog.String("path", partialPath),
			slog.Any("error", err))
	}
}

// DownloadPartialPath is where an interrupted download of targetPath keeps
// its bytes, so callers can point users at it. The name carries the remote
// version (its QuickXorHash, or its eTag for a hashless item), so a partial
// is only ever resumed against the version it was started from.
func DownloadPartialPath(targetPath string, opts DownloadOpts) string {
	return filepath.Join(filepath.Dir(targetPath), downloadPartialName(filepath.Base(targetPath), partialVersionTag(opts)))
}

// singleUsePartialTag starts the tag of a single-use partial's name
// (partialPathFor), after the version tag.
const singleUsePartialTag = "~a"

// partialPathFor is where a download of targetPath writes: the file's own
// partial (DownloadPartialPath), which a later attempt resumes, unless a
// write abandoned on a hung disk still holds it (partialSink). Then it is a
// single-use partial of another name, which no attempt resumes; resumable
// says which. The single-use name keeps the owned prefix and suffix, so a
// leftover one is a stale partial like any other.
func (tm *TransferManager) partialPathFor(targetPath string, opts DownloadOpts) (path string, resumable bool) {
	own := DownloadPartialPath(targetPath, opts)

	tm.partialsMu.Lock()
	defer tm.partialsMu.Unlock()

	if _, held := tm.heldPartials[own]; !held {
		return own, true
	}

	for {
		tm.singleUsePartials++
		tag := partialVersionTag(opts) + singleUsePartialTag + strconv.FormatUint(tm.singleUsePartials, 10)
		path = filepath.Join(filepath.Dir(targetPath), downloadPartialName(filepath.Base(targetPath), tag))
		if _, held := tm.heldPartials[path]; !held {
			return path, false
		}
	}
}

// holdPartial records that an abandoned write still holds path.
func (tm *TransferManager) holdPartial(path string) {
	tm.partialsMu.Lock()
	defer tm.partialsMu.Unlock()

	if tm.heldPartials == nil {
		tm.heldPartials = make(map[string]struct{})
	}
	tm.heldPartials[path] = struct{}{}
}

// releasePartial records that the write that held path has returned.
func (tm *TransferManager) releasePartial(path string) {
	tm.partialsMu.Lock()
	defer tm.partialsMu.Unlock()

	delete(tm.heldPartials, path)
}

// errPartialDisowned is what a write to a partial returns once the partial
// has been given up to a write stuck in it.
var errPartialDisowned = errors.New("download: the partial file was given up to a write stuck in it")

// partialSink is the local side of one download attempt: the partial file
// it writes. The graph client abandons a write to it that hangs past the
// stall timeout (graph.AbandonableWriter), and the stuck write then owns the
// file.
//
// Invariant: no two writes ever land in one partial. The stuck write keeps
// its descriptor and lands its bytes at its own offset whenever it returns,
// so a retry that appended to the same partial (a resume) or truncated it (a
// fresh start) would carry the stuck write's bytes inside its own, and for a
// hashless item nothing would catch it. So an abandoned sink disowns its
// partial: Abandon holds the name (TransferManager.heldPartials), the next
// attempt writes a single-use partial of another name (partialPathFor), and
// once the stuck write has returned its goroutine closes the file, removes
// the partial and frees the name (AbandonedWriteReturned). The name is freed
// last, so no attempt can take it while the file is still being finished.
type partialSink struct {
	w      io.Writer
	file   *os.File
	path   string
	target TargetFS
	tm     *TransferManager

	abandoned atomic.Bool
}

var _ graph.AbandonableWriter = (*partialSink)(nil)

func (tm *TransferManager) newPartialSink(target TargetFS, f *os.File, path string, w io.Writer) *partialSink {
	return &partialSink{w: w, file: f, path: path, target: target, tm: tm}
}

// Write writes the partial, and refuses once the partial has been given up:
// only the stuck write, already inside the file, may still land.
func (s *partialSink) Write(p []byte) (int, error) {
	if s.abandoned.Load() {
		return 0, errPartialDisowned
	}

	return s.w.Write(p) //nolint:wrapcheck // the file's own error; the download wraps it
}

// Name names the partial, for the graph client's record of abandoned calls.
func (s *partialSink) Name() string { return s.path }

// Abandon disowns the partial: a write is stuck in it.
func (s *partialSink) Abandon() {
	s.abandoned.Store(true)
	s.tm.holdPartial(s.path)
}

// AbandonedWriteReturned runs on the abandoned write's goroutine once the
// stuck write has returned: the partial holds bytes no attempt owns, so it
// is closed and removed, and its name is free again.
func (s *partialSink) AbandonedWriteReturned() {
	defer s.tm.releasePartial(s.path)

	if err := s.file.Close(); err != nil {
		s.tm.logger.Debug("closing a partial file given up to a stuck write",
			slog.String("path", s.path), slog.Any("error", err))
	}
	s.tm.removePartial(s.target, s.path)
}

func (s *partialSink) isAbandoned() bool { return s.abandoned.Load() }

// failure is the error an abandoned attempt returns: the download's, or,
// should a downloader have returned none, errPartialDisowned.
func (s *partialSink) failure(err error) error {
	if err != nil {
		return err
	}

	return errPartialDisowned
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
//
// resumable says partialPath is the file's own partial; a single-use one
// (partialPathFor) is never resumed, and a failed attempt removes it.
func (tm *TransferManager) downloadToPartial(
	ctx context.Context, driveID driveid.ID, itemID, partialPath string, opts DownloadOpts, resumable, allowResume bool,
) (string, int64, error) {
	version := downloadVersion(opts)
	rd, canRange := tm.downloads.(RangeDownloader)

	if resumable && allowResume && canRange && (version.QuickXorHash != "" || version.ETag != "") {
		f, openErr := opts.target().OpenFile(partialPath, os.O_APPEND|os.O_WRONLY, downloadTempFilePerms)
		if openErr == nil {
			info, statErr := f.Stat()
			if statErr != nil || info.Size() == 0 {
				// Empty or unreadable — close and start fresh.
				if closeErr := f.Close(); closeErr != nil {
					tm.logger.Warn("failed to close partial file before fresh download",
						slog.String("path", partialPath),
						slog.Any("error", closeErr))
				}
			} else {
				return tm.resumeDownloadFromFile(ctx, driveID, itemID, rd, f, partialPath, info.Size(), opts)
			}
		} else if !errors.Is(openErr, os.ErrNotExist) {
			tm.logger.Warn("cannot open partial file for resume, starting fresh",
				slog.String("path", partialPath), slog.Any("error", openErr))
		}
	}

	return tm.freshDownload(ctx, driveID, itemID, partialPath, opts, resumable)
}

// removePartialIfNotCanceled removes a .partial file unless the context was
// canceled (preserving the partial for future resume). Extracted to deduplicate
// the identical pattern in freshDownload and resumeDownload.
func (tm *TransferManager) removePartialIfNotCanceled(ctx context.Context, target TargetFS, path string) {
	if ctx.Err() == nil {
		if removeErr := target.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			tm.logger.Warn("couldn't remove a leftover download file",
				slog.String("path", path),
				slog.Any("error", removeErr),
			)
		}
	}
}

// freshDownload performs a full download to a new .partial file. resumable
// is false for a single-use partial, which a failed attempt removes: no
// later attempt would resume it.
func (tm *TransferManager) freshDownload(
	ctx context.Context, driveID driveid.ID, itemID, partialPath string, opts DownloadOpts, resumable bool,
) (string, int64, error) {
	f, err := opts.target().OpenFile(partialPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, downloadTempFilePerms)
	if err != nil {
		return "", 0, fmt.Errorf("creating the partial file: %w", err)
	}

	h := quickxorhash.New()
	sink := tm.newPartialSink(opts.target(), f, partialPath, io.MultiWriter(f, h))

	size, err := tm.downloads.Download(ctx, driveID, itemID, sink)
	if sink.isAbandoned() {
		// The stuck write owns the file and the partial now (partialSink):
		// neither is closed, removed or kept here.
		return "", 0, fmt.Errorf("receiving the file: %w", sink.failure(err))
	}
	if errors.Is(err, graph.ErrNoDownloadURL) && opts.RemoteSize == 0 {
		// Business omits the download URL of some empty files. An empty
		// file has nothing to fetch, so the truncated partial is the file
		// (RCL-053); without this every such item failed forever.
		size, err = 0, nil
	}
	if err != nil {
		if closeErr := f.Close(); closeErr != nil {
			tm.logger.Warn("failed to close partial file after download error",
				slog.String("path", partialPath), slog.Any("error", closeErr))
		}

		// Preserve the partial on cancellation, and on a stalled transfer,
		// so the retry resumes from it; a single-use partial is never
		// resumed, so it goes whatever the failure.
		switch {
		case !resumable:
			tm.removePartial(opts.target(), partialPath)
		case !errors.Is(err, graph.ErrTransferStalled):
			tm.removePartialIfNotCanceled(ctx, opts.target(), partialPath)
		}

		return "", 0, fmt.Errorf("receiving the file: %w", err)
	}

	if err := f.Close(); err != nil {
		// Close failure is always an error regardless of context — the file is corrupt.
		baseErr := fmt.Errorf("closing the partial file: %w", err)
		if removeErr := opts.target().Remove(partialPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return "", 0, errors.Join(baseErr, fmt.Errorf("removing the corrupt partial file: %w", removeErr))
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

	sink := tm.newPartialSink(opts.target(), f, partialPath, f)
	n, err := rd.DownloadRange(ctx, driveID, itemID, sink, existingSize, downloadVersion(opts))
	if sink.isAbandoned() {
		// The stuck write owns the file and the partial now (partialSink).
		return "", 0, fmt.Errorf("resuming the download: %w", sink.failure(err))
	}

	if closeErr := f.Close(); closeErr != nil {
		tm.logger.Warn("failed to close partial file after range download",
			slog.String("path", partialPath), slog.Any("error", closeErr))

		tm.removePartialIfNotCanceled(ctx, opts.target(), partialPath)

		return tm.freshDownload(ctx, driveID, itemID, partialPath, opts, true)
	}

	if err != nil {
		if !resumeRefused(err) {
			// A stall, a cancellation or a dropped connection: the partial
			// still holds verified-offset bytes of this version, so keep it
			// and let the retry resume again. Starting over here threw away
			// every resumed byte, so a link that stalls before a file
			// finishes never finished it.
			return "", 0, fmt.Errorf("resuming the download: %w", err)
		}

		// Debug only: a handled fallback. If the fresh download fails too,
		// its error is the report.
		tm.logger.Debug("range download refused, falling back to fresh download",
			slog.String("path", partialPath), slog.Any("error", err))

		tm.removePartialIfNotCanceled(ctx, opts.target(), partialPath)

		return tm.freshDownload(ctx, driveID, itemID, partialPath, opts, true)
	}

	totalSize := existingSize + n

	localHash, err := hashTargetFile(opts.target(), partialPath)
	if err != nil {
		tm.removePartialIfNotCanceled(ctx, opts.target(), partialPath)

		return "", 0, fmt.Errorf("hashing the resumed download: %w", err)
	}

	return localHash, totalSize, nil
}

// resumeRefused reports a resume the server cannot serve from this partial:
// the version moved on, the Range was not honored, or the range is not
// satisfiable. Only these start the download over.
func resumeRefused(err error) bool {
	return errors.Is(err, graph.ErrRangeResumeRejected) || errors.Is(err, graph.ErrRangeNotSatisfiable)
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
func (tm *TransferManager) checkDiskSpace(ctx context.Context, target TargetFS, targetPath string, fileSize int64) error {
	if tm.diskAvailableFunc == nil || tm.minFreeSpace == 0 {
		return nil
	}

	// Check the filesystem containing the target's parent directory.
	absTarget, err := target.Abs(targetPath)
	if err != nil {
		return fmt.Errorf("checking free space: %w", err)
	}
	checkPath := filepath.Dir(absTarget)

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
			slog.Any("error", err),
		)

		return nil
	}

	minFree := uint64(tm.minFreeSpace)
	if available < minFree {
		return fmt.Errorf(
			"%w: %s free, min_free_space requires %s; free up space or lower min_free_space in the config",
			ErrDiskFull, humanunit.Size(clampToInt64(available)), humanunit.Size(tm.minFreeSpace))
	}

	// Per-file check: can this specific file fit within available - minFreeSpace?
	if fileSize > 0 && available < uint64(fileSize)+minFree {
		return fmt.Errorf(
			"%w: the file needs %s but only %s is free after keeping min_free_space (%s) in reserve",
			ErrFileTooLargeForSpace, humanunit.Size(fileSize), humanunit.Size(clampToInt64(available-minFree)),
			humanunit.Size(tm.minFreeSpace))
	}

	return nil
}
