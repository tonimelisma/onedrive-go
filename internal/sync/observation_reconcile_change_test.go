package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-2.8.21
func TestBuildObservationReconcilePlan_ReportsChangeOnlyWhenTheIssueSetDiffers(t *testing.T) {
	t.Parallel()

	bad := ObservationIssue{Path: "docs/bad:name.txt", IssueType: IssueInvalidFilename}
	persisted := observationReconcileStoreState{issues: []ObservationIssueRow{
		{Path: bad.Path, IssueType: bad.IssueType},
	}}
	managed := func(issues ...ObservationIssue) *ObservationFindingsBatch {
		batch := newLocalObservationFindingsBatch()
		batch.Issues = issues
		return &batch
	}

	tests := []struct {
		name    string
		batch   *ObservationFindingsBatch
		state   observationReconcileStoreState
		changes bool
	}{
		{name: "a new finding", batch: managed(bad), state: observationReconcileStoreState{}, changes: true},
		{name: "the same finding re-reported", batch: managed(bad), state: persisted, changes: false},
		{name: "a persistent finding cleared", batch: managed(), state: persisted, changes: true},
		{
			name:    "a known path with a different issue",
			batch:   managed(ObservationIssue{Path: bad.Path, IssueType: issuePathTooLong}),
			state:   persisted,
			changes: true,
		},
		{name: "nothing before, nothing now", batch: managed(), state: observationReconcileStoreState{}, changes: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			plan := buildObservationReconcilePlan(tt.batch, tt.state)
			assert.Equal(t, tt.changes, plan.changesIssues)
		})
	}
}
