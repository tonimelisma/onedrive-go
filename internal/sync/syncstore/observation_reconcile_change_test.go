package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.8.21
func TestBuildObservationReconcilePlan_ReportsChangeOnlyWhenTheIssueSetDiffers(t *testing.T) {
	t.Parallel()

	bad := syncdomain.ObservationIssue{Path: "docs/bad:name.txt", IssueType: syncdomain.IssueInvalidFilename}
	persisted := observationReconcileStoreState{issues: []syncdomain.ObservationIssueRow{
		{Path: bad.Path, IssueType: bad.IssueType},
	}}
	managed := func(issues ...syncdomain.ObservationIssue) *syncdomain.ObservationFindingsBatch {
		batch := syncdomain.NewLocalObservationFindingsBatch()
		batch.Issues = issues
		return &batch
	}

	tests := []struct {
		name    string
		batch   *syncdomain.ObservationFindingsBatch
		state   observationReconcileStoreState
		changes bool
	}{
		{name: "a new finding", batch: managed(bad), state: observationReconcileStoreState{}, changes: true},
		{name: "the same finding re-reported", batch: managed(bad), state: persisted, changes: false},
		{name: "a persistent finding cleared", batch: managed(), state: persisted, changes: true},
		{
			name:    "a known path with a different issue",
			batch:   managed(syncdomain.ObservationIssue{Path: bad.Path, IssueType: syncdomain.IssuePathTooLong}),
			state:   persisted,
			changes: true,
		},
		{name: "nothing before, nothing now", batch: managed(), state: observationReconcileStoreState{}, changes: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			plan, err := buildObservationReconcilePlan(tt.batch, tt.state)
			require.NoError(t, err)
			assert.Equal(t, tt.changes, plan.changesIssues)
		})
	}
}
