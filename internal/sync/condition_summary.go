package sync

import "sort"

// ConditionGroupCount is one derived visible condition family with its aggregated
// count in the read-only status/watch summary projection.
type ConditionGroupCount struct {
	Key       SummaryKey
	Count     int
	ScopeKind string
	Scope     string
}

// ConditionSummary is the store-owned aggregate view of visible conditions for status
// and watch summaries. It centralizes how actionable rows and special derived
// scopes count toward summary output.
type ConditionSummary struct {
	Groups   []ConditionGroupCount
	Retrying int
}

func (s ConditionSummary) VisibleTotal() int {
	total := 0
	for _, group := range s.Groups {
		total += group.Count
	}

	return total
}

func (s ConditionSummary) ConflictCount() int {
	return 0
}

func (s ConditionSummary) ActionableCount() int {
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

func (s ConditionSummary) RemoteBlockedCount() int {
	return s.countForKey(SummaryRemoteWriteDenied)
}

func (s ConditionSummary) AuthRequiredCount() int {
	return s.countForKey(SummaryAuthenticationRequired)
}

func (s ConditionSummary) RetryingCount() int {
	return s.Retrying
}

func (s ConditionSummary) countForKey(key SummaryKey) int {
	total := 0
	for _, group := range s.Groups {
		if group.Key == key {
			total += group.Count
		}
	}

	return total
}

type conditionGroupIdentity struct {
	Key       SummaryKey
	ScopeKind string
	Scope     string
}

type conditionGroupAccumulator map[conditionGroupIdentity]int

func (a conditionGroupAccumulator) Add(key SummaryKey, count int, scopeKind, scope string) {
	if key == "" || count <= 0 {
		return
	}

	a[conditionGroupIdentity{
		Key:       key,
		ScopeKind: scopeKind,
		Scope:     scope,
	}] += count
}

func (a conditionGroupAccumulator) Groups() []ConditionGroupCount {
	groups := make([]ConditionGroupCount, 0, len(a))
	for identity, count := range a {
		groups = append(groups, ConditionGroupCount{
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
