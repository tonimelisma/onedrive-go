package devtool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultDODRecentMerged = 20

	defaultDODCommentManifest = ".dod-pr-comments.json"
	dodManifestVersion        = 1
	githubCLI                 = "gh"
	mainBranchName            = "main"
	originMainRef             = "origin/main"
	prCIPollInterval          = 10 * time.Second
	postMergeRunListLimit     = "10"
	checkStatusCompleted      = "completed"
	checkStatusInProgress     = "in_progress"
	checkConclusionSuccess    = "success"
	checkConclusionSkipped    = "skipped"
)

type DODStage string

const (
	DODStageStart     DODStage = "start"
	DODStagePrePR     DODStage = "pre-pr"
	DODStagePreMerge  DODStage = "pre-merge"
	DODStagePostMerge DODStage = "post-merge"
)

type DODCommentClassification string

const (
	DODCommentFixed         DODCommentClassification = "fixed"
	DODCommentAlreadyFixed  DODCommentClassification = "already_fixed"
	DODCommentNonActionable DODCommentClassification = "non_actionable"
)

type DODCommentManifest struct {
	Version      int               `json:"version"`
	GeneratedAt  string            `json:"generated_at"`
	Repo         string            `json:"repo"`
	RecentMerged int               `json:"recent_merged"`
	Threads      []DODReviewThread `json:"threads"`
}

type DODReviewThread struct {
	ThreadID       string                   `json:"thread_id"`
	SourcePR       int                      `json:"source_pr"`
	SourcePRTitle  string                   `json:"source_pr_title,omitempty"`
	SourcePRURL    string                   `json:"source_pr_url,omitempty"`
	Path           string                   `json:"path,omitempty"`
	Line           *int                     `json:"line,omitempty"`
	IsResolved     bool                     `json:"is_resolved"`
	IsOutdated     bool                     `json:"is_outdated"`
	Comments       []DODReviewComment       `json:"comments"`
	Classification DODCommentClassification `json:"classification,omitempty"`
	WhatChanged    string                   `json:"what_changed,omitempty"`
	HowFixed       string                   `json:"how_fixed,omitempty"`
	Evidence       []string                 `json:"evidence,omitempty"`
	Reason         string                   `json:"reason,omitempty"`
}

type DODReviewComment struct {
	Author    string `json:"author,omitempty"`
	Body      string `json:"body,omitempty"`
	URL       string `json:"url,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

type githubRepo struct {
	Owner string
	Name  string
}

type prCheckState string

const (
	prCheckStatePass    prCheckState = "pass"
	prCheckStatePending prCheckState = "pending"
	prCheckStateFail    prCheckState = "fail"
)

// prCheckRun is one entry of a pull request's statusCheckRollup. A rerun adds
// an entry beside the old one: StartedAt and WorkflowName tell them apart.
type prCheckRun struct {
	Name         string
	Status       string
	Conclusion   string
	DetailsURL   string
	StartedAt    string
	WorkflowName string
}

type pullRequestState struct {
	State             string
	MergedAt          string
	MergeCommit       mergeCommit
	StatusCheckRollup []prCheckRun
	URL               string
}

type mergeCommit struct {
	OID string
}

// githubRun is one workflow run as `gh run list --json` and `gh run view
// --json` print it; each command fills the fields it was asked for. Like the
// other gh response structs it is untagged: encoding/json matches gh's
// camelCase keys case-insensitively.
type githubRun struct {
	DatabaseID int64
	Name       string
	Event      string
	Status     string
	Conclusion string
	HeadSHA    string
	CreatedAt  string
	UpdatedAt  string
	URL        string
	// Jobs are the jobs of the run's latest attempt (`gh run view` only).
	Jobs []githubJob
}

// githubJob is one job of a workflow run.
type githubJob struct {
	DatabaseID int64
	Name       string
	Conclusion string
}

type dodReviewThreadApplySummary struct {
	CommentedAndResolved int
	AlreadyResolved      int
}

type dodReviewThreadGateSummary struct {
	CurrentPRThreads    int
	RecentMergedThreads int
}

func RunVerifyDOD(ctx context.Context, runner commandRunner, opts *VerifyOptions) error {
	if opts == nil {
		return fmt.Errorf("dod verify options are required")
	}
	stdout, stderr := resolveVerifyWriters(opts)
	stage := opts.DODStage
	if stage == "" {
		return fmt.Errorf("dod verify: --stage is required")
	}
	gh := newGHClient(runner, nil)

	switch stage {
	case DODStageStart:
		return runDODStart(ctx, runner, gh, opts, stdout)
	case DODStagePrePR:
		return runDODPrePR(ctx, runner, opts, stdout)
	case DODStagePreMerge:
		return runDODPreMerge(ctx, runner, gh, opts, stdout)
	case DODStagePostMerge:
		return runDODPostMerge(ctx, runner, gh, opts, stdout, stderr)
	default:
		return fmt.Errorf("dod verify: unknown stage %q", stage)
	}
}

func runDODStart(ctx context.Context, runner commandRunner, gh ghClient, opts *VerifyOptions, stdout io.Writer) error {
	repo, err := loadGitHubRepo(ctx, gh, opts.RepoRoot)
	if err != nil {
		return err
	}
	threads, err := fetchRecentMergedReviewThreads(ctx, gh, opts.RepoRoot, repo, dodRecentMerged(opts))
	if err != nil {
		return err
	}
	manifestPath := dodManifestPath(opts)
	manifest, err := loadDODCommentManifest(manifestPath)
	if err != nil {
		return err
	}
	manifest = mergeDODManifest(manifest, repo, dodRecentMerged(opts), threads)
	writeErr := writeDODCommentManifest(manifestPath, manifest)
	if writeErr != nil {
		return writeErr
	}
	validateErr := validateDODCommentManifest(manifest)
	if validateErr != nil {
		return validateErr
	}
	if err := writeStatus(stdout, fmt.Sprintf("dod start: manifest ready at %s\n", manifestPath)); err != nil {
		return err
	}
	return runDODStartNightly(ctx, runner, gh, opts, stdout)
}

// runDODStartNightly makes the latest nightly run part of every increment:
// each open failure is either claimed for this increment, or already held by
// another one. The stage fails when a failure would be left without an owner.
func runDODStartNightly(ctx context.Context, runner commandRunner, gh ghClient, opts *VerifyOptions, stdout io.Writer) error {
	owner := opts.DODBranch
	if owner == "" {
		branch, err := currentIncrementBranch(ctx, runner, opts.RepoRoot)
		if err != nil {
			return err
		}
		owner = branch
	}
	return checkNightly(ctx, runner, gh, opts.RepoRoot, owner, time.Now, stdout)
}

// runDODPrePR is the last local gate before a pull request. With no profile
// named it runs `pr`, what the required PR check runs on Linux: the full race
// tier does not fit its deadline on a small host, and the nightly run owns the
// race, coverage and exhaustive failures that reach main (R-6.10.22). Naming
// `default` keeps the full tier as this stage's gate.
func runDODPrePR(ctx context.Context, runner commandRunner, opts *VerifyOptions, stdout io.Writer) error {
	profileOpts := *opts
	profileOpts.DOD = false
	if profileOpts.Profile == "" {
		profileOpts.Profile = VerifyPR
	}
	if err := runVerifyProfile(ctx, runner, &profileOpts); err != nil {
		return err
	}
	if err := verifyBranchRebased(ctx, runner, opts.RepoRoot); err != nil {
		return err
	}
	manifest, err := loadDODCommentManifest(dodManifestPath(opts))
	if err != nil {
		return err
	}
	if err := validateDODCommentManifest(manifest); err != nil {
		return err
	}
	return writeStatus(stdout, "dod pre-pr: local verifier, rebase, and PR-comment manifest gates passed\n")
}

func runDODPreMerge(ctx context.Context, runner commandRunner, gh ghClient, opts *VerifyOptions, stdout io.Writer) error {
	if opts.DODPR == 0 {
		return fmt.Errorf("dod pre-merge: --pr is required")
	}
	ciWait, err := newCIWaitConfig(opts, opts.RepoRoot, ciEventPullRequest, stdout)
	if err != nil {
		return err
	}
	err = requireNightlyClaimsInPR(ctx, runner, gh, opts.RepoRoot, opts.DODPR)
	if err != nil {
		return err
	}
	err = waitForPRCI(ctx, runner, opts.RepoRoot, opts.DODPR, ciWait)
	if err != nil {
		return err
	}
	applySummary, err := applyManifestReviewThreads(ctx, runner, gh, opts, stdout)
	if err != nil {
		return err
	}
	gateSummary, err := requireNoUnresolvedPRThreads(ctx, gh, opts.RepoRoot, opts.DODPR, dodRecentMerged(opts))
	if err != nil {
		return err
	}
	return writeStatus(stdout, formatDODPreMergeSummary(applySummary, gateSummary))
}

func runDODPostMerge(ctx context.Context, runner commandRunner, gh ghClient, opts *VerifyOptions, stdout, stderr io.Writer) error {
	if opts.DODPR == 0 {
		return fmt.Errorf("dod post-merge: --pr is required")
	}
	if opts.DODWorktree == "" {
		return fmt.Errorf("dod post-merge: --worktree is required")
	}
	if opts.DODBranch == "" {
		return fmt.Errorf("dod post-merge: --branch is required")
	}
	// The wait's policy is read before the merge, so a missing workflow fails
	// the stage while nothing has happened yet.
	ciWait, err := newCIWaitConfig(opts, opts.RepoRoot, ciEventPush, stdout)
	if err != nil {
		return err
	}

	prState, err := mergePR(ctx, runner, gh, opts.RepoRoot, opts.DODPR)
	if err != nil {
		return err
	}
	mainRoot, err := mainWorktreePath(ctx, runner, opts.RepoRoot)
	if err != nil {
		return err
	}
	if err := markNightlyClaimsFixed(ctx, runner, mainRoot, opts.DODBranch, prState.MergeCommit.OID, time.Now()); err != nil {
		return err
	}
	if err := cleanupMergedIncrement(ctx, runner, mainRoot, opts.DODWorktree, opts.DODBranch, stderr); err != nil {
		return err
	}
	if err := waitForPostMergePushCI(ctx, runner, mainRoot, prState.MergeCommit.OID, ciWait); err != nil {
		return err
	}
	if err := RunCleanupAudit(ctx, runner, CleanupAuditOptions{
		RepoRoot: mainRoot,
		Stdout:   stdout,
		Stderr:   stderr,
	}); err != nil {
		return fmt.Errorf("dod post-merge cleanup audit: %w", err)
	}
	return writeStatus(stdout, "dod post-merge: merge, cleanup, and post-merge CI interpretation passed\n")
}

func dodRecentMerged(opts *VerifyOptions) int {
	if opts.DODRecentMerged > 0 {
		return opts.DODRecentMerged
	}
	return DefaultDODRecentMerged
}

func dodManifestPath(opts *VerifyOptions) string {
	if opts.DODCommentManifest == "" {
		return filepath.Join(opts.RepoRoot, defaultDODCommentManifest)
	}
	if filepath.IsAbs(opts.DODCommentManifest) {
		return opts.DODCommentManifest
	}
	return filepath.Join(opts.RepoRoot, opts.DODCommentManifest)
}

func loadDODCommentManifest(path string) (DODCommentManifest, error) {
	data, err := readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DODCommentManifest{Version: dodManifestVersion}, nil
	}
	if err != nil {
		return DODCommentManifest{}, fmt.Errorf("read dod comment manifest %s: %w", path, err)
	}
	var manifest DODCommentManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return DODCommentManifest{}, fmt.Errorf("decode dod comment manifest %s: %w", path, err)
	}
	if manifest.Version == 0 {
		manifest.Version = dodManifestVersion
	}
	return manifest, nil
}

func writeDODCommentManifest(path string, manifest DODCommentManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal dod comment manifest: %w", err)
	}
	data = append(data, '\n')
	if err := writeFile(path, data); err != nil {
		return fmt.Errorf("write dod comment manifest: %w", err)
	}
	return nil
}

func mergeDODManifest(
	existing DODCommentManifest,
	repo githubRepo,
	recentMerged int,
	fetched []DODReviewThread,
) DODCommentManifest {
	manual := make(map[string]DODReviewThread, len(existing.Threads))
	for i := range existing.Threads {
		thread := existing.Threads[i]
		manual[thread.ThreadID] = thread
	}

	threads := make([]DODReviewThread, 0, len(fetched))
	seen := make(map[string]bool, len(fetched))
	for i := range fetched {
		thread := fetched[i]
		if seen[thread.ThreadID] {
			continue
		}
		seen[thread.ThreadID] = true
		if prior, ok := manual[thread.ThreadID]; ok {
			thread.Classification = prior.Classification
			thread.WhatChanged = prior.WhatChanged
			thread.HowFixed = prior.HowFixed
			thread.Evidence = append([]string(nil), prior.Evidence...)
			thread.Reason = prior.Reason
		}
		threads = append(threads, thread)
	}

	return DODCommentManifest{
		Version:      dodManifestVersion,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Repo:         repo.Owner + "/" + repo.Name,
		RecentMerged: recentMerged,
		Threads:      threads,
	}
}

func validateDODCommentManifest(manifest DODCommentManifest) error {
	var problems []string
	for i := range manifest.Threads {
		thread := manifest.Threads[i]
		if thread.IsResolved {
			continue
		}
		switch thread.Classification {
		case DODCommentFixed, DODCommentAlreadyFixed:
			if strings.TrimSpace(thread.WhatChanged) == "" {
				problems = append(problems, thread.ThreadID+": missing what_changed")
			}
			if strings.TrimSpace(thread.HowFixed) == "" {
				problems = append(problems, thread.ThreadID+": missing how_fixed")
			}
			if len(nonEmptyStrings(thread.Evidence)) == 0 {
				problems = append(problems, thread.ThreadID+": missing evidence")
			}
		case DODCommentNonActionable:
			if strings.TrimSpace(thread.Reason) == "" {
				problems = append(problems, thread.ThreadID+": missing reason")
			}
		case "":
			problems = append(problems, thread.ThreadID+": missing classification")
		default:
			problems = append(problems, thread.ThreadID+": invalid classification "+string(thread.Classification))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("dod comment manifest has unresolved threads needing manual classification: %s", strings.Join(problems, "; "))
	}
	return nil
}

func nonEmptyStrings(values []string) []string {
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			kept = append(kept, value)
		}
	}
	return kept
}

func loadGitHubRepo(ctx context.Context, gh ghClient, repoRoot string) (githubRepo, error) {
	out, err := gh.output(ctx, repoRoot, "dod github repo view", "repo", "view", "--json", "nameWithOwner")
	if err != nil {
		return githubRepo{}, err
	}
	var response struct {
		NameWithOwner string
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return githubRepo{}, fmt.Errorf("decode gh repo view: %w", err)
	}
	parts := strings.Split(response.NameWithOwner, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return githubRepo{}, fmt.Errorf("decode gh repo view: invalid nameWithOwner %q", response.NameWithOwner)
	}
	return githubRepo{Owner: parts[0], Name: parts[1]}, nil
}

const mergedPRListQuery = `
query($owner:String!, $repo:String!, $recent:Int!) {
  repository(owner:$owner, name:$repo) {
    pullRequests(first:$recent, states:MERGED, orderBy:{field:UPDATED_AT, direction:DESC}) {
      nodes {
        number
      }
    }
  }
}`

const singlePRReviewThreadsQuery = `
query($owner:String!, $repo:String!, $number:Int!, $after:String) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$number) {
      number
      title
      url
      reviewThreads(first:100, after:$after) {
        pageInfo {
          hasNextPage
          endCursor
        }
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          comments(first:20) {
            nodes {
              author { login }
              body
              url
              createdAt
            }
          }
        }
      }
    }
  }
}`

func fetchRecentMergedReviewThreads(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	repo githubRepo,
	recent int,
) ([]DODReviewThread, error) {
	prNumbers, err := fetchRecentMergedPullRequestNumbers(ctx, gh, repoRoot, repo, recent)
	if err != nil {
		return nil, err
	}

	threads := []DODReviewThread{}
	for _, prNumber := range prNumbers {
		prThreads, err := fetchPullRequestReviewThreads(ctx, gh, repoRoot, repo, prNumber)
		if err != nil {
			return nil, err
		}
		threads = append(threads, prThreads...)
	}
	return threads, nil
}

func fetchRecentMergedPullRequestNumbers(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	repo githubRepo,
	recent int,
) ([]int, error) {
	out, err := gh.output(
		ctx,
		repoRoot,
		"dod fetch recent merged PRs",
		"api",
		"graphql",
		"-f",
		"query="+mergedPRListQuery,
		"-f",
		"owner="+repo.Owner,
		"-f",
		"repo="+repo.Name,
		"-F",
		"recent="+strconv.Itoa(recent),
	)
	if err != nil {
		return nil, err
	}

	var response mergedPRListResponse
	if err := json.Unmarshal(out, &response); err != nil {
		return nil, fmt.Errorf("decode recent merged PRs: %w", err)
	}

	nodes := response.Data.Repository.PullRequests.Nodes
	numbers := make([]int, 0, len(nodes))
	for i := range nodes {
		numbers = append(numbers, nodes[i].Number)
	}
	return numbers, nil
}

func fetchPullRequestReviewThreads(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	repo githubRepo,
	prNumber int,
) ([]DODReviewThread, error) {
	pr, err := fetchPullRequestReviewThreadPages(ctx, gh, repoRoot, repo, prNumber)
	if err != nil {
		return nil, err
	}
	return reviewThreadsFromPR(pr.Number, pr.Title, pr.URL, pr.ReviewThreads.Nodes), nil
}

func fetchPullRequestReviewThreadPages(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	repo githubRepo,
	prNumber int,
) (graphqlPullRequest, error) {
	var merged graphqlPullRequest
	cursor := ""
	for {
		page, err := fetchPullRequestReviewThreadPage(ctx, gh, repoRoot, repo, prNumber, cursor)
		if err != nil {
			return graphqlPullRequest{}, err
		}
		if merged.Number == 0 {
			merged.Number = page.Number
			merged.Title = page.Title
			merged.URL = page.URL
		}
		merged.ReviewThreads.Nodes = append(merged.ReviewThreads.Nodes, page.ReviewThreads.Nodes...)
		pageInfo := page.ReviewThreads.PageInfo
		if !pageInfo.HasNextPage {
			return merged, nil
		}
		if pageInfo.EndCursor == "" {
			return graphqlPullRequest{}, fmt.Errorf("dod fetch PR #%d review threads: missing next cursor", prNumber)
		}
		cursor = pageInfo.EndCursor
	}
}

func fetchPullRequestReviewThreadPage(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	repo githubRepo,
	prNumber int,
	cursor string,
) (graphqlPullRequest, error) {
	args := []string{
		"api",
		"graphql",
		"-f",
		"query=" + singlePRReviewThreadsQuery,
		"-f",
		"owner=" + repo.Owner,
		"-f",
		"repo=" + repo.Name,
		"-F",
		"number=" + strconv.Itoa(prNumber),
	}
	if cursor != "" {
		args = append(args, "-f", "after="+cursor)
	}
	out, err := gh.output(ctx, repoRoot, fmt.Sprintf("dod fetch PR #%d review threads", prNumber), args...)
	if err != nil {
		return graphqlPullRequest{}, err
	}

	var response singlePRThreadsResponse
	if err := json.Unmarshal(out, &response); err != nil {
		return graphqlPullRequest{}, fmt.Errorf("decode PR review threads: %w", err)
	}
	return response.Data.Repository.PullRequest, nil
}

type mergedPRListResponse struct {
	Data struct {
		Repository struct {
			PullRequests struct {
				Nodes []graphqlPullRequestSummary
			}
		}
	}
}

type graphqlPullRequestSummary struct {
	Number int
}

type singlePRThreadsResponse struct {
	Data struct {
		Repository struct {
			PullRequest graphqlPullRequest
		}
	}
}

type graphqlPullRequest struct {
	Number        int
	Title         string
	URL           string
	ReviewThreads graphqlReviewThreadConnection
}

type graphqlReviewThreadConnection struct {
	PageInfo graphqlPageInfo
	Nodes    []graphqlReviewThread
}

type graphqlPageInfo struct {
	HasNextPage bool
	EndCursor   string
}

type graphqlReviewThread struct {
	ID         string
	IsResolved bool
	IsOutdated bool
	Path       string
	Line       *int
	Comments   struct {
		Nodes []graphqlReviewComment
	}
}

type graphqlReviewComment struct {
	Author struct {
		Login string
	}
	Body      string
	URL       string
	CreatedAt string
}

func reviewThreadsFromPR(prNumber int, prTitle string, prURL string, nodes []graphqlReviewThread) []DODReviewThread {
	threads := make([]DODReviewThread, 0, len(nodes))
	for i := range nodes {
		node := nodes[i]
		comments := make([]DODReviewComment, 0, len(node.Comments.Nodes))
		for j := range node.Comments.Nodes {
			comment := node.Comments.Nodes[j]
			comments = append(comments, DODReviewComment{
				Author:    comment.Author.Login,
				Body:      comment.Body,
				URL:       comment.URL,
				CreatedAt: comment.CreatedAt,
			})
		}
		threads = append(threads, DODReviewThread{
			ThreadID:      node.ID,
			SourcePR:      prNumber,
			SourcePRTitle: prTitle,
			SourcePRURL:   prURL,
			Path:          node.Path,
			Line:          node.Line,
			IsResolved:    node.IsResolved,
			IsOutdated:    node.IsOutdated,
			Comments:      comments,
		})
	}
	return threads
}

func verifyBranchRebased(ctx context.Context, runner commandRunner, repoRoot string) error {
	if err := runner.Run(ctx, repoRoot, os.Environ(), io.Discard, io.Discard, "git", "fetch", "origin"); err != nil {
		return fmt.Errorf("dod pre-pr fetch origin: %w", err)
	}
	if err := runner.Run(
		ctx,
		repoRoot,
		os.Environ(),
		io.Discard,
		io.Discard,
		"git",
		"merge-base",
		"--is-ancestor",
		originMainRef,
		"HEAD",
	); err != nil {
		return fmt.Errorf("dod pre-pr branch is not rebased on %s: %w", originMainRef, err)
	}
	return nil
}

func loadPullRequestState(ctx context.Context, gh ghClient, repoRoot string, prNumber int) (pullRequestState, error) {
	out, err := gh.output(
		ctx,
		repoRoot,
		fmt.Sprintf("dod gh pr view %d", prNumber),
		"pr",
		"view",
		strconv.Itoa(prNumber),
		"--json",
		"state,mergedAt,mergeCommit,statusCheckRollup,url",
	)
	if err != nil {
		return pullRequestState{}, err
	}
	var state pullRequestState
	if err := json.Unmarshal(out, &state); err != nil {
		return pullRequestState{}, fmt.Errorf("decode gh pr view %d: %w", prNumber, err)
	}
	return state, nil
}

func applyManifestReviewThreads(
	ctx context.Context,
	runner commandRunner,
	gh ghClient,
	opts *VerifyOptions,
	stdout io.Writer,
) (dodReviewThreadApplySummary, error) {
	repo, err := loadGitHubRepo(ctx, gh, opts.RepoRoot)
	if err != nil {
		return dodReviewThreadApplySummary{}, err
	}
	currentPRThreads, err := fetchPullRequestReviewThreads(ctx, gh, opts.RepoRoot, repo, opts.DODPR)
	if err != nil {
		return dodReviewThreadApplySummary{}, err
	}
	manifestPath := dodManifestPath(opts)
	manifest, err := loadDODCommentManifest(manifestPath)
	if err != nil {
		return dodReviewThreadApplySummary{}, err
	}
	fetchedThreads := make([]DODReviewThread, 0, len(currentPRThreads)+len(manifest.Threads))
	fetchedThreads = append(fetchedThreads, currentPRThreads...)
	fetchedThreads = append(fetchedThreads, manifest.Threads...)
	manifest = mergeDODManifest(manifest, repo, dodRecentMerged(opts), fetchedThreads)
	writeErr := writeDODCommentManifest(manifestPath, manifest)
	if writeErr != nil {
		return dodReviewThreadApplySummary{}, writeErr
	}
	validateErr := validateDODCommentManifest(manifest)
	if validateErr != nil {
		return dodReviewThreadApplySummary{}, validateErr
	}
	commit, err := gitHeadShort(ctx, runner, opts.RepoRoot)
	if err != nil {
		return dodReviewThreadApplySummary{}, err
	}
	summary, changed, replyErr := replyToManifestThreads(ctx, gh, opts.RepoRoot, manifest.Threads, opts.DODPR, commit, stdout)
	if changed {
		// Record what is now resolved even when a later reply failed, so a
		// rerun never answers the same thread twice.
		if err := writeDODCommentManifest(manifestPath, manifest); err != nil {
			return dodReviewThreadApplySummary{}, errors.Join(replyErr, err)
		}
	}
	if replyErr != nil {
		return dodReviewThreadApplySummary{}, replyErr
	}
	return summary, nil
}

// replyToManifestThreads answers and resolves every classified thread that is
// still open on GitHub. The manifest's is_resolved is only a snapshot from when
// the manifest was written, and an ignored manifest outlives its increment, so
// each thread's live state is checked first; without that, every pre-merge
// run from the same checkout replied to the same long-resolved thread again,
// crediting whichever PR was merging. threads is updated in place, and changed
// reports whether any is_resolved flag moved so the caller can persist it.
func replyToManifestThreads(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	threads []DODReviewThread,
	prNumber int,
	commit string,
	stdout io.Writer,
) (dodReviewThreadApplySummary, bool, error) {
	summary := dodReviewThreadApplySummary{}
	changed := false
	for i := range threads {
		thread := &threads[i]
		if thread.IsResolved {
			summary.AlreadyResolved++
			continue
		}
		resolved, err := reviewThreadIsResolved(ctx, gh, repoRoot, thread.ThreadID)
		if err != nil {
			return summary, changed, err
		}
		if resolved {
			thread.IsResolved = true
			changed = true
			summary.AlreadyResolved++
			continue
		}
		body := renderDODThreadReply(thread, prNumber, commit)
		if _, err := postReviewThreadReply(ctx, gh, repoRoot, thread.ThreadID, body); err != nil {
			return summary, changed, err
		}
		if err := resolveReviewThread(ctx, gh, repoRoot, thread.ThreadID); err != nil {
			return summary, changed, err
		}
		thread.IsResolved = true
		changed = true
		summary.CommentedAndResolved++
		if err := writeStatus(stdout, formatDODThreadResolutionStatus(thread)); err != nil {
			return summary, changed, err
		}
	}
	return summary, changed, nil
}

// reviewThreadIsResolved reads one review thread's live resolution state.
func reviewThreadIsResolved(ctx context.Context, gh ghClient, repoRoot string, threadID string) (bool, error) {
	out, err := gh.output(ctx, repoRoot, "dod read review thread "+threadID, buildThreadResolvedQueryArgs(threadID)...)
	if err != nil {
		return false, err
	}
	// Untagged like the other gh response structs: encoding/json matches
	// GitHub's camelCase keys case-insensitively.
	var response struct {
		Data struct {
			Node *struct {
				IsResolved bool
			}
		}
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return false, fmt.Errorf("decode review thread %s state: %w", threadID, err)
	}
	if response.Data.Node == nil {
		return false, fmt.Errorf("dod read review thread %s: thread not found", threadID)
	}
	return response.Data.Node.IsResolved, nil
}

func formatDODThreadResolutionStatus(thread *DODReviewThread) string {
	return fmt.Sprintf(
		"dod pre-merge: commented and resolved %s classification=%s evidence=%d\n",
		thread.ThreadID,
		thread.Classification,
		len(nonEmptyStrings(thread.Evidence)),
	)
}

func formatDODPreMergeSummary(
	applySummary dodReviewThreadApplySummary,
	gateSummary dodReviewThreadGateSummary,
) string {
	return fmt.Sprintf(
		"dod pre-merge: PR CI and review-thread gates passed "+
			"(commented_resolved=%d, already_resolved=%d, current_pr_threads=%d, recent_merged_threads=%d)\n",
		applySummary.CommentedAndResolved,
		applySummary.AlreadyResolved,
		gateSummary.CurrentPRThreads,
		gateSummary.RecentMergedThreads,
	)
}

func gitHeadShort(ctx context.Context, runner commandRunner, repoRoot string) (string, error) {
	out, err := runner.Output(ctx, repoRoot, os.Environ(), "git", "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("dod git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func renderDODThreadReply(thread *DODReviewThread, fixPR int, commit string) string {
	var body bytes.Buffer
	switch thread.Classification {
	case DODCommentFixed:
		renderDODFixedThreadReply(&body, thread, fixPR, commit)
	case DODCommentAlreadyFixed:
		renderDODFixedThreadReply(&body, thread, fixPR, commit)
	case DODCommentNonActionable:
		fmt.Fprintf(&body, "Resolved as non-actionable during #%d", fixPR)
		if commit != "" {
			fmt.Fprintf(&body, " (%s)", commit)
		}
		fmt.Fprintf(&body, ".\n\nReason: %s", strings.TrimSpace(thread.Reason))
	default:
		fmt.Fprintf(&body, "Unable to render unclassified thread %s.", thread.ThreadID)
	}
	return body.String()
}

func renderDODFixedThreadReply(body *bytes.Buffer, thread *DODReviewThread, fixPR int, commit string) {
	fmt.Fprintf(body, "Addressed in #%d", fixPR)
	if commit != "" {
		fmt.Fprintf(body, " (%s)", commit)
	}
	fmt.Fprintf(
		body,
		".\n\nWhat changed: %s\n\nHow fixed: %s",
		strings.TrimSpace(thread.WhatChanged),
		strings.TrimSpace(thread.HowFixed),
	)
	evidence := nonEmptyStrings(thread.Evidence)
	if len(evidence) > 0 {
		fmt.Fprintf(body, "\n\nEvidence:")
		for _, item := range evidence {
			fmt.Fprintf(body, "\n- %s", strings.TrimSpace(item))
		}
	}
}

// postReviewThreadReply posts one reply, once: GitHub can answer a 5xx after
// it posted, and a repeat would post the reply twice.
func postReviewThreadReply(ctx context.Context, gh ghClient, repoRoot string, threadID string, body string) (string, error) {
	out, err := gh.outputOnce(ctx, repoRoot, "dod reply to review thread "+threadID, buildAddThreadReplyArgs(threadID, body)...)
	if err != nil {
		return "", err
	}
	var response struct {
		Data struct {
			AddPullRequestReviewThreadReply struct {
				Comment struct {
					URL string
				}
			}
		}
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return "", fmt.Errorf("decode review thread reply response: %w", err)
	}
	return response.Data.AddPullRequestReviewThreadReply.Comment.URL, nil
}

// resolveReviewThread resolves one thread. Resolving a resolved thread
// changes nothing, so a failed attempt is repeated.
func resolveReviewThread(ctx context.Context, gh ghClient, repoRoot string, threadID string) error {
	if _, err := gh.output(ctx, repoRoot, "dod resolve review thread "+threadID, buildResolveThreadArgs(threadID)...); err != nil {
		return err
	}
	return nil
}

func buildAddThreadReplyArgs(threadID string, body string) []string {
	return []string{
		"api",
		"graphql",
		"-f",
		"query=mutation($threadId:ID!, $body:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$threadId, body:$body}){comment{url}}}", //nolint:lll // GraphQL mutation string must stay exact for gh api.
		"-F",
		"threadId=" + threadID,
		"-F",
		"body=" + body,
	}
}

func buildThreadResolvedQueryArgs(threadID string) []string {
	return []string{
		"api",
		"graphql",
		"-f",
		"query=query($threadId:ID!){node(id:$threadId){... on PullRequestReviewThread{isResolved}}}",
		"-F",
		"threadId=" + threadID,
	}
}

func buildResolveThreadArgs(threadID string) []string {
	return []string{
		"api",
		"graphql",
		"-f",
		"query=mutation($threadId:ID!){resolveReviewThread(input:{threadId:$threadId}){thread{id isResolved}}}",
		"-F",
		"threadId=" + threadID,
	}
}

func requireNoUnresolvedPRThreads(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	prNumber int,
	recentMerged int,
) (dodReviewThreadGateSummary, error) {
	repo, err := loadGitHubRepo(ctx, gh, repoRoot)
	if err != nil {
		return dodReviewThreadGateSummary{}, err
	}
	threads, err := fetchPullRequestReviewThreads(ctx, gh, repoRoot, repo, prNumber)
	if err != nil {
		return dodReviewThreadGateSummary{}, err
	}
	summary := dodReviewThreadGateSummary{CurrentPRThreads: len(threads)}
	unresolved := unresolvedThreadIDs(threads)
	if len(unresolved) > 0 {
		return dodReviewThreadGateSummary{}, fmt.Errorf(
			"dod pre-merge unresolved review threads remain on PR #%d: %s",
			prNumber,
			strings.Join(unresolved, ", "),
		)
	}
	recent, err := fetchRecentMergedReviewThreads(ctx, gh, repoRoot, repo, recentMerged)
	if err != nil {
		return dodReviewThreadGateSummary{}, err
	}
	summary.RecentMergedThreads = len(recent)
	if unresolved = unresolvedThreadIDs(recent); len(unresolved) > 0 {
		return dodReviewThreadGateSummary{}, fmt.Errorf(
			"dod pre-merge unresolved recent merged-PR review threads remain: %s",
			strings.Join(unresolved, ", "),
		)
	}
	return summary, nil
}

func unresolvedThreadIDs(threads []DODReviewThread) []string {
	unresolved := []string{}
	for i := range threads {
		thread := threads[i]
		if !thread.IsResolved {
			unresolved = append(unresolved, thread.ThreadID)
		}
	}
	return unresolved
}

// mergePR merges the pull request once. A failed merge is not repeated: the
// server's merge state, read next, decides whether it happened.
func mergePR(ctx context.Context, runner commandRunner, gh ghClient, repoRoot string, prNumber int) (pullRequestState, error) {
	output, err := runner.CombinedOutput(
		ctx,
		repoRoot,
		os.Environ(),
		githubCLI,
		"pr",
		"merge",
		strconv.Itoa(prNumber),
		"--squash",
		"--delete-branch",
	)
	if err == nil {
		return requirePRMerged(ctx, gh, repoRoot, prNumber)
	}
	state, stateErr := requirePRMerged(ctx, gh, repoRoot, prNumber)
	if stateErr == nil {
		return state, nil
	}
	if !isRecoverableMergePRError(string(output), err) {
		return pullRequestState{}, fmt.Errorf(
			"dod merge PR #%d: %s: %w (server merge check: %v)",
			prNumber,
			strings.TrimSpace(string(output)),
			err,
			stateErr.Error(),
		)
	}
	return pullRequestState{}, stateErr
}

func isRecoverableMergePRError(output string, err error) bool {
	return isMainWorktreeMergeQuirk(output, err) || isAlreadyMergedPRMergeOutput(output, err)
}

func isMainWorktreeMergeQuirk(output string, err error) bool {
	text := strings.ToLower(output + " " + err.Error())
	return strings.Contains(text, "main") && strings.Contains(text, "already used by worktree")
}

func isAlreadyMergedPRMergeOutput(output string, err error) bool {
	text := strings.ToLower(output + " " + err.Error())
	return strings.Contains(text, "already merged")
}

func requirePRMerged(ctx context.Context, gh ghClient, repoRoot string, prNumber int) (pullRequestState, error) {
	state, err := loadPullRequestState(ctx, gh, repoRoot, prNumber)
	if err != nil {
		return pullRequestState{}, err
	}
	if state.State != "MERGED" || state.MergedAt == "" || state.MergeCommit.OID == "" {
		return pullRequestState{}, fmt.Errorf("dod merge PR #%d: server state is not merged", prNumber)
	}
	return state, nil
}

func mainWorktreePath(ctx context.Context, runner commandRunner, repoRoot string) (string, error) {
	worktrees, err := loadWorktreeStates(ctx, runner, repoRoot)
	if err != nil {
		return "", fmt.Errorf("dod load worktrees: %w", err)
	}
	for i := range worktrees {
		worktree := worktrees[i]
		if worktree.Branch == mainBranchName {
			return worktree.Path, nil
		}
	}
	return "", fmt.Errorf("dod main worktree not found")
}

func cleanupMergedIncrement(
	ctx context.Context,
	runner commandRunner,
	mainRoot string,
	worktreePath string,
	branch string,
	stderr io.Writer,
) error {
	if err := fastForwardMainForDODCleanup(ctx, runner, mainRoot, stderr); err != nil {
		return err
	}
	if err := removeDODWorktree(ctx, runner, mainRoot, worktreePath, stderr); err != nil {
		return err
	}
	if err := deleteDODIncrementBranches(ctx, runner, mainRoot, branch); err != nil {
		return err
	}
	if err := runDODCleanupGit(ctx, runner, mainRoot, stderr, "fetch", "--prune", "origin"); err != nil {
		return fmt.Errorf("dod cleanup final fetch: %w", err)
	}
	return nil
}

func fastForwardMainForDODCleanup(ctx context.Context, runner commandRunner, mainRoot string, stderr io.Writer) error {
	if err := runDODCleanupGit(ctx, runner, mainRoot, stderr, "fetch", "--prune", "origin"); err != nil {
		return fmt.Errorf("dod cleanup fetch: %w", err)
	}
	if err := runDODCleanupGit(ctx, runner, mainRoot, stderr, "checkout", mainBranchName); err != nil {
		return fmt.Errorf("dod cleanup checkout main: %w", err)
	}
	if err := runDODCleanupGit(ctx, runner, mainRoot, stderr, "pull", "--ff-only", "origin", mainBranchName); err != nil {
		return fmt.Errorf("dod cleanup pull main: %w", err)
	}
	return nil
}

func removeDODWorktree(
	ctx context.Context,
	runner commandRunner,
	mainRoot string,
	worktreePath string,
	stderr io.Writer,
) error {
	if _, err := stat(worktreePath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("dod cleanup stat worktree: %w", err)
	}
	// git refuses a worktree that holds untracked files, and the bootstrap
	// symlink is one. Only what bootstrap put there goes; anything else is left
	// for git to refuse, so a worktree holding real work is never forced away.
	if err := removeBootstrapEntries(defaultWorktreeFS(), mainRoot, worktreePath); err != nil {
		return fmt.Errorf("dod cleanup remove bootstrap entries: %w", err)
	}
	if err := runDODCleanupGit(ctx, runner, mainRoot, stderr, "worktree", "remove", worktreePath); err != nil {
		return fmt.Errorf("dod cleanup remove worktree: %w", err)
	}
	return nil
}

func deleteDODIncrementBranches(
	ctx context.Context,
	runner commandRunner,
	mainRoot string,
	branch string,
) error {
	if err := deleteDODLocalBranch(ctx, runner, mainRoot, branch); err != nil {
		return err
	}
	if err := deleteDODRemoteBranch(ctx, runner, mainRoot, branch); err != nil {
		return err
	}
	return nil
}

func deleteDODLocalBranch(ctx context.Context, runner commandRunner, mainRoot string, branch string) error {
	output, err := runner.CombinedOutput(ctx, mainRoot, os.Environ(), "git", "branch", "-D", branch)
	if err == nil || isMissingLocalBranchText(string(output), err) {
		return nil
	}
	return fmt.Errorf("dod cleanup delete local branch: %s: %w", strings.TrimSpace(string(output)), err)
}

func deleteDODRemoteBranch(ctx context.Context, runner commandRunner, mainRoot string, branch string) error {
	output, err := runner.CombinedOutput(ctx, mainRoot, os.Environ(), "git", "push", "origin", "--delete", branch)
	if err == nil || isMissingRemoteBranchText(string(output), err) {
		return nil
	}
	return fmt.Errorf("dod cleanup delete remote branch: %s: %w", strings.TrimSpace(string(output)), err)
}

func runDODCleanupGit(
	ctx context.Context,
	runner commandRunner,
	mainRoot string,
	stderr io.Writer,
	args ...string,
) error {
	if err := runner.Run(ctx, mainRoot, os.Environ(), io.Discard, stderr, "git", args...); err != nil {
		return fmt.Errorf("run cleanup git %v: %w", args, err)
	}
	return nil
}

func isMissingLocalBranchText(output string, err error) bool {
	text := strings.ToLower(output + " " + err.Error())
	return strings.Contains(text, "branch") && (strings.Contains(text, "not found") || strings.Contains(text, "not a branch"))
}

func isMissingRemoteBranchText(output string, err error) bool {
	text := strings.ToLower(output + " " + err.Error())
	return strings.Contains(text, "remote ref does not exist") ||
		strings.Contains(text, "remote ref not found")
}
