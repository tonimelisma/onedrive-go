package sync

import (
	"sort"
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

func buildWatchConditionSummary(snapshot *DriveStatusSnapshot) (ConditionSummary, []watchRemoteBlockedGroup) {
	if snapshot == nil {
		return ConditionSummary{}, nil
	}

	blockedByScope := groupWatchBlockedRetryWork(snapshot.BlockedRetryWork)
	counts := make(conditionGroupAccumulator)

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
				BoundaryPath: visibleRemoteBoundaryPath(block.Key.RemotePath()),
				BlockedPaths: paths,
			})
		}
	}

	sort.Slice(remoteGroups, func(i, j int) bool {
		return remoteGroups[i].BoundaryPath < remoteGroups[j].BoundaryPath
	})

	return ConditionSummary{
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
