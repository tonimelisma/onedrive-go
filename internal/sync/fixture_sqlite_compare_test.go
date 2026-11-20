package sync

func comparisonKindsByPath(rows []SQLiteComparisonRow) map[string]string {
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].Path] = rows[i].ComparisonKind.String()
	}

	return out
}

func reconciliationKindsByPath(rows []SQLiteReconciliationRow) map[string]string {
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].Path] = rows[i].ReconciliationKind.String()
	}

	return out
}
