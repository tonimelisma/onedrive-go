package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	diskScopeInitialTrialInterval = 5 * time.Minute
	diskScopeMaxTrialInterval     = 1 * time.Hour
)

type scopeStartupPolicy int

const (
	scopeStartupRequiresBoundary scopeStartupPolicy = iota + 1
	scopeStartupRequiresScopedFailures
	scopeStartupServerTimedOnly
	scopeStartupRevalidateDisk
	scopeStartupRevalidateAuth
)

type persistedScopeFacts struct {
	boundaryKeys        map[synctypes.ScopeKey]bool
	failureCountByScope map[synctypes.ScopeKey]int
}

// loadActiveScopes refreshes watch runtime scope state from the persisted
// scope_blocks table. The store remains the restart/recovery record; watch
// mode keeps only the current working set in memory.
func (controller *scopeController) loadActiveScopes(ctx context.Context, watch *watchRuntime) error {
	flow := controller.flow

	if watch == nil {
		return nil
	}

	blocks, err := flow.engine.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing active scopes: %w", err)
	}

	activeScopes := make([]syncstore.ScopeBlock, 0, len(blocks))
	for i := range blocks {
		if blocks[i].Key.IsPermRemote() {
			continue
		}
		activeScopes = append(activeScopes, *blocks[i])
	}

	remoteHeld, err := flow.engine.baseline.ListRemoteBlockedFailures(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing remote blocked failures: %w", err)
	}
	seenRemote := make(map[synctypes.ScopeKey]bool, len(remoteHeld))
	for i := range remoteHeld {
		if !remoteHeld[i].ScopeKey.IsPermRemote() || seenRemote[remoteHeld[i].ScopeKey] {
			continue
		}
		seenRemote[remoteHeld[i].ScopeKey] = true
		activeScopes = append(activeScopes, syncstore.ScopeBlock{
			Key:       remoteHeld[i].ScopeKey,
			IssueType: remoteHeld[i].ScopeKey.IssueType(),
		})
	}
	watch.replaceActiveScopes(activeScopes)

	return nil
}

// repairPersistedScopes normalizes persisted scope rows against current store
// evidence before any admission begins. The store remains authoritative for
// restart state; watch mode loads activeScopes only after this repair pass.
func (controller *scopeController) repairPersistedScopes(
	ctx context.Context,
	watch *watchRuntime,
	proof driveIdentityProof,
	proofErr error,
) error {
	flow := controller.flow

	blocks, listScopeErr := flow.engine.baseline.ListScopeBlocks(ctx)
	if listScopeErr != nil {
		return fmt.Errorf("sync: listing scope blocks: %w", listScopeErr)
	}
	if err := controller.dropLegacyRemoteScopes(ctx, blocks); err != nil {
		return err
	}
	releasedLegacyThrottle, err := controller.releaseLegacyThrottleAccountScopes(ctx, blocks)
	if err != nil {
		return err
	}
	if releasedLegacyThrottle {
		blocks, listScopeErr = flow.engine.baseline.ListScopeBlocks(ctx)
		if listScopeErr != nil {
			return fmt.Errorf("sync: relisting scope blocks after throttle repair: %w", listScopeErr)
		}
	}
	if repairErr := controller.repairPersistedAuthScope(ctx, blocks, proof, proofErr); repairErr != nil {
		return repairErr
	}

	failures, err := controller.loadRepairedPersistedFailures(ctx)
	if err != nil {
		return err
	}
	if err := controller.repairPersistedNonAuthScopes(ctx, blocks, failures, proof, proofErr); err != nil {
		return err
	}

	flow.mustAssertInvariants(ctx, watch, "repair persisted scopes")

	return nil
}

func (controller *scopeController) dropLegacyRemoteScopes(
	ctx context.Context,
	blocks []*syncstore.ScopeBlock,
) error {
	flow := controller.flow

	for i := range blocks {
		if !blocks[i].Key.IsPermRemote() {
			continue
		}
		if err := flow.engine.baseline.DropLegacyRemoteBlockedScope(ctx, blocks[i].Key); err != nil {
			return fmt.Errorf("sync: dropping legacy remote scope %s: %w", blocks[i].Key.String(), err)
		}
	}

	return nil
}

func (controller *scopeController) releaseLegacyThrottleAccountScopes(
	ctx context.Context,
	blocks []*syncstore.ScopeBlock,
) (bool, error) {
	released := false

	for i := range blocks {
		if blocks[i].Key != synctypes.SKThrottleAccount() {
			continue
		}
		if err := controller.releaseStartupScope(ctx, blocks[i].Key, "released legacy account-wide throttle scope"); err != nil {
			return false, err
		}
		released = true
	}

	return released, nil
}

func (controller *scopeController) repairPersistedAuthScope(
	ctx context.Context,
	blocks []*syncstore.ScopeBlock,
	proof driveIdentityProof,
	proofErr error,
) error {
	for i := range blocks {
		if blocks[i].Key != synctypes.SKAuthAccount() {
			continue
		}

		return controller.repairPersistedScope(ctx, blocks[i], persistedScopeFacts{}, proof, proofErr)
	}

	return nil
}

func (controller *scopeController) loadRepairedPersistedFailures(
	ctx context.Context,
) ([]syncstore.SyncFailureRow, error) {
	flow := controller.flow

	failures, err := flow.engine.baseline.ListSyncFailures(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing sync failures: %w", err)
	}
	if !controller.clearResolvedPersistedRemoteFailures(ctx, failures) {
		return failures, nil
	}

	failures, err = flow.engine.baseline.ListSyncFailures(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: relisting sync failures after remote cleanup: %w", err)
	}

	return failures, nil
}

func (controller *scopeController) clearResolvedPersistedRemoteFailures(
	ctx context.Context,
	failures []syncstore.SyncFailureRow,
) bool {
	flow := controller.flow

	remoteResolved := false
	for i := range failures {
		if failures[i].Role != synctypes.FailureRoleHeld || !failures[i].ScopeKey.IsPermRemote() {
			continue
		}
		if flow.isFailureResolved(ctx, &failures[i]) {
			remoteResolved = true
		}
	}

	return remoteResolved
}

func (controller *scopeController) repairPersistedNonAuthScopes(
	ctx context.Context,
	blocks []*syncstore.ScopeBlock,
	failures []syncstore.SyncFailureRow,
	proof driveIdentityProof,
	proofErr error,
) error {
	facts := summarizePersistedScopeFailures(failures)

	for i := range blocks {
		if blocks[i].Key == synctypes.SKAuthAccount() || blocks[i].Key.IsPermRemote() {
			continue
		}

		if err := controller.repairPersistedScope(ctx, blocks[i], facts, proof, proofErr); err != nil {
			return err
		}
	}

	return nil
}

func (controller *scopeController) repairPersistedScope(
	ctx context.Context,
	block *syncstore.ScopeBlock,
	facts persistedScopeFacts,
	proof driveIdentityProof,
	proofErr error,
) error {
	now := controller.flow.engine.nowFunc()

	switch scopeStartupPolicyFor(block.Key) {
	case scopeStartupRevalidateAuth:
		return controller.repairAuthScope(ctx, block, proof, proofErr)
	case scopeStartupRequiresBoundary:
		if facts.boundaryKeys[block.Key] {
			return nil
		}
		return controller.releaseStartupScope(ctx, block.Key, "released scope without boundary evidence")
	case scopeStartupRequiresScopedFailures:
		if facts.failureCountByScope[block.Key] > 0 {
			return nil
		}
		if hasActivePreserveDeadline(block, now) {
			return nil
		}
		return controller.discardStartupScope(ctx, block.Key, "discarded scope without scoped failures")
	case scopeStartupServerTimedOnly:
		if block.TimingSource == synctypes.ScopeTimingServerRetryAfter {
			return nil
		}
		return controller.releaseStartupScope(ctx, block.Key, "released non-server-timed restart scope")
	case scopeStartupRevalidateDisk:
		return controller.repairDiskScope(ctx, block)
	default:
		panic(fmt.Sprintf("unknown startup policy %d", scopeStartupPolicyFor(block.Key)))
	}
}

func (controller *scopeController) releaseStartupScope(ctx context.Context, key synctypes.ScopeKey, note string) error {
	flow := controller.flow

	if err := flow.engine.baseline.ReleaseScope(ctx, key, flow.engine.nowFunc()); err != nil {
		return fmt.Errorf("sync: releasing startup scope %s: %w", key.String(), err)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventStartupScopeRepaired,
		ScopeKey: key,
		Note:     note,
	})
	return nil
}

func (controller *scopeController) discardStartupScope(ctx context.Context, key synctypes.ScopeKey, note string) error {
	flow := controller.flow

	if err := flow.engine.baseline.DiscardScope(ctx, key); err != nil {
		return fmt.Errorf("sync: discarding startup scope %s: %w", key.String(), err)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventStartupScopeRepaired,
		ScopeKey: key,
		Note:     note,
	})
	return nil
}

func summarizePersistedScopeFailures(rows []syncstore.SyncFailureRow) persistedScopeFacts {
	facts := persistedScopeFacts{
		boundaryKeys:        make(map[synctypes.ScopeKey]bool),
		failureCountByScope: make(map[synctypes.ScopeKey]int),
	}

	for i := range rows {
		if rows[i].ScopeKey.IsZero() {
			continue
		}
		facts.failureCountByScope[rows[i].ScopeKey]++
		if rows[i].Role == synctypes.FailureRoleBoundary {
			facts.boundaryKeys[rows[i].ScopeKey] = true
		}
	}

	return facts
}

func scopeStartupPolicyFor(key synctypes.ScopeKey) scopeStartupPolicy {
	switch {
	case key == synctypes.SKAuthAccount():
		return scopeStartupRevalidateAuth
	case key.IsPermDir(), key.IsPermRemote():
		return scopeStartupRequiresBoundary
	case key == synctypes.SKQuotaOwn() || key.Kind == synctypes.ScopeQuotaShortcut:
		return scopeStartupRequiresScopedFailures
	case key.IsThrottleTarget(), key == synctypes.SKThrottleAccount(), key == synctypes.SKService():
		return scopeStartupServerTimedOnly
	case key == synctypes.SKDiskLocal():
		return scopeStartupRevalidateDisk
	default:
		return scopeStartupRequiresScopedFailures
	}
}

func hasActivePreserveDeadline(block *syncstore.ScopeBlock, now time.Time) bool {
	if block == nil || block.PreserveUntil.IsZero() {
		return false
	}

	return block.PreserveUntil.After(now)
}

func (controller *scopeController) repairAuthScope(
	ctx context.Context,
	block *syncstore.ScopeBlock,
	proof driveIdentityProof,
	proofErr error,
) error {
	// Auth scope repair is deliberately proof-based. Token-source creation or
	// session construction does not prove that the current credentials still
	// authorize the configured drive.
	if !proof.attempted {
		return fmt.Errorf("sync: revalidating auth scope %s: drive verifier required", block.Key.String())
	}

	if proofErr == nil {
		return controller.releaseStartupScope(ctx, block.Key, "released auth scope after successful proof")
	}

	if errors.Is(proofErr, graph.ErrUnauthorized) {
		controller.flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventStartupScopeRepaired,
			ScopeKey: block.Key,
			Note:     "kept auth scope after unauthorized proof",
		})
	}

	return proofErr
}

func (controller *scopeController) repairDiskScope(ctx context.Context, block *syncstore.ScopeBlock) error {
	flow := controller.flow

	if flow.engine.minFreeSpace <= 0 {
		if err := flow.engine.baseline.ReleaseScope(ctx, block.Key, flow.engine.nowFunc()); err != nil {
			return fmt.Errorf("sync: releasing disk scope %s with disabled min_free_space: %w", block.Key.String(), err)
		}
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventStartupScopeRepaired,
			ScopeKey: block.Key,
			Note:     "released disk scope with disabled min_free_space",
		})
		return nil
	}

	available, err := flow.engine.diskAvailableFn(flow.engine.syncRoot)
	if err != nil {
		flow.engine.logger.Warn("repairPersistedScopes: disk revalidation failed, releasing stale disk scope",
			slog.String("scope_key", block.Key.String()),
			slog.String("error", err.Error()),
		)
		if releaseErr := flow.engine.baseline.ReleaseScope(ctx, block.Key, flow.engine.nowFunc()); releaseErr != nil {
			return fmt.Errorf("sync: releasing stale disk scope %s: %w", block.Key.String(), releaseErr)
		}
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventStartupScopeRepaired,
			ScopeKey: block.Key,
			Note:     "released disk scope after revalidation error",
		})
		return nil
	}

	if int64(available) >= flow.engine.minFreeSpace {
		if err := flow.engine.baseline.ReleaseScope(ctx, block.Key, flow.engine.nowFunc()); err != nil {
			return fmt.Errorf("sync: releasing recovered disk scope %s: %w", block.Key.String(), err)
		}
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventStartupScopeRepaired,
			ScopeKey: block.Key,
			Note:     "released disk scope after healthy revalidation",
		})
		return nil
	}

	now := flow.engine.nowFunc()
	interval := computeTrialInterval(block.Key, 0, 0)
	if err := flow.engine.baseline.UpsertScopeBlock(ctx, &syncstore.ScopeBlock{
		Key:           block.Key,
		IssueType:     synctypes.IssueDiskFull,
		TimingSource:  synctypes.ScopeTimingBackoff,
		BlockedAt:     now,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
		PreserveUntil: time.Time{},
	}); err != nil {
		return fmt.Errorf("sync: refreshing disk scope %s: %w", block.Key.String(), err)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventStartupScopeRepaired,
		ScopeKey: block.Key,
		Note:     "refreshed disk scope from current local truth",
	})

	return nil
}

func (controller *scopeController) getScopeBlock(watch *watchRuntime, key synctypes.ScopeKey) (syncstore.ScopeBlock, bool) {
	if watch == nil {
		return syncstore.ScopeBlock{}, false
	}
	return watch.lookupActiveScope(key)
}

func (controller *scopeController) isScopeBlocked(watch *watchRuntime, key synctypes.ScopeKey) bool {
	if watch == nil {
		return false
	}
	return watch.hasActiveScope(key)
}

func (controller *scopeController) activeBlockingScope(watch *watchRuntime, ta *TrackedAction) synctypes.ScopeKey {
	if watch == nil {
		return synctypes.ScopeKey{}
	}
	return watch.findBlockingScope(ta)
}

func (controller *scopeController) scopeBlockKeys(watch *watchRuntime) []synctypes.ScopeKey {
	if watch == nil {
		return nil
	}
	return watch.activeScopeKeys()
}

func (controller *scopeController) activateScope(ctx context.Context, watch *watchRuntime, block *syncstore.ScopeBlock) error {
	flow := controller.flow

	if block == nil {
		return fmt.Errorf("sync: activating scope: missing block")
	}

	if err := flow.engine.baseline.UpsertScopeBlock(ctx, block); err != nil {
		return fmt.Errorf("sync: activating scope %s: %w", block.Key.String(), err)
	}

	if watch != nil {
		watch.upsertActiveScope(block)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventScopeActivated,
		ScopeKey: block.Key,
	})

	flow.mustAssertInvariants(ctx, watch, "activate scope")

	return nil
}

func (controller *scopeController) activateAuthScope(ctx context.Context, watch *watchRuntime) error {
	block := &syncstore.ScopeBlock{
		Key:          synctypes.SKAuthAccount(),
		IssueType:    synctypes.IssueUnauthorized,
		TimingSource: synctypes.ScopeTimingNone,
		BlockedAt:    controller.flow.engine.nowFunc(),
	}

	return controller.activateScope(ctx, watch, block)
}

func (controller *scopeController) extendScopeTrial(
	ctx context.Context,
	watch *watchRuntime,
	scopeKey synctypes.ScopeKey,
	retryAfter time.Duration,
) {
	flow := controller.flow

	if watch == nil {
		return
	}

	block, ok := controller.getScopeBlock(watch, scopeKey)
	if !ok {
		return
	}

	newInterval := computeTrialInterval(scopeKey, retryAfter, block.TrialInterval)
	nextAt := flow.engine.nowFunc().Add(newInterval)

	flow.engine.logger.Debug("extending trial interval",
		slog.String("scope_key", scopeKey.String()),
		slog.Duration("new_interval", newInterval),
		slog.Duration("retry_after", retryAfter),
	)

	block.NextTrialAt = nextAt
	block.TrialInterval = newInterval
	block.PreserveUntil = time.Time{}
	block.TrialCount++
	block.TimingSource = scopeTimingSource(retryAfter)
	if err := controller.activateScope(ctx, watch, &block); err != nil {
		flow.engine.logger.Warn("extendScopeTrial: failed to persist interval extension",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	watch.armTrialTimer()
}

func (controller *scopeController) preserveScopeTrial(ctx context.Context, watch *watchRuntime, scopeKey synctypes.ScopeKey) {
	flow := controller.flow

	if watch == nil {
		return
	}

	block, ok := controller.getScopeBlock(watch, scopeKey)
	if !ok {
		return
	}
	if block.TrialInterval <= 0 {
		return
	}

	block.NextTrialAt = flow.engine.nowFunc().Add(block.TrialInterval)
	block.PreserveUntil = block.NextTrialAt
	if err := controller.activateScope(ctx, watch, &block); err != nil {
		flow.engine.logger.Warn("preserveScopeTrial: failed to persist preserved interval",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	flow.engine.logger.Debug("preserving trial interval",
		slog.String("scope_key", scopeKey.String()),
		slog.Duration("interval", block.TrialInterval),
	)

	watch.armTrialTimer()
}

// computeTrialInterval is the single source of truth for trial interval
// computation (R-2.10.14). Both initial scope block creation and subsequent
// trial extensions use this function, preventing policy divergence.
//
//   - retryAfter > 0: server-provided value used directly, no cap (R-2.10.7)
//   - retryAfter == 0, currentInterval > 0: double current, cap at defaultMaxTrialInterval
//   - retryAfter == 0, currentInterval == 0: use defaultInitialTrialInterval
func computeTrialInterval(scopeKey synctypes.ScopeKey, retryAfter, currentInterval time.Duration) time.Duration {
	initialInterval := DefaultInitialTrialInterval
	maxInterval := DefaultMaxTrialInterval
	if scopeKey == synctypes.SKDiskLocal() {
		initialInterval = diskScopeInitialTrialInterval
		maxInterval = diskScopeMaxTrialInterval
	}

	if retryAfter > 0 {
		return retryAfter
	}
	if currentInterval > 0 {
		doubled := currentInterval * 2
		if doubled > maxInterval {
			return maxInterval
		}
		return doubled
	}
	return initialInterval
}

func scopeTimingSource(retryAfter time.Duration) synctypes.ScopeTimingSource {
	if retryAfter > 0 {
		return synctypes.ScopeTimingServerRetryAfter
	}

	return synctypes.ScopeTimingBackoff
}

// isObservationSuppressed returns true if a global scope block is active,
// meaning all shortcut observation polling should be skipped to avoid wasting
// API calls. Target-scoped 429 blocks are handled separately.
func (controller *scopeController) isObservationSuppressed(watch *watchRuntime) bool {
	return controller.isScopeBlocked(watch, synctypes.SKAuthAccount()) ||
		controller.isScopeBlocked(watch, synctypes.SKThrottleAccount()) ||
		controller.isScopeBlocked(watch, synctypes.SKService())
}

// suppressedShortcutTargets returns the exact shared shortcut targets whose
// observation should be skipped while target-scoped 429 blocks are active.
// Own-drive throttles do not suppress shortcut observation.
func (controller *scopeController) suppressedShortcutTargets(watch *watchRuntime) map[string]struct{} {
	keys := controller.scopeBlockKeys(watch)
	if len(keys) == 0 {
		return nil
	}

	var suppressed map[string]struct{}
	for i := range keys {
		if !keys[i].IsThrottleShared() {
			continue
		}
		if suppressed == nil {
			suppressed = make(map[string]struct{})
		}
		suppressed[keys[i].ThrottleShortcutKey()] = struct{}{}
	}

	return suppressed
}

// feedScopeDetection feeds a worker result into scope detection sliding
// windows. If a threshold is crossed, creates a scope block. Called directly
// from the normal processWorkerResult switch — never called for trial results
// (the scope is already blocked, and re-detecting would overwrite the doubled
// interval).
func (controller *scopeController) feedScopeDetection(ctx context.Context, watch *watchRuntime, r *WorkerResult) {
	if watch == nil {
		return
	}

	// Local errors (HTTPStatus==0) must not feed scope detection windows.
	// Only remote API errors should increment service/quota counters (R-6.7.27).
	if r.HTTPStatus == 0 {
		return
	}

	sr := watch.scopeState.UpdateScope(r)
	if sr.Block {
		controller.applyScopeBlock(ctx, watch, sr)
	}
}

// applyScopeBlock persists and activates a new scope block. Uses
// computeTrialInterval for the initial interval, ensuring the same
// Retry-After-vs-backoff policy as extendScopeTrial.
func (controller *scopeController) applyScopeBlock(ctx context.Context, watch *watchRuntime, sr synctypes.ScopeUpdateResult) {
	flow := controller.flow

	now := flow.engine.nowFunc()
	interval := computeTrialInterval(sr.ScopeKey, sr.RetryAfter, 0)

	block := &syncstore.ScopeBlock{
		Key:           sr.ScopeKey,
		IssueType:     sr.IssueType,
		TimingSource:  scopeTimingSource(sr.RetryAfter),
		BlockedAt:     now,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
		PreserveUntil: time.Time{},
	}
	if err := controller.activateScope(ctx, watch, block); err != nil {
		flow.engine.logger.Warn("applyScopeBlock: failed to persist scope block",
			slog.String("scope_key", sr.ScopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	flow.engine.logger.Warn("scope block active — actions held",
		slog.String("scope_key", sr.ScopeKey.String()),
		slog.String("issue_type", sr.IssueType),
		slog.Duration("trial_interval", interval),
	)

	if watch != nil {
		watch.armTrialTimer() // arm so the first trial fires at NextTrialAt (R-2.10.5)
	}
}

// releaseScope atomically removes the scope block, deletes any actionable
// boundary row for the scope, and makes held descendants retryable now.
func (controller *scopeController) releaseScope(ctx context.Context, watch *watchRuntime, key synctypes.ScopeKey) error {
	flow := controller.flow

	if err := flow.engine.baseline.ReleaseScope(ctx, key, flow.engine.nowFunc()); err != nil {
		return fmt.Errorf("sync: releasing scope %s: %w", key.String(), err)
	}

	if watch != nil {
		watch.removeActiveScope(key)
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventScopeReleased,
			ScopeKey: key,
		})
		watch.kickRetrySweepNow()
		watch.armTrialTimer()
	} else {
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventScopeReleased,
			ScopeKey: key,
		})
	}

	flow.engine.logger.Info("scope block cleared — failures unblocked",
		slog.String("scope_key", key.String()),
	)

	flow.mustAssertReleasedScope(ctx, watch, key, "release scope")
	flow.mustAssertInvariants(ctx, watch, "release scope")

	return nil
}

// discardScope atomically removes the scope block and deletes all failure rows
// tied to it. Used when the blocked subtree itself disappears.
func (controller *scopeController) discardScope(ctx context.Context, watch *watchRuntime, key synctypes.ScopeKey) error {
	flow := controller.flow

	if err := flow.engine.baseline.DiscardScope(ctx, key); err != nil {
		return fmt.Errorf("sync: discarding scope %s: %w", key.String(), err)
	}

	if watch != nil {
		watch.removeActiveScope(key)
		watch.armTrialTimer()
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventScopeDiscarded,
		ScopeKey: key,
	})

	flow.mustAssertDiscardedScope(ctx, watch, key, "discard scope")
	flow.mustAssertInvariants(ctx, watch, "discard scope")

	return nil
}

func pathMatchesPrefix(path string, prefix string) bool {
	if prefix == "" {
		return true
	}

	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func (controller *scopeController) clearResolvedRemoteBlockedFailures(
	ctx context.Context,
	watch *watchRuntime,
	changedPaths map[string]bool,
) {
	if len(changedPaths) == 0 {
		return
	}

	flow := controller.flow
	rows, err := flow.engine.baseline.ListRemoteBlockedFailures(ctx)
	if err != nil {
		flow.engine.logger.Warn("failed to list remote blocked failures for cleanup",
			slog.String("error", err.Error()),
		)
		return
	}

	clearedScopes := make(map[synctypes.ScopeKey]bool)
	for i := range rows {
		if !remoteBlockedFailureRelevant(&rows[i], changedPaths) {
			continue
		}
		if flow.isFailureResolved(ctx, &rows[i]) {
			clearedScopes[rows[i].ScopeKey] = true
		}
	}

	if watch == nil || len(clearedScopes) == 0 {
		return
	}

	controller.releaseResolvedRemoteScopes(ctx, watch, clearedScopes)
}

func remoteBlockedFailureRelevant(
	row *syncstore.SyncFailureRow,
	changedPaths map[string]bool,
) bool {
	boundary := row.ScopeKey.RemotePath()
	for changedPath := range changedPaths {
		if pathMatchesPrefix(row.Path, changedPath) ||
			pathMatchesPrefix(changedPath, row.Path) ||
			pathMatchesPrefix(boundary, changedPath) ||
			pathMatchesPrefix(changedPath, boundary) {
			return true
		}
	}

	return false
}

func (controller *scopeController) releaseResolvedRemoteScopes(
	ctx context.Context,
	watch *watchRuntime,
	clearedScopes map[synctypes.ScopeKey]bool,
) {
	flow := controller.flow
	remainingScopes, ok := controller.remainingRemoteBlockedScopes(ctx)
	if !ok {
		return
	}

	for key := range clearedScopes {
		if remainingScopes[key] {
			continue
		}
		if err := controller.releaseScope(ctx, watch, key); err != nil {
			flow.engine.logger.Warn("failed to clear resolved remote blocked scope",
				slog.String("scope_key", key.String()),
				slog.String("error", err.Error()),
			)
		}
	}
}

func (controller *scopeController) remainingRemoteBlockedScopes(
	ctx context.Context,
) (map[synctypes.ScopeKey]bool, bool) {
	flow := controller.flow

	remainingRows, err := flow.engine.baseline.ListRemoteBlockedFailures(ctx)
	if err != nil {
		flow.engine.logger.Warn("failed to relist remote blocked failures after cleanup",
			slog.String("error", err.Error()),
		)
		return nil, false
	}

	remainingScopes := make(map[synctypes.ScopeKey]bool, len(remainingRows))
	for i := range remainingRows {
		remainingScopes[remainingRows[i].ScopeKey] = true
	}

	return remainingScopes, true
}

// admitReady applies watch-mode trial interception and scope admission to a
// ready action set, returning the actions that should enter the watch loop's
// outbox. It is the single admission path used by both newly-planned actions
// and newly-ready dependents from result processing.
func (controller *scopeController) admitReady(
	ctx context.Context,
	watch *watchRuntime,
	ready []*TrackedAction,
) []*TrackedAction {
	flow := controller.flow

	var dispatch []*TrackedAction

	for _, ta := range ready {
		if ta.IsTrial {
			if ta.TrialScopeKey.BlocksAction(ta.Action.Path,
				ta.Action.ThrottleTargetKey(), ta.Action.ShortcutKey(), ta.Action.Type,
				ta.Action.TargetsOwnDrive()) {
				dispatch = append(dispatch, ta)
			} else {
				// Trial candidate no longer matches scope — clear stale failure,
				// run normal admission. Best-effort: log on error, don't abort.
				if err := flow.engine.baseline.ClearSyncFailure(ctx, ta.Action.Path, ta.Action.DriveID); err != nil {
					flow.engine.logger.Debug("admitReady: failed to clear stale trial failure",
						slog.String("path", ta.Action.Path),
						slog.String("error", err.Error()),
					)
				}

				if key := controller.activeBlockingScope(watch, ta); key.IsZero() {
					flow.setDispatch(ctx, &ta.Action)
					dispatch = append(dispatch, ta)
				}
				if watch != nil {
					watch.armTrialTimer()
				}
			}

			continue
		}

		// Normal scope admission.
		if watch != nil {
			if key := controller.activeBlockingScope(watch, ta); !key.IsZero() {
				controller.cascadeRecordAndComplete(ctx, ta, key)
				continue
			}
		}

		flow.setDispatch(ctx, &ta.Action)
		dispatch = append(dispatch, ta)
	}

	return dispatch
}

// cascadeRecordAndComplete records a scope-blocked action and all its
// transitive dependents as sync_failures, completing each in the graph.
// Uses BFS to traverse the dependency tree. Each dependent inherits the
// parent's scope_key (section 3.4).
//
// Safe for concurrent use — depGraph.Complete uses a mutex. Two cascades
// from different goroutines cannot return the same dependent (depsLeft is
// atomic — the last parent to complete returns the dependent).
func (controller *scopeController) cascadeRecordAndComplete(
	ctx context.Context,
	ta *TrackedAction,
	scopeKey synctypes.ScopeKey,
) {
	flow := controller.flow

	seen := make(map[int64]bool)
	queue := []*TrackedAction{ta}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		controller.recordScopeBlockedFailure(ctx, &current.Action, scopeKey)
		// No resetDispatchStatus — setDispatch was never called for blocked
		// actions (active-scope admission runs BEFORE setDispatch, per section 2.2).
		ready := flow.completeDepGraphAction(current.ID, "cascadeRecordAndComplete")
		queue = append(queue, ready...)
	}
}

// cascadeFailAndComplete records each transitive dependent as a cascade
// failure and completes it in the DepGraph. BFS ensures grandchildren are
// not stranded. Used for worker failures (non-scope-related).
func (controller *scopeController) cascadeFailAndComplete(
	ctx context.Context,
	ready []*TrackedAction,
	r *WorkerResult,
) {
	flow := controller.flow

	seen := make(map[int64]bool)
	queue := ready

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		controller.recordCascadeFailure(ctx, &current.Action, r)
		next := flow.completeDepGraphAction(current.ID, "cascadeFailAndComplete")
		queue = append(queue, next...)
	}
}

// completeSubtree silently completes all transitive dependents without
// recording failures. Used for shutdown (context canceled — not a failure).
func (controller *scopeController) completeSubtree(ready []*TrackedAction) {
	flow := controller.flow

	seen := make(map[int64]bool)
	queue := ready

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		next := flow.completeDepGraphAction(current.ID, "completeSubtree")
		queue = append(queue, next...)
	}
}

// recordScopeBlockedFailure records a sync_failure for an action that was
// blocked by an active scope. Uses next_retry_at = NULL (nil delayFn) so the
// retry sweep ignores it until releaseScope sets next_retry_at.
func (controller *scopeController) recordScopeBlockedFailure(ctx context.Context, action *Action, scopeKey synctypes.ScopeKey) {
	flow := controller.flow

	direction := directionFromAction(action.Type)

	driveID := action.DriveID
	if driveID.String() == "" {
		driveID = flow.engine.driveID
	}

	if err := flow.engine.baseline.RecordFailure(ctx, &syncstore.SyncFailureParams{
		Path:       action.Path,
		DriveID:    driveID,
		Direction:  direction,
		ActionType: action.Type,
		ItemID:     action.ItemID,
		Role:       synctypes.FailureRoleHeld,
		Category:   synctypes.CategoryTransient,
		IssueType:  scopeKey.IssueType(),
		ScopeKey:   scopeKey,
		ErrMsg:     "scope blocked: " + scopeKey.String(),
	}, nil); err != nil { // nil delayFn → next_retry_at = NULL
		flow.engine.logger.Warn("failed to record scope-blocked failure",
			slog.String("path", action.Path),
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
	}
}

func (controller *scopeController) rehomeHeldFailure(
	ctx context.Context,
	r *WorkerResult,
	scopeKey synctypes.ScopeKey,
) {
	flow := controller.flow

	direction := directionFromAction(r.ActionType)

	driveID := r.DriveID
	if driveID.String() == "" {
		driveID = flow.engine.driveID
	}

	if err := flow.engine.baseline.RecordFailure(ctx, &syncstore.SyncFailureParams{
		Path:       r.Path,
		DriveID:    driveID,
		Direction:  direction,
		ActionType: r.ActionType,
		Role:       synctypes.FailureRoleHeld,
		Category:   synctypes.CategoryTransient,
		IssueType:  scopeKey.IssueType(),
		ScopeKey:   scopeKey,
		ErrMsg:     "scope blocked: " + scopeKey.String(),
		HTTPStatus: r.HTTPStatus,
	}, nil); err != nil {
		flow.engine.logger.Warn("failed to rehome held failure",
			slog.String("path", r.Path),
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
	}
}

// recordCascadeFailure records a sync_failure for a dependent whose parent
// failed. The dependent inherits the parent's error context but gets its
// own direction and a fresh failure_count. Uses retry.ReconcilePolicy().Delay for
// exponential backoff — the dependent retries independently.
func (controller *scopeController) recordCascadeFailure(
	ctx context.Context,
	action *Action,
	parentResult *WorkerResult,
) {
	flow := controller.flow

	direction := directionFromAction(action.Type)

	driveID := action.DriveID
	if driveID.String() == "" {
		driveID = flow.engine.driveID
	}

	issueType := issueTypeForHTTPStatus(parentResult.HTTPStatus, parentResult.Err)

	if err := flow.engine.baseline.RecordFailure(ctx, &syncstore.SyncFailureParams{
		Path:       action.Path,
		DriveID:    driveID,
		Direction:  direction,
		ActionType: action.Type,
		Category:   synctypes.CategoryTransient,
		IssueType:  issueType,
		ErrMsg:     "parent action failed: " + parentResult.ErrMsg,
	}, retry.ReconcilePolicy().Delay); err != nil {
		flow.engine.logger.Warn("failed to record cascade failure",
			slog.String("path", action.Path),
			slog.String("error", err.Error()),
		)
	}
}
