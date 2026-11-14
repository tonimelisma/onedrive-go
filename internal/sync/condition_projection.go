package sync

import "sort"

// StoredConditionGroup is the raw cross-authority condition projection built
// from the durable sync authorities. It intentionally stays machine-oriented so
// watch logging, status rendering, and future debug reads can share one
// grouping pass without taking on each other's presentation policy.
type StoredConditionGroup struct {
	ConditionKey  ConditionKey
	ConditionType IssueType
	ScopeKey      ScopeKey
	Count         int
	Paths         []string
}

type storedConditionGroupKey struct {
	conditionKey ConditionKey
	scopeKey     string
}

// ProjectStoredConditionGroups collapses durable observation issues,
// block scopes, blocked retry rows, and held deletes into one raw grouped
// view.
func ProjectStoredConditionGroups(snapshot *DriveStatusSnapshot) []StoredConditionGroup {
	if snapshot == nil {
		return nil
	}

	blockedByScope := groupBlockedRetryWork(snapshot.BlockedRetryWork)
	groupIndex := make(map[storedConditionGroupKey]int)
	groups := make([]StoredConditionGroup, 0, len(snapshot.ObservationIssues)+len(snapshot.BlockScopes)+1)

	for i := range snapshot.ObservationIssues {
		issue := snapshot.ObservationIssues[i]
		group := ensureStoredConditionGroup(
			&groups,
			groupIndex,
			conditionKeyForStoredCondition(issue.IssueType, issue.ScopeKey),
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

	projectActionableRetryWork(&groups, groupIndex, snapshot.ActionableRetryWork)

	for i := range snapshot.BlockScopes {
		block := snapshot.BlockScopes[i]
		if block == nil {
			continue
		}

		projection := blockedByScope[block.Key]
		groupKey := storedConditionGroupKey{
			conditionKey: conditionKeyForStoredCondition(block.Key.ConditionType(), block.Key),
			scopeKey:     block.Key.String(),
		}
		existingCount := 0
		if idx, ok := groupIndex[groupKey]; ok {
			existingCount = groups[idx].Count
		}

		group := ensureStoredConditionGroup(
			&groups,
			groupIndex,
			groupKey.conditionKey,
			block.Key.ConditionType(),
			block.Key,
		)
		if group == nil {
			continue
		}
		if projection.Count > 0 {
			group.Count += projection.Count
		} else if existingCount == 0 {
			// A scope with no linked blocked rows still contributes one active
			// condition, but do not double count when observation rows already
			// carry the same scope key.
			group.Count++
		}
		if len(projection.Paths) > 0 {
			group.Paths = append(group.Paths, projection.Paths...)
		}
	}

	for i := range snapshot.HeldDeletes {
		held := snapshot.HeldDeletes[i]
		scopeKey := sKDeleteSafety(deleteSafetySide(held.Side))
		group := ensureStoredConditionGroup(
			&groups,
			groupIndex,
			ConditionDeletesHeld,
			issueDeletesHeld,
			scopeKey,
		)
		if group == nil {
			continue
		}
		group.Count++
		group.Paths = append(group.Paths, held.Path)
	}

	finalizeStoredConditionGroups(groups)
	return groups
}

// unsyncedIssueCount is how many observation issues in the snapshot keep a
// file unsynced: an issue counts when status lists it (storedConditionListed,
// the filter ProjectStoredConditionGroups applies) and its type leaves a file
// unsynced (issueTypeLeavesFileUnsynced). Retry work is not read: every
// retry row a live pass leaves belongs to an action of that pass's plan, so
// the run's own failed and left-for-next-sync counts already hold it. Held
// deletes are a deliberate hold and are not read either.
func (s *DriveStatusSnapshot) unsyncedIssueCount() int {
	if s == nil {
		return 0
	}

	count := 0
	for i := range s.ObservationIssues {
		issue := &s.ObservationIssues[i]
		if storedConditionListed(issue.IssueType, issue.ScopeKey) && issueTypeLeavesFileUnsynced(issue.IssueType) {
			count++
		}
	}

	return count
}

// interruptedRemovalCount is how many files in the snapshot sync set aside
// while removing them and never moved out (local_interrupted_removal), as
// status lists them: the user's content, which keeps a local tree that would
// be removed (Report.LocalTreeHolds).
func (s *DriveStatusSnapshot) interruptedRemovalCount() int {
	if s == nil {
		return 0
	}

	count := 0
	for i := range s.ObservationIssues {
		issue := &s.ObservationIssues[i]
		if issue.IssueType == issueLocalInterruptedRemoval && storedConditionListed(issue.IssueType, issue.ScopeKey) {
			count++
		}
	}

	return count
}

// storedConditionListed reports whether status lists a stored condition
// record: ProjectStoredConditionGroups leaves out a record with no condition
// family (ensureStoredConditionGroup).
func storedConditionListed(conditionType IssueType, scopeKey ScopeKey) bool {
	return conditionKeyForStoredCondition(conditionType, scopeKey) != ""
}

// projectActionableRetryWork adds the work the executor could not do because
// of a condition only the user can clear (OneDrive refused the path as too
// long, a folder is not writable) to the scanner's findings of the same
// condition, instead of an anonymous "Retrying" count (R-2.10.71).
func projectActionableRetryWork(
	groups *[]StoredConditionGroup,
	groupIndex map[storedConditionGroupKey]int,
	rows []RetryWorkRow,
) {
	for i := range rows {
		row := rows[i]
		group := ensureStoredConditionGroup(
			groups,
			groupIndex,
			conditionKeyForStoredCondition(row.ConditionType, ScopeKey{}),
			row.ConditionType,
			ScopeKey{},
		)
		if group == nil {
			continue
		}
		group.Count++
		group.Paths = append(group.Paths, row.Path)
	}
}

func ensureStoredConditionGroup(
	groups *[]StoredConditionGroup,
	groupIndex map[storedConditionGroupKey]int,
	conditionKey ConditionKey,
	conditionType IssueType,
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
