package sync

import "sort"

// BlockedRetryGroup is the raw durable projection of blocked retry_work rows
// for one scope. It is intentionally machine-oriented so watch/status can
// share one grouping pass while keeping phrasing and rendering separate.
type BlockedRetryGroup struct {
	Count int
	Paths []string
}

// GroupBlockedRetryWork groups blocked retry_work rows by scope key, keeping a
// stable path list for downstream watch/status projections.
func GroupBlockedRetryWork(rows []RetryWorkRow) map[ScopeKey]BlockedRetryGroup {
	grouped := make(map[ScopeKey]BlockedRetryGroup)
	for i := range rows {
		scopeKey := rows[i].ScopeKey
		if scopeKey.IsZero() {
			continue
		}

		group := grouped[scopeKey]
		group.Count++
		if rows[i].Path != "" {
			group.Paths = append(group.Paths, rows[i].Path)
		}
		grouped[scopeKey] = group
	}

	for key, group := range grouped {
		sort.Strings(group.Paths)
		group.Paths = sortedUniqueStrings(group.Paths)
		grouped[key] = group
	}

	return grouped
}

func sortedUniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}

	result := values[:1]
	for i := 1; i < len(values); i++ {
		if values[i] == values[i-1] {
			continue
		}
		result = append(result, values[i])
	}

	return result
}
