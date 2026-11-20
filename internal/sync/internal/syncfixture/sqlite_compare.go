package syncfixture

import (
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func ComparisonKindsByPath(rows []syncdomain.SQLiteComparisonRow) map[string]string {
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].Path] = rows[i].ComparisonKind.String()
	}

	return out
}

func ReconciliationKindsByPath(rows []syncdomain.SQLiteReconciliationRow) map[string]string {
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].Path] = rows[i].ReconciliationKind.String()
	}

	return out
}
