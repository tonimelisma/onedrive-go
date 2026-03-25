package reviewgate

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// Gate evaluates whether the current PR head has a qualifying Codex review.
type Gate struct {
	config Config
	reader PullRequestReader
}

// NewGate wires the policy and GitHub reader together.
func NewGate(reader PullRequestReader, config Config) Gate {
	return Gate{
		config: config,
		reader: reader,
	}
}

// Evaluate runs the deterministic gate policy for the current PR.
func (g Gate) Evaluate(ctx context.Context, pullRequest PullRequest) (Decision, error) {
	if strings.TrimSpace(pullRequest.Repository) == "" {
		return Decision{}, fmt.Errorf("review gate: missing repository")
	}
	if pullRequest.Number <= 0 {
		return Decision{}, fmt.Errorf("review gate: missing pull request number")
	}
	if strings.TrimSpace(pullRequest.HeadSHA) == "" {
		return Decision{}, fmt.Errorf("review gate: missing pull request head SHA")
	}

	if pullRequest.Draft {
		return Decision{
			Status:  DecisionSkip,
			Message: draftPRMessage,
		}, nil
	}

	changedFiles, err := g.reader.ListChangedFiles(
		ctx,
		pullRequest.Repository,
		pullRequest.Number,
		pullRequest.ChangedFileCount,
	)
	if err != nil {
		return Decision{}, fmt.Errorf("review gate: list changed files: %w", err)
	}
	if isDocsOnlyChangeSet(changedFiles) {
		return Decision{
			Status:  DecisionSkip,
			Message: docsOnlyMessage,
		}, nil
	}

	reviews, err := g.reader.ListReviews(ctx, pullRequest.Repository, pullRequest.Number)
	if err != nil {
		return Decision{}, fmt.Errorf("review gate: list reviews: %w", err)
	}

	return evaluateHeadReview(pullRequest.HeadSHA, g.config.codexReviewLogin(), reviews), nil
}

func (c Config) codexReviewLogin() string {
	login := strings.TrimSpace(c.CodexReviewLogin)
	if login == "" {
		return defaultCodexReviewLogin
	}
	return login
}

func isDocsOnlyChangeSet(changedFiles ChangedFiles) bool {
	if !changedFiles.Complete {
		return false
	}
	if len(changedFiles.Entries) == 0 {
		return true
	}

	for _, changedFile := range changedFiles.Entries {
		if !isDocsOnlyChangedFile(changedFile) {
			return false
		}
	}

	return true
}

func isDocsOnlyChangedFile(changedFile ChangedFile) bool {
	status := normalizeChangedFileStatus(changedFile.Status)
	if !isSupportedChangedFileStatus(status) {
		return false
	}

	effectivePaths, ok := changedFilePathsForDocsOnlyCheck(changedFile, status)
	if !ok {
		return false
	}

	for _, filePath := range effectivePaths {
		if !isDocsOnlyPath(filePath) {
			return false
		}
	}

	return true
}

func normalizeChangedFileStatus(status ChangedFileStatus) ChangedFileStatus {
	return ChangedFileStatus(strings.ToLower(strings.TrimSpace(string(status))))
}

func isSupportedChangedFileStatus(status ChangedFileStatus) bool {
	switch status {
	case
		ChangedFileStatusAdded,
		ChangedFileStatusCopied,
		ChangedFileStatusChanged,
		ChangedFileStatusModified,
		ChangedFileStatusRemoved,
		ChangedFileStatusRenamed,
		ChangedFileStatusUnchanged:
		return true
	default:
		return false
	}
}

func changedFilePathsForDocsOnlyCheck(
	changedFile ChangedFile,
	status ChangedFileStatus,
) ([]string, bool) {
	currentPath := strings.TrimSpace(changedFile.Path)
	if currentPath == "" {
		return nil, false
	}

	paths := []string{currentPath}
	previousPath := strings.TrimSpace(changedFile.PreviousPath)
	if previousPath != "" {
		paths = append(paths, previousPath)
	}

	if requiresPreviousPath(status) && previousPath == "" {
		return nil, false
	}

	return paths, true
}

func requiresPreviousPath(status ChangedFileStatus) bool {
	switch status {
	case ChangedFileStatusCopied, ChangedFileStatusRenamed:
		return true
	case
		ChangedFileStatusAdded,
		ChangedFileStatusChanged,
		ChangedFileStatusModified,
		ChangedFileStatusRemoved,
		ChangedFileStatusUnchanged:
		return false
	default:
		return false
	}
}

func isDocsOnlyPath(filename string) bool {
	cleanPath := strings.TrimSpace(filename)
	if cleanPath == "" {
		return false
	}

	cleanPath = path.Clean(cleanPath)
	switch cleanPath {
	case "README.md", "TODO.md", "LICENSE":
		return true
	}

	return strings.HasPrefix(cleanPath, "spec/")
}

func evaluateHeadReview(headSHA string, codexReviewLogin string, reviews []Review) Decision {
	latestReview, ok := latestRelevantReview(headSHA, codexReviewLogin, reviews)
	if !ok {
		return Decision{
			Status:  DecisionFail,
			Message: "missing Codex review on current head SHA",
		}
	}

	switch latestReview.State {
	case ReviewStateApproved:
		return Decision{
			Status:  DecisionPass,
			Message: "Codex review on current head SHA is approved",
		}
	case ReviewStateCommented:
		return Decision{
			Status:  DecisionPass,
			Message: "Codex review on current head SHA is commented",
		}
	case ReviewStateChangesRequested:
		return Decision{
			Status:  DecisionFail,
			Message: "Codex review on current head SHA requests changes",
		}
	default:
		return Decision{
			Status:  DecisionFail,
			Message: fmt.Sprintf("unsupported Codex review state on current head SHA: %s", latestReview.State),
		}
	}
}

func latestRelevantReview(headSHA string, codexReviewLogin string, reviews []Review) (Review, bool) {
	var latestReview Review
	var hasLatestReview bool

	for _, review := range reviews {
		if review.CommitID != headSHA {
			continue
		}
		if !strings.EqualFold(review.ReviewerLogin, codexReviewLogin) {
			continue
		}
		if !isSubmittedReviewState(review.State) {
			continue
		}
		if !hasLatestReview || submittedAfter(review, latestReview) {
			hasLatestReview = true
			latestReview = review
		}
	}

	return latestReview, hasLatestReview
}

func isSubmittedReviewState(state ReviewState) bool {
	switch state {
	case ReviewStateApproved, ReviewStateChangesRequested, ReviewStateCommented:
		return true
	default:
		return false
	}
}

func submittedAfter(candidate Review, current Review) bool {
	if candidate.SubmittedAt.After(current.SubmittedAt) {
		return true
	}
	if candidate.SubmittedAt.Equal(current.SubmittedAt) {
		return candidate.ID > current.ID
	}

	return false
}
