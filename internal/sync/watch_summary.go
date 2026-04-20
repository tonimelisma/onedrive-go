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
	ScopeKey     ScopeKey
	BoundaryPath string
	BlockedPaths []string
}

// watchConditionGroupCount is one derived visible condition family with its
// aggregated count in the watch-summary projection.
type watchConditionGroupCount struct {
	Key       SummaryKey
	Count     int
	ScopeKind string
	Scope     string
}

// watchConditionSummary is the engine-owned aggregate view of sync conditions
// for watch summaries and related assertions.
type watchConditionSummary struct {
	Groups   []watchConditionGroupCount
	Retrying int
}

func (s watchConditionSummary) VisibleTotal() int {
	total := 0
	for _, group := range s.Groups {
		total += group.Count
	}

	return total
}

func (s watchConditionSummary) ConflictCount() int {
	return 0
}

func (s watchConditionSummary) ActionableCount() int {
	total := 0
	for _, group := range s.Groups {
		if group.Key == SummaryRemoteWriteDenied ||
			group.Key == SummaryAuthenticationRequired {
			continue
		}
		total += group.Count
	}

	return total
}

func (s watchConditionSummary) RemoteBlockedCount() int {
	return s.countForKey(SummaryRemoteWriteDenied)
}

func (s watchConditionSummary) AuthRequiredCount() int {
	return s.countForKey(SummaryAuthenticationRequired)
}

func (s watchConditionSummary) RetryingCount() int {
	return s.Retrying
}

func (s watchConditionSummary) countForKey(key SummaryKey) int {
	total := 0
	for _, group := range s.Groups {
		if group.Key == key {
			total += group.Count
		}
	}

	return total
}

type watchConditionGroupIdentity struct {
	Key       SummaryKey
	ScopeKind string
	Scope     string
}

type watchConditionGroupAccumulator map[watchConditionGroupIdentity]int

func (a watchConditionGroupAccumulator) Add(key SummaryKey, count int, scopeKind, scope string) {
	if key == "" || count <= 0 {
		return
	}

	a[watchConditionGroupIdentity{
		Key:       key,
		ScopeKind: scopeKind,
		Scope:     scope,
	}] += count
}

func (a watchConditionGroupAccumulator) Groups() []watchConditionGroupCount {
	groups := make([]watchConditionGroupCount, 0, len(a))
	for identity, count := range a {
		groups = append(groups, watchConditionGroupCount{
			Key:       identity.Key,
			Count:     count,
			ScopeKind: identity.ScopeKind,
			Scope:     identity.Scope,
		})
	}

	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Key != groups[j].Key {
			return string(groups[i].Key) < string(groups[j].Key)
		}
		if groups[i].ScopeKind != groups[j].ScopeKind {
			return groups[i].ScopeKind < groups[j].ScopeKind
		}

		return groups[i].Scope < groups[j].Scope
	})

	return groups
}

func buildWatchConditionSummary(snapshot *DriveStatusSnapshot) (watchConditionSummary, []watchRemoteBlockedGroup) {
	if snapshot == nil {
		return watchConditionSummary{}, nil
	}

	blockedByScope := groupWatchBlockedRetryWork(snapshot.BlockedRetryWork)
	counts := make(watchConditionGroupAccumulator)

	for i := range snapshot.ObservationIssues {
		key := SummaryKeyForObservationIssue(
			snapshot.ObservationIssues[i].IssueType,
			snapshot.ObservationIssues[i].ScopeKey,
		)
		counts.Add(key, 1, "", "")
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
		counts.Add(SummaryKeyForBlockScope(block.IssueType, block.Key), count, "", "")

		if block.Key.IsPermRemoteWrite() {
			paths := append([]string(nil), projection.paths...)
			sort.Strings(paths)
			remoteGroups = append(remoteGroups, watchRemoteBlockedGroup{
				ScopeKey:     block.Key,
				BoundaryPath: visibleRemoteBoundaryPath(block.ScopePath()),
				BlockedPaths: paths,
			})
		}
	}

	sort.Slice(remoteGroups, func(i, j int) bool {
		return remoteGroups[i].BoundaryPath < remoteGroups[j].BoundaryPath
	})

	return watchConditionSummary{
		Groups:   counts.Groups(),
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

func visibleRemoteBoundaryPath(boundary string) string {
	if boundary == "" {
		return "/"
	}

	return boundary
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

		signature := strings.Join(group.BlockedPaths, "\x00")
		current[group.ScopeKey] = signature

		switch previous, ok := rt.lastRemoteBlocked[group.ScopeKey]; {
		case !ok:
			rt.engine.logger.Warn("shared-folder writes blocked",
				slog.String("boundary", group.BoundaryPath),
				slog.Int("blocked_writes", len(group.BlockedPaths)),
			)
		case previous != signature:
			rt.engine.logger.Warn("shared-folder writes still blocked",
				slog.String("boundary", group.BoundaryPath),
				slog.Int("blocked_writes", len(group.BlockedPaths)),
			)
		}
	}

	for scopeKey := range rt.lastRemoteBlocked {
		if _, ok := current[scopeKey]; ok {
			continue
		}
		rt.engine.logger.Info("shared-folder write block cleared",
			slog.String("boundary", visibleRemoteBoundaryPath(DescribeScopeKey(scopeKey).ScopePath())),
		)
	}

	rt.lastRemoteBlocked = current
}

func watchSummarySignature(summary watchConditionSummary) (string, string) {
	parts := make([]string, 0, len(summary.Groups))
	for i := range summary.Groups {
		parts = append(parts, fmt.Sprintf("%d %s", summary.Groups[i].Count, summary.Groups[i].Key))
	}
	sort.Strings(parts)
	breakdown := strings.Join(parts, ", ")
	return fmt.Sprintf("%d|%s", summary.VisibleTotal(), breakdown), breakdown
}
