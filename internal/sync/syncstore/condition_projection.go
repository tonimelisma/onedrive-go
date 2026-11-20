package syncstore

import (
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// StoredConditionGroup is the raw cross-authority condition projection built
// from the durable sync authorities. It intentionally stays machine-oriented so
// watch logging, status rendering, and future debug reads can share one
// grouping pass without taking on each other's presentation policy.
type StoredConditionGroup struct {
	ConditionKey  syncdomain.ConditionKey
	ConditionType syncdomain.IssueType
	ScopeKey      syncdomain.ScopeKey
	Count         int
	Paths         []string
}

type storedConditionGroupKey struct {
	conditionKey syncdomain.ConditionKey
	scopeKey     string
}

// ProjectStoredConditionGroups collapses durable observation issues,
// block scopes, blocked retry rows, and held deletes into one raw grouped
// view.
func ProjectStoredConditionGroups(snapshot *DriveStatusSnapshot) []StoredConditionGroup {
	if snapshot == nil {
		return nil
	}

	blockedByScope := GroupBlockedRetryWork(snapshot.BlockedRetryWork)
	groupIndex := make(map[storedConditionGroupKey]int)
	groups := make([]StoredConditionGroup, 0, len(snapshot.ObservationIssues)+len(snapshot.BlockScopes)+1)

	for i := range snapshot.ObservationIssues {
		issue := snapshot.ObservationIssues[i]
		group := ensureStoredConditionGroup(
			&groups,
			groupIndex,
			syncdomain.ConditionKeyForStoredCondition(issue.IssueType, issue.ScopeKey),
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
			conditionKey: syncdomain.ConditionKeyForStoredCondition(block.Key.ConditionType(), block.Key),
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
		scopeKey := syncdomain.SKDeleteSafety(syncdomain.DeleteSafetySide(held.Side))
		group := ensureStoredConditionGroup(
			&groups,
			groupIndex,
			syncdomain.ConditionDeletesHeld,
			syncdomain.IssueDeletesHeld,
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

// UnsyncedIssueCount is how many observation issues in the snapshot keep a
// file unsynced: an issue counts when status lists it (storedConditionListed,
// the filter ProjectStoredConditionGroups applies) and its type leaves a file
// unsynced (IssueTypeLeavesFileUnsynced). Retry work is not read: every
// retry row a live pass leaves belongs to an action of that pass's plan, so
// the run's own failed and left-for-next-sync counts already hold it. Held
// deletes are a deliberate hold and are not read either.
func (s *DriveStatusSnapshot) UnsyncedIssueCount() int {
	if s == nil {
		return 0
	}

	count := 0
	for i := range s.ObservationIssues {
		issue := &s.ObservationIssues[i]
		if storedConditionListed(issue.IssueType, issue.ScopeKey) && syncdomain.IssueTypeLeavesFileUnsynced(issue.IssueType) {
			count++
		}
	}

	return count
}

// InterruptedRemovalCount is how many files in the snapshot sync set aside
// while removing them and never moved out (local_interrupted_removal), as
// status lists them: the user's content, which keeps a local tree that would
// be removed (Report.LocalTreeHolds).
func (s *DriveStatusSnapshot) InterruptedRemovalCount() int {
	if s == nil {
		return 0
	}

	count := 0
	for i := range s.ObservationIssues {
		issue := &s.ObservationIssues[i]
		if issue.IssueType == syncdomain.IssueLocalInterruptedRemoval && storedConditionListed(issue.IssueType, issue.ScopeKey) {
			count++
		}
	}

	return count
}

// storedConditionListed reports whether status lists a stored condition
// record: ProjectStoredConditionGroups leaves out a record with no condition
// family (ensureStoredConditionGroup).
func storedConditionListed(conditionType syncdomain.IssueType, scopeKey syncdomain.ScopeKey) bool {
	return syncdomain.ConditionKeyForStoredCondition(conditionType, scopeKey) != ""
}

// projectActionableRetryWork adds the work the executor could not do because
// of a condition only the user can clear (OneDrive refused the path as too
// long, a folder is not writable) to the scanner's findings of the same
// condition, instead of an anonymous "Retrying" count (R-2.10.71).
func projectActionableRetryWork(
	groups *[]StoredConditionGroup,
	groupIndex map[storedConditionGroupKey]int,
	rows []syncdomain.RetryWorkRow,
) {
	for i := range rows {
		row := rows[i]
		group := ensureStoredConditionGroup(
			groups,
			groupIndex,
			syncdomain.ConditionKeyForStoredCondition(row.ConditionType, syncdomain.ScopeKey{}),
			row.ConditionType,
			syncdomain.ScopeKey{},
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
	conditionKey syncdomain.ConditionKey,
	conditionType syncdomain.IssueType,
	scopeKey syncdomain.ScopeKey,
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
			return syncdomain.ConditionKeyLess(left.ConditionKey, right.ConditionKey)
		}

		return left.ScopeKey.String() < right.ScopeKey.String()
	})
}
