package sync

import "sort"

// StoredConditionGroup is the raw cross-authority condition projection built
// from the durable sync authorities. It intentionally stays machine-oriented so
// watch logging, status rendering, and future debug reads can share one
// grouping pass without taking on each other's presentation policy.
type StoredConditionGroup struct {
	ConditionKey  ConditionKey
	ConditionType string
	ScopeKey      ScopeKey
	Count         int
	Paths         []string
}

type storedConditionGroupKey struct {
	conditionKey ConditionKey
	scopeKey     string
}

// ProjectStoredConditionGroups collapses durable observation issues,
// block scopes, and blocked retry rows into one raw grouped view.
func ProjectStoredConditionGroups(snapshot *DriveStatusSnapshot) []StoredConditionGroup {
	if snapshot == nil {
		return nil
	}

	blockedByScope := GroupBlockedRetryWork(snapshot.BlockedRetryWork)
	groupIndex := make(map[storedConditionGroupKey]int)
	groups := make([]StoredConditionGroup, 0, len(snapshot.ObservationIssues)+len(snapshot.BlockScopes))

	for i := range snapshot.ObservationIssues {
		issue := snapshot.ObservationIssues[i]
		group := ensureStoredConditionGroup(
			&groups,
			groupIndex,
			ConditionKeyForStoredCondition(issue.IssueType, issue.ScopeKey),
			issue.IssueType,
			issue.ScopeKey,
		)
		if group == nil {
			continue
		}
		group.Count++
		if issue.Path != "" {
			group.Paths = append(group.Paths, issue.Path)
		}
	}

	for i := range snapshot.BlockScopes {
		block := snapshot.BlockScopes[i]
		if block == nil {
			continue
		}

		projection := blockedByScope[block.Key]
		count := projection.Count
		if count == 0 {
			count = 1
		}

		group := ensureStoredConditionGroup(
			&groups,
			groupIndex,
			ConditionKeyForStoredCondition(block.ConditionType, block.Key),
			block.ConditionType,
			block.Key,
		)
		if group == nil {
			continue
		}
		group.Count += count
		if len(projection.Paths) > 0 {
			group.Paths = append(group.Paths, projection.Paths...)
		}
	}

	finalizeStoredConditionGroups(groups)
	return groups
}

func ensureStoredConditionGroup(
	groups *[]StoredConditionGroup,
	groupIndex map[storedConditionGroupKey]int,
	conditionKey ConditionKey,
	conditionType string,
	scopeKey ScopeKey,
) *StoredConditionGroup {
	if conditionKey == "" {
		return nil
	}

	key := storedConditionGroupKey{
		conditionKey: conditionKey,
		scopeKey:     scopeKey.String(),
	}
	if idx, ok := groupIndex[key]; ok {
		return &(*groups)[idx]
	}

	*groups = append(*groups, StoredConditionGroup{
		ConditionKey:  conditionKey,
		ConditionType: conditionType,
		ScopeKey:      scopeKey,
	})
	groupIndex[key] = len(*groups) - 1

	return &(*groups)[len(*groups)-1]
}

func finalizeStoredConditionGroups(groups []StoredConditionGroup) {
	for i := range groups {
		sort.Strings(groups[i].Paths)
		groups[i].Paths = sortedUniqueStrings(groups[i].Paths)
	}

	sort.Slice(groups, func(i, j int) bool {
		left := groups[i]
		right := groups[j]
		if left.ConditionKey != right.ConditionKey {
			return ConditionKeyLess(left.ConditionKey, right.ConditionKey)
		}

		return left.ScopeKey.String() < right.ScopeKey.String()
	})
}
