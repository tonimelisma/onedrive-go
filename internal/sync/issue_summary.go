package sync

import "sort"

// IssueGroupCount is one derived visible issue family with its aggregated
// count in the read-only status/watch summary projection.
type IssueGroupCount struct {
	Key       SummaryKey
	Count     int
	ScopeKind string
	Scope     string
}

// IssueSummary is the store-owned aggregate view of visible issues for status
// and watch summaries. It centralizes how actionable rows and special derived
// scopes count toward summary output.
type IssueSummary struct {
	Groups   []IssueGroupCount
	Retrying int
}

func (s IssueSummary) VisibleTotal() int {
	total := 0
	for _, group := range s.Groups {
		total += group.Count
	}

	return total
}

func (s IssueSummary) ConflictCount() int {
	return 0
}

func (s IssueSummary) ActionableCount() int {
	total := 0
	for _, group := range s.Groups {
		if group.Key == SummarySharedFolderWritesBlocked ||
			group.Key == SummaryAuthenticationRequired {
			continue
		}
		total += group.Count
	}

	return total
}

func (s IssueSummary) RemoteBlockedCount() int {
	return s.countForKey(SummarySharedFolderWritesBlocked)
}

func (s IssueSummary) AuthRequiredCount() int {
	return s.countForKey(SummaryAuthenticationRequired)
}

func (s IssueSummary) RetryingCount() int {
	return s.Retrying
}

func (s IssueSummary) countForKey(key SummaryKey) int {
	total := 0
	for _, group := range s.Groups {
		if group.Key == key {
			total += group.Count
		}
	}

	return total
}

type issueGroupIdentity struct {
	Key       SummaryKey
	ScopeKind string
	Scope     string
}

type issueGroupAccumulator map[issueGroupIdentity]int

func (a issueGroupAccumulator) Add(key SummaryKey, count int, scopeKind, scope string) {
	if key == "" || count <= 0 {
		return
	}

	a[issueGroupIdentity{
		Key:       key,
		ScopeKind: scopeKind,
		Scope:     scope,
	}] += count
}

func (a issueGroupAccumulator) Groups() []IssueGroupCount {
	groups := make([]IssueGroupCount, 0, len(a))
	for identity, count := range a {
		groups = append(groups, IssueGroupCount{
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
