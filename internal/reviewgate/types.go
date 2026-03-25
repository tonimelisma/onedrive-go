package reviewgate

import "time"

const (
	defaultCodexReviewLogin = "codex"
	docsOnlyMessage         = "docs-only PR"
	draftPRMessage          = "draft PR"
)

// Config controls reviewer matching for the review gate.
type Config struct {
	CodexReviewLogin string
}

// PullRequest captures the gate-relevant PR metadata from the GitHub event.
type PullRequest struct {
	Repository string
	Number     int
	Draft      bool
	HeadSHA    string
}

// ReviewState is the subset of GitHub review states that affect the gate.
type ReviewState string

const (
	ReviewStateApproved         ReviewState = "APPROVED"
	ReviewStateChangesRequested ReviewState = "CHANGES_REQUESTED"
	ReviewStateCommented        ReviewState = "COMMENTED"
)

// Review stores the submitted review metadata needed to evaluate the gate.
type Review struct {
	ID            int64
	ReviewerLogin string
	CommitID      string
	State         ReviewState
	SubmittedAt   time.Time
}

// DecisionStatus describes the gate outcome.
type DecisionStatus string

const (
	DecisionFail DecisionStatus = "fail"
	DecisionPass DecisionStatus = "pass"
	DecisionSkip DecisionStatus = "skip"
)

// Decision is the final gate result communicated back to CI.
type Decision struct {
	Status  DecisionStatus
	Message string
}
