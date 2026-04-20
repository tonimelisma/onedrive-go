package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// logWatchSummary logs a periodic one-liner summary of active sync conditions
// in watch mode. The summary builder stays raw and machine-oriented; this
// boundary owns human-oriented log phrasing and churn suppression.
func (rt *watchRuntime) logWatchSummary(ctx context.Context) {
	snapshot, err := rt.engine.baseline.ReadDriveStatusSnapshot(ctx)
	if err != nil {
		return
	}
	summary, groups := buildWatchConditionSummary(&snapshot)
	rt.logRemoteBlockedChanges(groups)

	totalConditions := summary.VisibleTotal()
	if totalConditions == 0 {
		if rt.lastSummaryTotal != 0 || rt.lastSummarySignature != "" {
			rt.engine.logger.Info("sync conditions cleared")
		}
		rt.lastSummaryTotal = 0
		rt.lastSummarySignature = ""
		return
	}

	signature, breakdown := watchSummarySignature(summary)
	if signature == rt.lastSummarySignature {
		return
	}

	rt.lastSummaryTotal = totalConditions
	rt.lastSummarySignature = signature

	rt.engine.logger.Warn("sync conditions",
		slog.Int("total", totalConditions),
		slog.String("breakdown", breakdown),
	)
}

func (rt *watchRuntime) logRemoteBlockedChanges(groups []watchRemoteBlockedGroup) {
	current := make(map[ScopeKey]string, len(groups))

	for i := range groups {
		group := groups[i]
		if !group.ScopeKey.IsPermRemoteWrite() {
			continue
		}
		boundary := group.ScopeKey.Humanize()

		signature := strings.Join(group.BlockedPaths, "\x00")
		current[group.ScopeKey] = signature

		switch previous, ok := rt.lastRemoteBlocked[group.ScopeKey]; {
		case !ok:
			rt.engine.logger.Warn("shared-folder writes blocked",
				slog.String("boundary", boundary),
				slog.Int("blocked_writes", len(group.BlockedPaths)),
			)
		case previous != signature:
			rt.engine.logger.Warn("shared-folder writes still blocked",
				slog.String("boundary", boundary),
				slog.Int("blocked_writes", len(group.BlockedPaths)),
			)
		}
	}

	for scopeKey := range rt.lastRemoteBlocked {
		if _, ok := current[scopeKey]; ok {
			continue
		}
		rt.engine.logger.Info("shared-folder write block cleared",
			slog.String("boundary", scopeKey.Humanize()),
		)
	}

	rt.lastRemoteBlocked = current
}

func watchSummarySignature(summary watchConditionSummary) (string, string) {
	parts := make([]string, 0, len(summary.Counts))
	for i := range summary.Counts {
		parts = append(parts, fmt.Sprintf("%d %s", summary.Counts[i].Count, summary.Counts[i].Key))
	}
	sort.Strings(parts)
	breakdown := strings.Join(parts, ", ")
	return fmt.Sprintf("%d|%s", summary.VisibleTotal(), breakdown), breakdown
}
