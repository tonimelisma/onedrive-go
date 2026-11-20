package sync

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func twinIssuePaths(t *testing.T, mgr *SyncStore) []string {
	t.Helper()

	issues, err := mgr.ListObservationIssues(t.Context())
	require.NoError(t, err)
	var paths []string
	for i := range issues {
		if issues[i].IssueType == IssueRemoteNameTwin {
			paths = append(paths, issues[i].Path)
		}
	}

	return paths
}
