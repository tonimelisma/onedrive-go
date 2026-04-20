package sync

import (
	"sort"
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

// watchConditionSummary is the raw engine-owned aggregate view consumed by
// watch logging and related assertions.
type watchConditionSummary struct {
	Counts         []watchConditionCount
	ConditionTotal int
	Retrying       int
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
		return ConditionKeyLess(counts[i].Key, counts[j].Key)
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

	summaryCounts := counts.Counts()

	return watchConditionSummary{
		Counts:         summaryCounts,
		ConditionTotal: watchConditionCountTotal(summaryCounts),
		Retrying:       snapshot.RetryingItems,
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

func watchConditionCountTotal(counts []watchConditionCount) int {
	total := 0
	for i := range counts {
		total += counts[i].Count
	}

	return total
}
