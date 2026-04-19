package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildGroupedConditionProjection_MergesScopeFamiliesAndSortsPaths(t *testing.T) {
	t.Parallel()

	projection := buildGroupedConditionProjection([]VisibleConditionGroup{
		{
			SummaryKey: SummaryRemoteWriteDenied,
			IssueType:  IssueRemoteWriteDenied,
			ScopeKey:   SKPermRemoteWrite("Shared/Docs"),
			Paths:      []string{"Shared/Docs/b.txt", "Shared/Docs/a.txt"},
			Count:      2,
		},
		{
			SummaryKey: SummaryRemoteWriteDenied,
			IssueType:  IssueRemoteWriteDenied,
			ScopeKey:   SKPermRemoteWrite("Shared/Docs"),
			Paths:      []string{"Shared/Docs/a.txt", "Shared/Docs/c.txt"},
			Count:      1,
		},
		{
			SummaryKey: SummaryInvalidFilename,
			IssueType:  IssueInvalidFilename,
			Paths:      []string{"/bad:name.txt"},
			Count:      1,
		},
	})

	require.Len(t, projection.Conditions, 2)

	remote := projection.Conditions[0]
	assert.Equal(t, SummaryRemoteWriteDenied, remote.SummaryKey)
	assert.Equal(t, IssueRemoteWriteDenied, remote.PrimaryIssueType)
	assert.Equal(t, SKPermRemoteWrite("Shared/Docs"), remote.ScopeKey)
	assert.Equal(t, "Shared/Docs", remote.ScopeLabel)
	assert.Equal(t, 3, remote.Count)
	assert.Equal(t, []string{"Shared/Docs/a.txt", "Shared/Docs/b.txt", "Shared/Docs/c.txt"}, remote.Paths)

	invalid := projection.Conditions[1]
	assert.Equal(t, SummaryInvalidFilename, invalid.SummaryKey)
	assert.Equal(t, IssueInvalidFilename, invalid.PrimaryIssueType)
	assert.Equal(t, 1, invalid.Count)
	assert.Equal(t, []string{"/bad:name.txt"}, invalid.Paths)
}

func TestGroupedConditionProjectionBuilder_IgnoresEmptyConditionsAndDedupesWithinScope(t *testing.T) {
	t.Parallel()

	builder := newGroupedConditionProjectionBuilder()
	builder.addGroupedPath("", IssueInvalidFilename, ScopeKey{}, []string{"ignored.txt"}, 3)
	builder.addGroupedPath(SummaryInvalidFilename, IssueInvalidFilename, ScopeKey{}, []string{"ignored-zero.txt"}, 0)
	builder.addGroupedPath(SummaryQuotaExceeded, IssueQuotaExceeded, SKQuotaOwn(), []string{"quota/b", "quota/a"}, 2)
	builder.addGroupedPath(SummaryQuotaExceeded, IssueQuotaExceeded, SKQuotaOwn(), []string{"quota/a", "quota/c"}, 1)

	projection := builder.projection()
	require.Len(t, projection.Conditions, 1)

	condition := projection.Conditions[0]
	assert.Equal(t, SummaryQuotaExceeded, condition.SummaryKey)
	assert.Equal(t, IssueQuotaExceeded, condition.PrimaryIssueType)
	assert.Equal(t, SKQuotaOwn(), condition.ScopeKey)
	assert.Equal(t, "this drive storage", condition.ScopeLabel)
	assert.Equal(t, 3, condition.Count)
	assert.Equal(t, []string{"quota/a", "quota/b", "quota/c"}, condition.Paths)
}
