package sync

import (
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

type watchRemoteBlockedGroup struct {
	ScopeKey     syncdomain.ScopeKey
	BlockedPaths []string
}

// watchConditionCount is one raw condition-key count in the watch summary.
type watchConditionCount struct {
	Key   syncdomain.ConditionKey
	Count int
}

// watchConditionSummary is the raw engine-owned aggregate view consumed by
// watch logging and related assertions.
type watchConditionSummary struct {
	Counts         []watchConditionCount
	ConditionTotal int
	Retrying       int
	RemoteBlocked  []watchRemoteBlockedGroup
}

func buildWatchConditionSummary(snapshot *syncstore.DriveStatusSnapshot) watchConditionSummary {
	if snapshot == nil {
		return watchConditionSummary{}
	}

	groups := syncstore.ProjectStoredConditionGroups(snapshot)
	remoteGroups := buildWatchRemoteBlockedGroups(snapshot)

	summaryCounts := watchConditionCounts(groups)

	return watchConditionSummary{
		Counts:         summaryCounts,
		ConditionTotal: watchConditionCountTotal(summaryCounts),
		Retrying:       snapshot.RetryingItems,
		RemoteBlocked:  remoteGroups,
	}
}

func buildWatchRemoteBlockedGroups(snapshot *syncstore.DriveStatusSnapshot) []watchRemoteBlockedGroup {
	if snapshot == nil {
		return nil
	}

	blockedByScope := syncstore.GroupBlockedRetryWork(snapshot.BlockedRetryWork)
	var remoteGroups []watchRemoteBlockedGroup

	for i := range snapshot.BlockScopes {
		block := snapshot.BlockScopes[i]
		if block == nil || !block.Key.IsPermRemoteWrite() {
			continue
		}

		remoteGroups = append(remoteGroups, watchRemoteBlockedGroup{
			ScopeKey:     block.Key,
			BlockedPaths: append([]string(nil), blockedByScope[block.Key].Paths...),
		})
	}

	sort.Slice(remoteGroups, func(i, j int) bool {
		return remoteGroups[i].ScopeKey.String() < remoteGroups[j].ScopeKey.String()
	})

	return remoteGroups
}

func watchConditionCounts(groups []syncstore.StoredConditionGroup) []watchConditionCount {
	if len(groups) == 0 {
		return nil
	}

	accumulator := make(map[syncdomain.ConditionKey]int)
	for i := range groups {
		group := groups[i]
		if group.ConditionKey == "" || group.Count <= 0 {
			continue
		}
		accumulator[group.ConditionKey] += group.Count
	}

	counts := make([]watchConditionCount, 0, len(accumulator))
	for key, count := range accumulator {
		counts = append(counts, watchConditionCount{
			Key:   key,
			Count: count,
		})
	}

	sort.Slice(counts, func(i, j int) bool {
		return syncdomain.ConditionKeyLess(counts[i].Key, counts[j].Key)
	})

	return counts
}

func watchConditionCountTotal(counts []watchConditionCount) int {
	total := 0
	for i := range counts {
		total += counts[i].Count
	}

	return total
}
