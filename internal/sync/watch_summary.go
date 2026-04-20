package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

type watchBlockedRetryProjection struct {
	count int
	paths []string
}

type watchRemoteBlockedGroup struct {
	ConditionKey ConditionKey
	ScopeKey     ScopeKey
	BlockedPaths []string
}

// watchConditionCount is one raw condition-key count in the watch summary.
type watchConditionCount struct {
	Key   ConditionKey
	Count int
}

// watchConditionSummary is the engine-owned aggregate view of sync conditions
// for watch summaries and related assertions.
type watchConditionSummary struct {
	Counts   []watchConditionCount
	Retrying int
}

func (s watchConditionSummary) VisibleTotal() int {
	total := 0
	for _, count := range s.Counts {
		total += count.Count
	}

	return total
}

func (s watchConditionSummary) ConflictCount() int {
	return 0
}

func (s watchConditionSummary) ActionableCount() int {
	total := 0
	for _, count := range s.Counts {
		if count.Key == ConditionRemoteWriteDenied ||
			count.Key == ConditionAuthenticationRequired {
			continue
		}
		total += count.Count
	}

	return total
}

func (s watchConditionSummary) RemoteBlockedCount() int {
	return s.countForKey(ConditionRemoteWriteDenied)
}

func (s watchConditionSummary) AuthRequiredCount() int {
	return s.countForKey(ConditionAuthenticationRequired)
}

func (s watchConditionSummary) RetryingCount() int {
	return s.Retrying
}

func (s watchConditionSummary) countForKey(key ConditionKey) int {
	total := 0
	for _, count := range s.Counts {
		if count.Key == key {
			total += count.Count
		}
	}

	return total
}

type watchConditionCountAccumulator map[ConditionKey]int

func (a watchConditionCountAccumulator) Add(key ConditionKey, count int) {
	if key == "" || count <= 0 {
		return
	}

	a[key] += count
}

func (a watchConditionCountAccumulator) Counts() []watchConditionCount {
	counts := make([]watchConditionCount, 0, len(a))
	for key, count := range a {
		counts = append(counts, watchConditionCount{
			Key:   key,
			Count: count,
		})
	}

	sort.Slice(counts, func(i, j int) bool {
		return string(counts[i].Key) < string(counts[j].Key)
	})

	return counts
}

func buildWatchConditionSummary(snapshot *DriveStatusSnapshot) (watchConditionSummary, []watchRemoteBlockedGroup) {
	if snapshot == nil {
		return watchConditionSummary{}, nil
	}

	blockedByScope := groupWatchBlockedRetryWork(snapshot.BlockedRetryWork)
	counts := make(watchConditionCountAccumulator)

	for i := range snapshot.ObservationIssues {
		key := ConditionKeyForObservationIssue(
			snapshot.ObservationIssues[i].IssueType,
			snapshot.ObservationIssues[i].ScopeKey,
		)
		counts.Add(key, 1)
	}

	var remoteGroups []watchRemoteBlockedGroup
	for i := range snapshot.BlockScopes {
		block := snapshot.BlockScopes[i]
		if block == nil {
			continue
		}

		projection := blockedByScope[block.Key]
		count := projection.count
		if count == 0 {
			count = 1
		}
		conditionKey := ConditionKeyForBlockScope(block.ConditionType, block.Key)
		counts.Add(conditionKey, count)

		if block.Key.IsPermRemoteWrite() {
			paths := append([]string(nil), projection.paths...)
			sort.Strings(paths)
			remoteGroups = append(remoteGroups, watchRemoteBlockedGroup{
				ConditionKey: conditionKey,
				ScopeKey:     block.Key,
				BlockedPaths: paths,
			})
		}
	}

	sort.Slice(remoteGroups, func(i, j int) bool {
		return remoteGroups[i].ScopeKey.String() < remoteGroups[j].ScopeKey.String()
	})

	return watchConditionSummary{
		Counts:   counts.Counts(),
		Retrying: snapshot.RetryingItems,
	}, remoteGroups
}

func groupWatchBlockedRetryWork(rows []RetryWorkRow) map[ScopeKey]watchBlockedRetryProjection {
	grouped := make(map[ScopeKey]watchBlockedRetryProjection)
	for i := range rows {
		scopeKey := rows[i].ScopeKey
		if scopeKey.IsZero() {
			continue
		}

		projection := grouped[scopeKey]
		projection.count++
		if rows[i].Path != "" {
			projection.paths = append(projection.paths, rows[i].Path)
		}
		grouped[scopeKey] = projection
	}

	return grouped
}

// logWatchSummary logs a periodic one-liner summary of active sync conditions
// in watch mode. Only logs when the count changes since the last summary
// to avoid noisy repeated output.
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
