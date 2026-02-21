package sync

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	gosync "sync"

	"golang.org/x/sync/errgroup"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// Default worker counts when no config is provided.
const (
	defaultDownloadWorkers = 8
	defaultUploadWorkers   = 8
)

// TransferManager dispatches downloads and uploads through bounded worker pools
// with shared bandwidth limiting. It wraps the Executor's per-file handlers with
// parallel dispatch via errgroup.
type TransferManager struct {
	executor        *Executor
	downloadWorkers int
	uploadWorkers   int
	transferOrder   string
	limiter         *BandwidthLimiter
	logger          *slog.Logger
}

// NewTransferManager creates a manager from the resolved transfer config.
// If cfg is nil, defaults are used (8 workers each, unlimited bandwidth, default order).
func NewTransferManager(
	executor *Executor,
	cfg *config.TransfersConfig,
	logger *slog.Logger,
) (*TransferManager, error) {
	if logger == nil {
		logger = slog.Default()
	}

	dlWorkers := defaultDownloadWorkers
	ulWorkers := defaultUploadWorkers
	order := "default"
	bwLimit := "0"

	if cfg != nil {
		if cfg.ParallelDownloads > 0 {
			dlWorkers = cfg.ParallelDownloads
		}

		if cfg.ParallelUploads > 0 {
			ulWorkers = cfg.ParallelUploads
		}

		if cfg.TransferOrder != "" {
			order = cfg.TransferOrder
		}

		bwLimit = cfg.BandwidthLimit
	}

	limiter, err := NewBandwidthLimiter(bwLimit, logger)
	if err != nil {
		return nil, err
	}

	logger.Info("transfer: manager created",
		"download_workers", dlWorkers,
		"upload_workers", ulWorkers,
		"order", order,
		"bandwidth_limited", limiter != nil,
	)

	return &TransferManager{
		executor:        executor,
		downloadWorkers: dlWorkers,
		uploadWorkers:   ulWorkers,
		transferOrder:   order,
		limiter:         limiter,
		logger:          logger,
	}, nil
}

// DownloadAll dispatches all download actions through a bounded worker pool.
// Fatal errors abort all workers. Skip-tier errors are recorded in the report.
func (tm *TransferManager) DownloadAll(ctx context.Context, actions []Action, report *SyncReport) error {
	if len(actions) == 0 {
		return nil
	}

	sortActions(actions, tm.transferOrder)
	tm.logger.Info("transfer: starting downloads", "count", len(actions), "workers", tm.downloadWorkers)

	return tm.dispatchPool(ctx, actions, report, tm.downloadWorkers, tm.executeDownloadAction)
}

// UploadAll dispatches all upload actions through a bounded worker pool.
func (tm *TransferManager) UploadAll(ctx context.Context, actions []Action, report *SyncReport) error {
	if len(actions) == 0 {
		return nil
	}

	sortActions(actions, tm.transferOrder)
	tm.logger.Info("transfer: starting uploads", "count", len(actions), "workers", tm.uploadWorkers)

	return tm.dispatchPool(ctx, actions, report, tm.uploadWorkers, tm.executeUploadAction)
}

// Close releases TransferManager-owned resources. Currently a no-op because all
// transfer work happens synchronously within DownloadAll/UploadAll — there are no
// background goroutines to stop. Callers (Engine.Close) must ensure that all
// DownloadAll/UploadAll calls have returned before calling Close; Engine enforces
// this via its WaitGroup. RunWatch (Phase 5) will add background goroutines and
// make Close meaningful.
func (tm *TransferManager) Close() {
	tm.logger.Debug("transfer: manager closed")
}

// actionResult holds the outcome of a single transfer action for thread-safe report updates.
type actionResult struct {
	bytes int64
	err   error
}

// dispatchPool runs handler for each action through a bounded errgroup.
// Fatal errors cancel remaining workers. Skip-tier errors are recorded.
func (tm *TransferManager) dispatchPool(
	ctx context.Context,
	actions []Action,
	report *SyncReport,
	workers int,
	handler func(context.Context, *Action) actionResult,
) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(workers)

	var mu gosync.Mutex

	for i := range actions {
		action := &actions[i]
		g.Go(func() error {
			result := handler(gctx, action)
			if result.err != nil {
				tier := classifyError(result.err)
				if tier == ErrorFatal {
					return result.err
				}

				mu.Lock()
				report.Skipped++
				report.Errors = append(report.Errors, ActionError{
					Action: *action,
					Err:    result.err,
					Tier:   tier,
				})
				mu.Unlock()

				tm.logger.Warn("transfer: action skipped",
					"path", action.Path,
					"error", result.err,
				)

				return nil // skip-tier: continue other workers
			}

			mu.Lock()
			updateReportSuccess(report, action.Type, result.bytes)
			mu.Unlock()

			return nil
		})
	}

	return g.Wait()
}

// executeDownloadAction wraps the executor's download handler for pool dispatch.
func (tm *TransferManager) executeDownloadAction(ctx context.Context, action *Action) actionResult {
	n, err := tm.executor.executeDownload(ctx, action)
	return actionResult{bytes: n, err: err}
}

// executeUploadAction wraps the executor's upload handler for pool dispatch.
func (tm *TransferManager) executeUploadAction(ctx context.Context, action *Action) actionResult {
	n, err := tm.executor.executeUpload(ctx, action)
	return actionResult{bytes: n, err: err}
}

// updateReportSuccess increments the appropriate counters for a completed transfer.
func updateReportSuccess(report *SyncReport, actionType ActionType, bytes int64) {
	switch actionType { //nolint:exhaustive // only download/upload actions use the transfer pool
	case ActionDownload:
		report.Downloaded++
		report.BytesDownloaded += bytes
	case ActionUpload:
		report.Uploaded++
		report.BytesUploaded += bytes
	}
}

// sortActions sorts actions in place according to the configured transfer order.
// Supported orders: "default" (no sort), "size_asc", "size_desc", "name_asc", "name_desc".
func sortActions(actions []Action, order string) {
	switch order {
	case "size_asc":
		slices.SortFunc(actions, func(a, b Action) int {
			return cmp.Compare(actionSize(&a), actionSize(&b))
		})
	case "size_desc":
		slices.SortFunc(actions, func(a, b Action) int {
			return cmp.Compare(actionSize(&b), actionSize(&a))
		})
	case "name_asc":
		slices.SortFunc(actions, func(a, b Action) int {
			return cmp.Compare(a.Path, b.Path)
		})
	case "name_desc":
		slices.SortFunc(actions, func(a, b Action) int {
			return cmp.Compare(b.Path, a.Path)
		})
	default:
		// "default" or unrecognized: preserve original order.
	}
}

// actionSize returns the file size for sorting. Returns 0 if size is unknown.
func actionSize(a *Action) int64 {
	if a.Item == nil || a.Item.Size == nil {
		return 0
	}

	return *a.Item.Size
}
