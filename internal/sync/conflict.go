package sync

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxConflictSuffix is the upper bound on the numeric suffix tried during
// conflict-path collision avoidance. Exceeding 1000 collisions is implausible
// in practice; if it happens the timestamp-only base path is returned as a
// best-effort fallback.
const maxConflictSuffix = 1000

// ConflictHandler resolves sync conflicts using the keep-both policy.
// It performs filesystem operations (renames) and returns sub-actions
// (downloads/uploads) for the executor to dispatch. The handler is stateless:
// it does not write to the store directly. Resolved ConflictRecords are
// returned to the executor, which records them.
type ConflictHandler struct {
	syncRoot string
	logger   *slog.Logger
}

// NewConflictHandler creates a ConflictHandler for the given sync root directory.
func NewConflictHandler(syncRoot string, logger *slog.Logger) *ConflictHandler {
	if logger == nil {
		logger = slog.Default()
	}

	return &ConflictHandler{
		syncRoot: syncRoot,
		logger:   logger,
	}
}

// ResolveResult holds the outcome of conflict resolution.
type ResolveResult struct {
	// Record is the resolved ConflictRecord (Resolution = ConflictKeepBoth, ResolvedBy = auto).
	Record *ConflictRecord
	// SubActions are downloads/uploads that the executor must dispatch to complete the resolution.
	SubActions []Action
}

// Resolve applies keep-both conflict resolution to the given conflict action.
// The resolution strategy depends on the conflict type tagged by the reconciler:
//
//   - ConflictEditEdit / ConflictCreateCreate: rename local file to a timestamped
//     conflict copy, then download the remote version to the original path.
//   - ConflictEditDelete: keep the local file and re-upload it to the remote
//     (the remote item was tombstoned, but the local edit should win).
//
// Any other conflict type is rejected with an error.
func (h *ConflictHandler) Resolve(_ context.Context, action *Action) (*ResolveResult, error) {
	if action.ConflictInfo == nil {
		return nil, fmt.Errorf("conflict handler: action for %q has nil ConflictInfo", action.Path)
	}

	if action.Item == nil {
		return nil, fmt.Errorf("conflict handler: action for %q has nil Item", action.Path)
	}

	h.logger.Info("conflict handler: resolving",
		"path", action.Path,
		"type", action.ConflictInfo.Type,
	)

	switch action.ConflictInfo.Type {
	case ConflictEditEdit, ConflictCreateCreate:
		return h.resolveKeepBothDownload(action)
	case ConflictEditDelete:
		return h.resolveKeepBothUpload(action)
	default:
		return nil, fmt.Errorf("conflict handler: unknown conflict type %q for %q", action.ConflictInfo.Type, action.Path)
	}
}

// resolveKeepBothDownload handles edit-edit (F5) and create-create (F11) conflicts.
// The local file is renamed to a timestamped conflict copy so no work is lost,
// then a download sub-action fetches the authoritative remote version.
func (h *ConflictHandler) resolveKeepBothDownload(action *Action) (*ResolveResult, error) {
	localPath := filepath.Join(h.syncRoot, action.Path)
	conflictPath := generateConflictPath(localPath)

	h.logger.Info("conflict handler: backing up local file",
		"path", action.Path,
		"conflict_path", conflictPath,
	)

	if err := os.Rename(localPath, conflictPath); err != nil {
		return nil, fmt.Errorf("conflict handler: rename %q to conflict copy: %w", action.Path, err)
	}

	now := NowNano()
	resolvedBy := ResolvedByAuto

	return &ResolveResult{
		Record: buildConflictRecord(action, now, &resolvedBy),
		SubActions: []Action{{
			Type:    ActionDownload,
			DriveID: action.DriveID,
			ItemID:  action.ItemID,
			Path:    action.Path,
			Item:    action.Item,
		}},
	}, nil
}

// resolveKeepBothUpload handles edit-delete (F9) conflicts.
// The remote item was tombstoned, but the local file was edited — we re-upload
// the local version so the user's work is preserved on both sides.
// No rename is needed: the local file stays at its current path.
func (h *ConflictHandler) resolveKeepBothUpload(action *Action) (*ResolveResult, error) {
	h.logger.Info("conflict handler: re-uploading local file for edit-delete conflict",
		"path", action.Path,
	)

	now := NowNano()
	resolvedBy := ResolvedByAuto

	return &ResolveResult{
		Record: buildConflictRecord(action, now, &resolvedBy),
		SubActions: []Action{{
			Type:    ActionUpload,
			DriveID: action.DriveID,
			ItemID:  action.ItemID,
			Path:    action.Path,
			Item:    action.Item,
		}},
	}, nil
}

// buildConflictRecord constructs a resolved ConflictRecord from the action fields.
// Resolution is always ConflictKeepBoth for automatic resolution.
func buildConflictRecord(action *Action, now int64, resolvedBy *ConflictResolvedBy) *ConflictRecord {
	info := action.ConflictInfo

	return &ConflictRecord{
		ID:          fmt.Sprintf("conflict-%d", now),
		DriveID:     info.DriveID,
		ItemID:      info.ItemID,
		Path:        info.Path,
		DetectedAt:  now,
		LocalHash:   info.LocalHash,
		RemoteHash:  info.RemoteHash,
		LocalMtime:  info.LocalMtime,
		RemoteMtime: info.RemoteMtime,
		Resolution:  ConflictKeepBoth,
		ResolvedAt:  Int64Ptr(now),
		ResolvedBy:  resolvedBy,
		Type:        info.Type,
	}
}

// generateConflictPath creates a conflict copy path using timestamp-based naming.
// Pattern: <stem>.conflict-<YYYYMMDD-HHMMSS><ext>
// Examples:
//   - report.docx  →  report.conflict-20260221-143052.docx
//   - .bashrc      →  .bashrc.conflict-20260221-143052
//   - Makefile     →  Makefile.conflict-20260221-143052
//
// Dotfiles like ".bashrc" are handled specially: Go's filepath.Ext treats the entire
// name as the extension for files whose only dot is the leading one, which would yield
// the wrong ".conflict-TIMESTAMP.bashrc" pattern. We detect this and treat the
// extension as empty, so the suffix is appended to the full dotfile name.
//
// Collision avoidance appends a numeric suffix (-1, -2, ...) up to maxConflictSuffix.
// If all candidates are taken, the base (no suffix) path is returned as a fallback.
func generateConflictPath(originalPath string) string {
	stem, ext := conflictStemExt(originalPath)
	ts := time.Now().UTC().Format("20060102-150405")

	base := stem + ".conflict-" + ts + ext
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return base
	}

	// Collision avoidance: append numeric suffix until a free slot is found.
	for i := 1; i <= maxConflictSuffix; i++ {
		candidate := fmt.Sprintf("%s.conflict-%s-%d%s", stem, ts, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}

	// Fallback: return the base path; the rename will overwrite if it exists.
	return base
}

// conflictStemExt splits originalPath into a (stem, ext) pair suitable for
// conflict-path generation. Dotfiles with no embedded extension (e.g., ".bashrc")
// are treated as having an empty extension so the conflict suffix is appended to
// the full filename rather than before the leading dot.
func conflictStemExt(originalPath string) (stem, ext string) {
	base := filepath.Base(originalPath)
	dir := originalPath[:len(originalPath)-len(base)] // preserve trailing separator

	// Dotfile: base starts with "." and the only dot is the leading one.
	// filepath.Ext would return the entire base as the extension — wrong for our use.
	if strings.HasPrefix(base, ".") && strings.Count(base, ".") == 1 {
		return dir + base, ""
	}

	ext = filepath.Ext(base)
	stem = dir + base[:len(base)-len(ext)]

	return stem, ext
}
