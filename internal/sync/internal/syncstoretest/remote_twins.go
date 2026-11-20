package syncstoretest

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

func TwinIssuePaths(t *testing.T, mgr *syncstore.SyncStore) []string {
	t.Helper()

	issues, err := mgr.ListObservationIssues(t.Context())
	require.NoError(t, err)
	var paths []string
	for i := range issues {
		if issues[i].IssueType == syncdomain.IssueRemoteNameTwin {
			paths = append(paths, issues[i].Path)
		}
	}

	return paths
}
