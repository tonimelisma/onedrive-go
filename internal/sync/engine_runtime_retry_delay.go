// The delays a completion records: the retry curve for each failure decision
// and condition, and the supersede streaks that park a wedged action.

package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// durableRecheckDelay is the first wait before an action refused by a
// condition that does not clear by itself is tried again
// (retryDelayForCondition).
const durableRecheckDelay = 30 * time.Second

// retryDelayForDecision is the backoff curve for the retry work a decision
// records. Work refused by a condition only the user can clear
// (ClassActionable: OneDrive refused the name or path, a local folder is not
// writable, the file is too large) does not succeed by being tried sooner,
// so it waits on the durable curve whatever the condition; before, only the
// two conditions named below did, and a folder OneDrive refused as too long
// was retried every few seconds (soak of 2026-11-02).
// The reconcile curve's jitter is drawn from jitter, the run's clock.
func retryDelayForDecision(decision *syncdomain.ResultDecision, jitter retry.JitterSource) func(int) time.Duration {
	if decision == nil {
		return reconcileRetryDelay(jitter)
	}
	if decision.Class == errclass.ClassActionable {
		return durableRecheckRetryDelay
	}

	return retryDelayForCondition(decision.ConditionType, jitter)
}

// retryDelayForCondition is the backoff curve for retry work of a condition,
// decided for every IssueType by name (the exhaustive linter holds the switch
// to that), so a new condition is put on a curve on purpose. A transient
// failure follows the reconcile curve, which starts at one second. Two
// conditions do not clear by themselves and wait from durableRecheckDelay,
// doubling to an hour, whatever class records them:
//   - a name collision clears when the user renames one of the two names or
//     the other one goes away online. On the one-second curve it was "held"
//     only while the next pass began within a second of the failure, so a
//     slower pass (over a second under -race) or a watch replan retried the
//     refused create again and again; a rename plans new work that this row
//     does not hold;
//   - a folder too large to verify before its recursive delete on OneDrive
//     clears when the user deletes it online or it shrinks. Every retry
//     relists up to RemoteFolderDeleteListLimit items before it fails again,
//     so on the one-second curve a watch session relisted 10,000 items on
//     every pass.
//
// Every other member clears by itself (a timeout, a conflict, a busy file),
// is held under its block scope until the scope's trial passes (throttling,
// an outage, a full drive or disk), is recorded as ClassActionable, which
// retryDelayForDecision already puts on the durable curve, or never becomes
// retry work (a sign-in failure, which stops the run, and the members only
// observation reports). A failure with no condition is transient. A value
// outside the enum is not known to clear by itself, so it waits on the
// durable curve: a hot retry loop is the failure this curve exists to
// prevent.
func retryDelayForCondition(condition syncdomain.IssueType, jitter retry.JitterSource) func(int) time.Duration {
	switch condition {
	case syncdomain.IssueCaseCollision, syncdomain.IssueRemoteFolderUnverifiable:
		return durableRecheckRetryDelay
	case syncdomain.IssueInvalidFilename,
		syncdomain.IssuePathTooLong,
		syncdomain.IssueFileTooLarge,
		syncdomain.IssueHashError,
		syncdomain.IssueUnauthorized,
		syncdomain.IssueRemoteWriteDenied,
		syncdomain.IssueRemoteReadDenied,
		syncdomain.IssueRemoteRootNotFound,
		syncdomain.IssueQuotaExceeded,
		syncdomain.IssueRateLimited,
		syncdomain.IssueLocalReadDenied,
		syncdomain.IssueLocalWriteDenied,
		syncdomain.IssueRemoteNameTwin,
		syncdomain.IssueDiskFull,
		syncdomain.IssueServiceOutage,
		syncdomain.IssueFileTooLargeForSpace,
		syncdomain.IssueLocalNameTooLong,
		syncdomain.IssueLocalReadOnly,
		syncdomain.IssueDeletesHeld,
		syncdomain.IssueLocalUnsyncableReplacement,
		syncdomain.IssueLocalMountBoundary,
		syncdomain.IssueLocalMountMissing,
		syncdomain.IssueLocalInterruptedRemoval,
		syncdomain.IssueRemovedFolderKept,
		syncdomain.IssueLocalFileTooLarge,
		syncdomain.IssueLocalBusy,
		syncdomain.IssueLocalCallAbandoned,
		syncdomain.IssueLocalCrossDevice,
		syncdomain.IssueUnsupportedOperation,
		syncdomain.IssueMalwareDetected,
		syncdomain.IssueLocalDatabaseOpen,
		syncdomain.IssueUploadVerification,
		syncdomain.IssueDownloadIntegrity,
		syncdomain.IssueRequestTimeout,
		syncdomain.IssueTransientConflict,
		syncdomain.IssueTransientNotFound,
		syncdomain.IssueResourceLocked,
		syncdomain.IssueTransferURLRejected,
		"":
		return reconcileRetryDelay(jitter)
	default:
		return durableRecheckRetryDelay
	}
}

// reconcileRetryDelay is the reconcile curve with its jitter drawn from
// jitter.
func reconcileRetryDelay(jitter retry.JitterSource) func(int) time.Duration {
	policy := retry.ReconcilePolicy()

	return func(attempt int) time.Duration { return policy.Delay(attempt, jitter) }
}

func durableRecheckRetryDelay(attempt int) time.Duration {
	delay := durableRecheckDelay
	for range attempt {
		delay *= 2
		if delay >= time.Hour {
			return time.Hour
		}
	}

	return delay
}

// supersedeParkThreshold is how many consecutive identical supersedes of one
// action make it wedged (D-059d). Below it a supersede replans at once and
// keeps no retry work (R-2.8.9), as truth moving on between planning and
// execution should. From it on the action is parked in retry_work with
// exponential backoff: a precondition that can never hold costs one attempt
// per backoff step instead of a hot replan loop in watch mode, the parked
// action shows in status as a retrying item, and the park outlives the run.
const supersedeParkThreshold = supersedeStreakWarnThreshold

const (
	supersedeParkBaseDelay = 30 * time.Second
	supersedeParkMaxDelay  = time.Hour
)

// supersedeParkDelay is the backoff after the attempt-th park (0-based) of
// one wedged action: doubling from the base to a cap.
func supersedeParkDelay(attempt int) time.Duration {
	delay := supersedeParkBaseDelay
	for range attempt {
		delay *= 2
		if delay >= supersedeParkMaxDelay {
			return supersedeParkMaxDelay
		}
	}

	return delay
}

// parkWedgedAction persists a wedged action in retry_work, the durable owner
// of per-action retry timing. The row is keyed by the action, so a replan that
// plans something else prunes it and a success resolves it.
func (flow *engineFlow) parkWedgedAction(ctx context.Context, r *syncdomain.ActionCompletion, current *syncdomain.TrackedAction) error {
	work := syncdomain.RetryWorkKeyForCompletion(r)
	if current != nil {
		work = syncdomain.RetryWorkKeyForAction(&current.Action)
	}
	if _, err := flow.retryWork.recordFailure(ctx, flow.deps.store, &syncdomain.RetryWorkFailure{Work: work}, supersedeParkDelay); err != nil {
		return fmt.Errorf("park wedged action %s: %w", r.Path, err)
	}
	flow.signals.armRetryTimer()

	return nil
}

// supersedeStreakWarnThreshold is how many consecutive supersedes of the same
// action make the stall visible at Warn. One or two are ordinary: truth moved
// on between planning and execution and the replan absorbs it.
const supersedeStreakWarnThreshold = 3

func supersedeStreakKey(r *syncdomain.ActionCompletion) string {
	return r.ActionType.String() + "\x00" + r.OldPath + "\x00" + r.Path
}

// recordSupersede logs every superseded completion at Debug and warns once
// when the same action keeps being superseded, so a precondition that can
// never hold is visible instead of a silent stall.
func (flow *engineFlow) recordSupersede(r *syncdomain.ActionCompletion) int {
	if r == nil || flow.supersedeStreaks == nil {
		return 0
	}
	key := supersedeStreakKey(r)
	flow.supersedeStreaks[key]++
	streak := flow.supersedeStreaks[key]

	reason := r.ErrMsg
	if r.Err != nil {
		reason = r.Err.Error()
	}
	logger := flow.deps.logger
	if logger == nil {
		return streak
	}
	logger.Debug("action superseded; replanning",
		slog.String("action", r.ActionType.String()),
		slog.String("path", r.Path),
		slog.String("old_path", r.OldPath),
		slog.Int("consecutive", streak),
		slog.String("reason", reason),
	)
	if streak == supersedeStreakWarnThreshold {
		// Once per streak: a file that is rewritten continuously also lands
		// here, and it must not warn on every pass.
		// The console shows the path and a plain reason; the raw chain (with
		// item IDs) and the classification travel as internal fields that
		// only the log file and --verbose show.
		attrs := []any{slog.String("path", r.Path)}
		if r.OldPath != "" && r.OldPath != r.Path {
			attrs = append(attrs, slog.String("from", r.OldPath))
		}
		attrs = append(attrs,
			slog.String("reason", "it no longer matches what sync planned"),
			slog.String("action_type", r.ActionType.String()),
			slog.Int("supersede_streak", streak),
			slog.String("error_detail", reason),
		)
		logger.Warn("A planned change keeps going stale before it can run; sync parks it and retries with backoff", attrs...)
	}

	return streak
}

// pruneSupersedeStreaks forgets the streaks of actions a new plan no longer
// contains: a plan that dropped the action means its supersedes were not
// consecutive, so it cannot be wedged.
func (flow *engineFlow) pruneSupersedeStreaks(plan *syncdomain.ActionPlan) {
	if len(flow.supersedeStreaks) == 0 {
		return
	}
	planned := map[string]bool{}
	if plan != nil {
		for i := range plan.Actions {
			action := &plan.Actions[i]
			planned[supersedeStreakKey(&syncdomain.ActionCompletion{ActionType: action.Type, OldPath: action.OldPath, Path: action.Path})] = true
		}
	}
	for key := range flow.supersedeStreaks {
		if !planned[key] {
			delete(flow.supersedeStreaks, key)
		}
	}
}

func (flow *engineFlow) clearSupersedeStreak(r *syncdomain.ActionCompletion) {
	if r == nil || flow.supersedeStreaks == nil {
		return
	}
	delete(flow.supersedeStreaks, supersedeStreakKey(r))
}
