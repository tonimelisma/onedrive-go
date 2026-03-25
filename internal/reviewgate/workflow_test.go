package reviewgate

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.3.5
func TestReviewGateWorkflowDefinition(t *testing.T) {
	workflowFile, err := os.ReadFile("../../.github/workflows/review-gate.yml")
	require.NoError(t, err)

	workflowYAML := string(workflowFile)

	assert.Contains(t, workflowYAML, "name: review-gate")
	assert.Contains(t, workflowYAML, "pull_request_target:")
	assert.Contains(t, workflowYAML, "ready_for_review")
	assert.Contains(t, workflowYAML, "synchronize")
	assert.Contains(t, workflowYAML, "pull_request_review:")
	assert.Contains(t, workflowYAML, "submitted")
	assert.Contains(t, workflowYAML, "dismissed")
	assert.Contains(t, workflowYAML, "ref: ${{ github.event.pull_request.base.sha }}")
	assert.Contains(t, workflowYAML, "go run ./cmd/review-gate")
	assert.Contains(t, workflowYAML, "jobs:\n  review-gate:")
	assert.NotContains(t, workflowYAML, "\n    paths:")
	assert.NotContains(t, workflowYAML, "github.event.pull_request.head.sha")
}
