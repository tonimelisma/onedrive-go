package reviewgate

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePullRequestReader struct {
	changedFiles ChangedFiles
	filesErr     error
	reviews      []Review
	reviewsErr   error
}

func (f fakePullRequestReader) ListChangedFiles(
	ctx context.Context,
	repository string,
	pullNumber int,
	expectedFileCount int,
) (ChangedFiles, error) {
	return f.changedFiles, f.filesErr
}

func (f fakePullRequestReader) ListReviews(ctx context.Context, repository string, pullNumber int) ([]Review, error) {
	return f.reviews, f.reviewsErr
}

// Validates: R-6.3.5, R-6.3.6
func TestGateEvaluate(t *testing.T) {
	timestamp := time.Date(2026, time.March, 25, 9, 0, 0, 0, time.UTC)

	testCases := []struct {
		name            string
		pullRequest     PullRequest
		reader          fakePullRequestReader
		config          Config
		expectedStatus  DecisionStatus
		expectedMessage string
	}{
		{
			name: "draft pull request skips",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     1,
				Draft:      true,
				HeadSHA:    "head-sha",
			},
			expectedStatus:  DecisionSkip,
			expectedMessage: draftPRMessage,
		},
		{
			name: "docs only pull request skips",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     2,
				HeadSHA:    "head-sha",
			},
			reader: fakePullRequestReader{
				changedFiles: completeChangedFiles(
					changedDocFile("README.md"),
					changedDocFile("spec/design/ci-review-gate.md"),
					changedDocFile("TODO.md"),
				),
			},
			expectedStatus:  DecisionSkip,
			expectedMessage: docsOnlyMessage,
		},
		{
			name: "control plane markdown still requires review",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     3,
				HeadSHA:    "head-sha",
			},
			reader: fakePullRequestReader{
				changedFiles: completeChangedFiles(changedCodeFile("CLAUDE.md")),
			},
			expectedStatus:  DecisionFail,
			expectedMessage: "missing Codex review on current head SHA",
		},
		{
			name: "renamed non doc file into spec still requires review",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     4,
				HeadSHA:    "head-sha",
			},
			reader: fakePullRequestReader{
				changedFiles: completeChangedFiles(ChangedFile{
					Path:         "spec/reference/moved.md",
					PreviousPath: "internal/reviewgate/gate.go",
					Status:       ChangedFileStatusRenamed,
				}),
			},
			expectedStatus:  DecisionFail,
			expectedMessage: "missing Codex review on current head SHA",
		},
		{
			name: "incomplete docs listing requires review",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     5,
				HeadSHA:    "head-sha",
			},
			reader: fakePullRequestReader{
				changedFiles: ChangedFiles{
					Entries:  []ChangedFile{changedDocFile("spec/design/ci-review-gate.md")},
					Complete: false,
				},
			},
			expectedStatus:  DecisionFail,
			expectedMessage: "missing Codex review on current head SHA",
		},
		{
			name: "missing codex review on head fails",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     6,
				HeadSHA:    "head-sha",
			},
			reader: fakePullRequestReader{
				changedFiles: completeChangedFiles(changedCodeFile("internal/reviewgate/gate.go")),
				reviews: []Review{
					{
						ID:            1,
						ReviewerLogin: "codex",
						CommitID:      "old-sha",
						State:         ReviewStateCommented,
						SubmittedAt:   timestamp,
					},
				},
			},
			expectedStatus:  DecisionFail,
			expectedMessage: "missing Codex review on current head SHA",
		},
		{
			name: "older sha codex review fails",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     7,
				HeadSHA:    "new-sha",
			},
			reader: fakePullRequestReader{
				changedFiles: completeChangedFiles(changedCodeFile("internal/reviewgate/gate.go")),
				reviews: []Review{
					{
						ID:            2,
						ReviewerLogin: "codex",
						CommitID:      "old-sha",
						State:         ReviewStateApproved,
						SubmittedAt:   timestamp,
					},
				},
			},
			expectedStatus:  DecisionFail,
			expectedMessage: "missing Codex review on current head SHA",
		},
		{
			name: "non codex review on head fails",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     8,
				HeadSHA:    "head-sha",
			},
			reader: fakePullRequestReader{
				changedFiles: completeChangedFiles(changedCodeFile("internal/reviewgate/gate.go")),
				reviews: []Review{
					{
						ID:            3,
						ReviewerLogin: "octocat",
						CommitID:      "head-sha",
						State:         ReviewStateCommented,
						SubmittedAt:   timestamp,
					},
				},
			},
			expectedStatus:  DecisionFail,
			expectedMessage: "missing Codex review on current head SHA",
		},
		{
			name: "commented review on head passes",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     9,
				HeadSHA:    "head-sha",
			},
			reader: fakePullRequestReader{
				changedFiles: completeChangedFiles(changedCodeFile("internal/reviewgate/gate.go")),
				reviews: []Review{
					{
						ID:            4,
						ReviewerLogin: "codex",
						CommitID:      "head-sha",
						State:         ReviewStateCommented,
						SubmittedAt:   timestamp,
					},
				},
			},
			expectedStatus:  DecisionPass,
			expectedMessage: "Codex review on current head SHA is commented",
		},
		{
			name: "approved review on head passes",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     10,
				HeadSHA:    "head-sha",
			},
			reader: fakePullRequestReader{
				changedFiles: completeChangedFiles(changedCodeFile("internal/reviewgate/gate.go")),
				reviews: []Review{
					{
						ID:            5,
						ReviewerLogin: "codex",
						CommitID:      "head-sha",
						State:         ReviewStateApproved,
						SubmittedAt:   timestamp,
					},
				},
			},
			expectedStatus:  DecisionPass,
			expectedMessage: "Codex review on current head SHA is approved",
		},
		{
			name: "changes requested on head fails",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     11,
				HeadSHA:    "head-sha",
			},
			reader: fakePullRequestReader{
				changedFiles: completeChangedFiles(changedCodeFile("internal/reviewgate/gate.go")),
				reviews: []Review{
					{
						ID:            6,
						ReviewerLogin: "codex",
						CommitID:      "head-sha",
						State:         ReviewStateChangesRequested,
						SubmittedAt:   timestamp,
					},
				},
			},
			expectedStatus:  DecisionFail,
			expectedMessage: "Codex review on current head SHA requests changes",
		},
		{
			name: "older changes requested superseded by newer head review passes",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     12,
				HeadSHA:    "new-sha",
			},
			reader: fakePullRequestReader{
				changedFiles: completeChangedFiles(changedCodeFile("internal/reviewgate/gate.go")),
				reviews: []Review{
					{
						ID:            7,
						ReviewerLogin: "codex",
						CommitID:      "old-sha",
						State:         ReviewStateChangesRequested,
						SubmittedAt:   timestamp,
					},
					{
						ID:            8,
						ReviewerLogin: "codex",
						CommitID:      "new-sha",
						State:         ReviewStateCommented,
						SubmittedAt:   timestamp.Add(time.Minute),
					},
				},
			},
			expectedStatus:  DecisionPass,
			expectedMessage: "Codex review on current head SHA is commented",
		},
		{
			name: "custom reviewer login is honored",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     13,
				HeadSHA:    "head-sha",
			},
			reader: fakePullRequestReader{
				changedFiles: completeChangedFiles(changedCodeFile("internal/reviewgate/gate.go")),
				reviews: []Review{
					{
						ID:            9,
						ReviewerLogin: "openai-codex-app",
						CommitID:      "head-sha",
						State:         ReviewStateApproved,
						SubmittedAt:   timestamp,
					},
				},
			},
			config: Config{
				CodexReviewLogin: "openai-codex-app",
			},
			expectedStatus:  DecisionPass,
			expectedMessage: "Codex review on current head SHA is approved",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			gate := NewGate(testCase.reader, testCase.config)

			decision, err := gate.Evaluate(context.Background(), testCase.pullRequest)

			require.NoError(t, err)
			assert.Equal(t, testCase.expectedStatus, decision.Status)
			assert.Equal(t, testCase.expectedMessage, decision.Message)
		})
	}
}

func completeChangedFiles(entries ...ChangedFile) ChangedFiles {
	return ChangedFiles{
		Entries:  entries,
		Complete: true,
	}
}

func changedDocFile(path string) ChangedFile {
	return ChangedFile{
		Path:   path,
		Status: ChangedFileStatusModified,
	}
}

func changedCodeFile(path string) ChangedFile {
	return ChangedFile{
		Path:   path,
		Status: ChangedFileStatusModified,
	}
}

func TestGateEvaluateErrors(t *testing.T) {
	testCases := []struct {
		name           string
		pullRequest    PullRequest
		reader         fakePullRequestReader
		expectedErrMsg string
	}{
		{
			name: "missing repository fails fast",
			pullRequest: PullRequest{
				Number:  1,
				HeadSHA: "head-sha",
			},
			expectedErrMsg: "review gate: missing repository",
		},
		{
			name: "changed files error is wrapped",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     2,
				HeadSHA:    "head-sha",
			},
			reader: fakePullRequestReader{
				filesErr: fmt.Errorf("boom"),
			},
			expectedErrMsg: "review gate: list changed files: boom",
		},
		{
			name: "reviews error is wrapped",
			pullRequest: PullRequest{
				Repository: "tonimelisma/onedrive-go",
				Number:     3,
				HeadSHA:    "head-sha",
			},
			reader: fakePullRequestReader{
				changedFiles: completeChangedFiles(changedCodeFile("internal/reviewgate/gate.go")),
				reviewsErr:   fmt.Errorf("boom"),
			},
			expectedErrMsg: "review gate: list reviews: boom",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			gate := NewGate(testCase.reader, Config{})

			_, err := gate.Evaluate(context.Background(), testCase.pullRequest)

			require.Error(t, err)
			assert.EqualError(t, err, testCase.expectedErrMsg)
		})
	}
}
