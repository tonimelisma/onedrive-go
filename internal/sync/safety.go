package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// Safety invariant error sentinels.
var (
	// ErrBigDeleteBlocked is returned when a sync cycle would delete more items
	// than the configured safety thresholds allow. Use --force to override.
	ErrBigDeleteBlocked = errors.New("big-delete protection triggered")

	// ErrInsufficientDiskSpace is returned when completing all planned downloads
	// would reduce available disk space below the configured minimum.
	ErrInsufficientDiskSpace = errors.New("insufficient disk space")
)

// percentMultiplier converts a count to a percentage (multiply before dividing to avoid integer truncation).
const percentMultiplier = 100

// SafetyChecker validates an ActionPlan against the seven safety invariants (S1-S7)
// before any destructive operations are executed. It is the pre-execution gate
// described in sync-algorithm.md section 8.
type SafetyChecker struct {
	store      Store
	cfg        *config.SafetyConfig
	logger     *slog.Logger
	syncRoot   string
	statfsFunc func(path string) (uint64, error) // injectable for testing disk space
}

// NewSafetyChecker creates a SafetyChecker with the given store, config, sync root,
// and logger. The sync root is needed for disk space checks (S6). The default
// disk space function uses platform-specific syscalls (getDiskSpace).
func NewSafetyChecker(
	store Store,
	cfg *config.SafetyConfig,
	syncRoot string,
	logger *slog.Logger,
) *SafetyChecker {
	if logger == nil {
		logger = slog.Default()
	}

	return &SafetyChecker{
		store:      store,
		cfg:        cfg,
		logger:     logger,
		syncRoot:   syncRoot,
		statfsFunc: getDiskSpace,
	}
}

// Check validates the action plan against all seven safety invariants.
// It may modify the plan (removing unsafe actions) and returns the validated plan.
// When force is true, big-delete protection (S5) logs a warning instead of blocking.
// When dryRun is true, safety violations are logged as warnings but never returned as errors.
func (sc *SafetyChecker) Check(
	ctx context.Context,
	plan *ActionPlan,
	force, dryRun bool,
) (*ActionPlan, error) {
	sc.logger.Info("safety check started",
		"total_actions", plan.TotalActions(),
		"force", force,
		"dry_run", dryRun,
	)

	// S1: Never delete remote from local absence.
	plan.RemoteDeletes = filterBySyncedHash(sc.logger, "S1", plan.RemoteDeletes)

	// S2: No deletions from incomplete delta.
	if err := sc.checkS2IncompleteDelta(ctx, plan); err != nil {
		return plan, err
	}

	// S3: Atomic file writes (plan-level consistency check).
	sc.checkS3AtomicWrites(plan)

	// S4: Hash-before-delete guard (SyncedHash presence check at plan time).
	plan.LocalDeletes = filterBySyncedHash(sc.logger, "S4", plan.LocalDeletes)

	// S5: Big-delete protection.
	if err := sc.checkS5BigDelete(ctx, plan, force, dryRun); err != nil {
		return plan, err
	}

	// S6: Disk space check.
	if err := sc.checkS6DiskSpace(plan, dryRun); err != nil {
		return plan, err
	}

	// S7: No temp/partial uploads.
	sc.checkS7TempUploads(plan)

	sc.logger.Info("safety check passed",
		"remaining_actions", plan.TotalActions(),
	)

	return plan, nil
}

// filterBySyncedHash is the plan-time pre-check for S1/S4: it removes delete actions
// whose Item is nil or has an empty SyncedHash. This pre-check ensures only items with a
// confirmed synced baseline may be deleted. It is distinct from the execution-time
// hash-before-delete check (S4), where the executor verifies that the local file's current
// content hash still matches SyncedHash before actually deleting.
func filterBySyncedHash(logger *slog.Logger, invariant string, actions []Action) []Action {
	var kept []Action

	for i := range actions {
		action := &actions[i]
		if action.Item == nil || action.Item.SyncedHash == "" {
			logger.Warn(invariant+": removed delete without SyncedHash",
				"path", action.Path,
				"item_id", action.ItemID,
			)

			continue
		}

		kept = append(kept, *action)
	}

	removed := len(actions) - len(kept)
	if removed > 0 {
		logger.Warn(invariant+": suppressed deletes without SyncedHash",
			"removed", removed,
		)
	}

	return kept
}

// checkS2IncompleteDelta enforces S2: never process local deletions when the delta
// enumeration was incomplete. Incomplete deltas mean we cannot trust that items
// missing from the remote are truly deleted — they may simply not have been enumerated.
func (sc *SafetyChecker) checkS2IncompleteDelta(ctx context.Context, plan *ActionPlan) error {
	if len(plan.LocalDeletes) == 0 {
		return nil
	}

	// Collect unique drive IDs from local delete actions.
	driveIDs := make(map[string]struct{})
	for i := range plan.LocalDeletes {
		driveIDs[plan.LocalDeletes[i].DriveID] = struct{}{}
	}

	// Check delta completeness for each drive.
	incompleteDrives := make(map[string]struct{})

	for driveID := range driveIDs {
		complete, err := sc.store.IsDeltaComplete(ctx, driveID)
		if err != nil {
			return fmt.Errorf("S2: check delta complete for drive %s: %w", driveID, err)
		}

		if !complete {
			incompleteDrives[driveID] = struct{}{}
		}
	}

	if len(incompleteDrives) == 0 {
		return nil
	}

	// Remove local deletes for drives with incomplete deltas.
	var kept []Action

	for i := range plan.LocalDeletes {
		action := &plan.LocalDeletes[i]
		if _, incomplete := incompleteDrives[action.DriveID]; incomplete {
			continue
		}

		kept = append(kept, *action)
	}

	removed := len(plan.LocalDeletes) - len(kept)
	sc.logger.Warn("S2: suppressed local deletes from incomplete delta",
		"removed", removed,
	)

	plan.LocalDeletes = kept

	return nil
}

// checkS3AtomicWrites enforces S3 at plan time: verify no download actions target
// .partial paths as final destinations. The actual atomic write (write to .partial
// then rename) is the executor's responsibility; this check catches plan-level errors.
func (sc *SafetyChecker) checkS3AtomicWrites(plan *ActionPlan) {
	for i := range plan.Downloads {
		action := &plan.Downloads[i]
		if strings.HasSuffix(strings.ToLower(action.Path), ".partial") {
			sc.logger.Warn("S3: download targets a .partial path",
				"path", action.Path,
				"item_id", action.ItemID,
			)
		}
	}
}

// checkS5BigDelete enforces S5: if a sync cycle would delete more items than either
// the absolute threshold or the percentage threshold, abort unless force is set.
func (sc *SafetyChecker) checkS5BigDelete(
	ctx context.Context,
	plan *ActionPlan,
	force, dryRun bool,
) error {
	deleteCount := plan.TotalDeletes()
	if deleteCount == 0 {
		return nil
	}

	items, err := sc.store.ListAllActiveItems(ctx)
	if err != nil {
		return fmt.Errorf("S5: list active items: %w", err)
	}

	totalItems := len(items)

	// Drives below the minimum item count skip big-delete protection.
	if totalItems < sc.cfg.BigDeleteMinItems {
		sc.logger.Debug("S5: drive below min items, skipping big-delete check",
			"total_items", totalItems,
			"min_items", sc.cfg.BigDeleteMinItems,
		)

		return nil
	}

	countExceeded := deleteCount > sc.cfg.BigDeleteThreshold

	var percentExceeded bool
	if totalItems > 0 {
		percentExceeded = (deleteCount * percentMultiplier / totalItems) > sc.cfg.BigDeletePercentage
	}

	if !countExceeded && !percentExceeded {
		return nil
	}

	return sc.handleBigDeleteViolation(deleteCount, totalItems, force, dryRun)
}

// handleBigDeleteViolation formats and handles a big-delete threshold violation.
func (sc *SafetyChecker) handleBigDeleteViolation(deleteCount, totalItems int, force, dryRun bool) error {
	msg := fmt.Sprintf(
		"would delete %d items (%d%% of %d total), thresholds: %d items or %d%%",
		deleteCount,
		deleteCount*percentMultiplier/totalItems,
		totalItems,
		sc.cfg.BigDeleteThreshold,
		sc.cfg.BigDeletePercentage,
	)

	if force {
		sc.logger.Warn("S5: big-delete override via --force", "detail", msg)
		return nil
	}

	if dryRun {
		sc.logger.Warn("S5: big-delete would block (dry-run)", "detail", msg)
		return nil
	}

	sc.logger.Error("S5: big-delete protection triggered", "detail", msg)

	return fmt.Errorf("%w: %s", ErrBigDeleteBlocked, msg)
}

// checkS6DiskSpace enforces S6: verify that planned downloads will not reduce
// available disk space below the configured minimum.
func (sc *SafetyChecker) checkS6DiskSpace(plan *ActionPlan, dryRun bool) error {
	if len(plan.Downloads) == 0 {
		return nil
	}

	totalDownloadSize := sumDownloadSize(plan)

	if totalDownloadSize == 0 {
		return nil
	}

	minFreeBytes, err := config.ParseSize(sc.cfg.MinFreeSpace)
	if err != nil {
		return fmt.Errorf("S6: parse min_free_space %q: %w", sc.cfg.MinFreeSpace, err)
	}

	// Skip check if min_free_space is zero (disabled).
	if minFreeBytes == 0 {
		return nil
	}

	available, err := sc.statfsFunc(sc.syncRoot)
	if err != nil {
		return fmt.Errorf("S6: get disk space for %q: %w", sc.syncRoot, err)
	}

	// Cap available to int64 max to prevent overflow when converting from uint64.
	availableI64 := int64(min(available, uint64(math.MaxInt64))) // capped at MaxInt64 to prevent overflow

	remainingAfterDownloads := availableI64 - totalDownloadSize
	if remainingAfterDownloads < minFreeBytes {
		return sc.handleDiskSpaceViolation(totalDownloadSize, available, remainingAfterDownloads, minFreeBytes, dryRun)
	}

	return nil
}

// sumDownloadSize totals the byte sizes of all download actions in the plan.
func sumDownloadSize(plan *ActionPlan) int64 {
	var total int64

	for i := range plan.Downloads {
		if plan.Downloads[i].Item != nil && plan.Downloads[i].Item.Size != nil {
			total += *plan.Downloads[i].Item.Size
		}
	}

	return total
}

// handleDiskSpaceViolation formats and handles a disk space violation.
func (sc *SafetyChecker) handleDiskSpaceViolation(
	needed int64,
	available uint64,
	remaining, minFree int64,
	dryRun bool,
) error {
	msg := fmt.Sprintf(
		"downloads need %d bytes, %d available, would leave %d (min %d required)",
		needed, available, remaining, minFree,
	)

	if dryRun {
		sc.logger.Warn("S6: insufficient disk space (dry-run)", "detail", msg)
		return nil
	}

	sc.logger.Error("S6: insufficient disk space", "detail", msg)

	return fmt.Errorf("%w: %s", ErrInsufficientDiskSpace, msg)
}

// checkS7TempUploads enforces S7: never upload temporary or partial files.
// Files matching .partial, .tmp, or ~* patterns are removed from the upload plan.
func (sc *SafetyChecker) checkS7TempUploads(plan *ActionPlan) {
	var kept []Action

	for i := range plan.Uploads {
		action := &plan.Uploads[i]
		name := filepath.Base(action.Path)

		if isTempFile(name) {
			sc.logger.Warn("S7: removed temp/partial file from uploads",
				"path", action.Path,
				"name", name,
			)

			continue
		}

		kept = append(kept, *action)
	}

	removed := len(plan.Uploads) - len(kept)
	if removed > 0 {
		sc.logger.Warn("S7: suppressed temp/partial uploads",
			"removed", removed,
		)
	}

	plan.Uploads = kept
}

// isTempFile checks whether a filename matches temporary/partial file patterns:
// .partial, .tmp, or ~* (tilde prefix).
func isTempFile(name string) bool {
	lower := strings.ToLower(name)

	if strings.HasSuffix(lower, ".partial") {
		return true
	}

	if strings.HasSuffix(lower, ".tmp") {
		return true
	}

	if strings.HasPrefix(name, "~") {
		return true
	}

	return false
}
