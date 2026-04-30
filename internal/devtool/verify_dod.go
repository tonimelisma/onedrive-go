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
	DefaultDODCITimeout    = 30 * time.Minute

	defaultDODCommentManifest = ".dod-pr-comments.json"
	dodManifestVersion        = 1
	githubCLI                 = "gh"
	mainBranchName            = "main"
	originMainRef             = "origin/main"
	prCIPollInterval          = 10 * time.Second
	postMergeRunListLimit     = "10"
	checkStatusCompleted      = "completed"
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

type prCheckRun struct {
	Name       string
	Status     string
	Conclusion string
	DetailsURL string
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

type githubRun struct {
	Name       string
	Event      string
	Status     string
	Conclusion string
	HeadSHA    string
	URL        string
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

	switch stage {
	case DODStageStart:
		return runDODStart(ctx, runner, opts, stdout)
	case DODStagePrePR:
		return runDODPrePR(ctx, runner, opts, stdout)
	case DODStagePreMerge:
		return runDODPreMerge(ctx, runner, opts, stdout)
	case DODStagePostMerge:
		return runDODPostMerge(ctx, runner, opts, stdout, stderr)
	default:
		return fmt.Errorf("dod verify: unknown stage %q", stage)
	}
}

func runDODStart(ctx context.Context, runner commandRunner, opts *VerifyOptions, stdout io.Writer) error {
	repo, err := loadGitHubRepo(ctx, runner, opts.RepoRoot)
	if err != nil {
		return err
	}
	threads, err := fetchRecentMergedReviewThreads(ctx, runner, opts.RepoRoot, repo, dodRecentMerged(opts))
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
	return writeStatus(stdout, fmt.Sprintf("dod start: manifest ready at %s\n", manifestPath))
}

func runDODPrePR(ctx context.Context, runner commandRunner, opts *VerifyOptions, stdout io.Writer) error {
	profileOpts := *opts
	profileOpts.DOD = false
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

func runDODPreMerge(ctx context.Context, runner commandRunner, opts *VerifyOptions, stdout io.Writer) error {
	if opts.DODPR == 0 {
		return fmt.Errorf("dod pre-merge: --pr is required")
	}
	if err := waitForPRCI(ctx, runner, opts.RepoRoot, opts.DODPR, dodCITimeout(opts)); err != nil {
		return err
	}
	if err := applyManifestReviewThreads(ctx, runner, opts, stdout); err != nil {
		return err
	}
	if err := requireNoUnresolvedPRThreads(ctx, runner, opts.RepoRoot, opts.DODPR, dodRecentMerged(opts)); err != nil {
		return err
	}
	return writeStatus(stdout, "dod pre-merge: PR CI and review-thread gates passed\n")
}

func runDODPostMerge(ctx context.Context, runner commandRunner, opts *VerifyOptions, stdout, stderr io.Writer) error {
	if opts.DODPR == 0 {
		return fmt.Errorf("dod post-merge: --pr is required")
	}
	if opts.DODWorktree == "" {
		return fmt.Errorf("dod post-merge: --worktree is required")
	}
	if opts.DODBranch == "" {
		return fmt.Errorf("dod post-merge: --branch is required")
	}

	prState, err := mergePR(ctx, runner, opts.RepoRoot, opts.DODPR)
	if err != nil {
		return err
	}
	mainRoot, err := mainWorktreePath(ctx, runner, opts.RepoRoot)
	if err != nil {
		return err
	}
	if err := cleanupMergedIncrement(ctx, runner, mainRoot, opts.DODWorktree, opts.DODBranch, stderr); err != nil {
		return err
	}
	if err := waitForPostMergePushCI(
		ctx,
		runner,
		mainRoot,
		prState.MergeCommit.OID,
		dodCITimeout(opts),
	); err != nil {
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

func dodCITimeout(opts *VerifyOptions) time.Duration {
	if opts.DODCITimeout > 0 {
		return opts.DODCITimeout
	}
	return DefaultDODCITimeout
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

func loadGitHubRepo(ctx context.Context, runner commandRunner, repoRoot string) (githubRepo, error) {
	out, err := runner.Output(ctx, repoRoot, os.Environ(), githubCLI, "repo", "view", "--json", "nameWithOwner")
	if err != nil {
		return githubRepo{}, fmt.Errorf("dod github repo view: %w", err)
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

const mergedPRReviewThreadsQuery = `
query($owner:String!, $repo:String!, $recent:Int!) {
  repository(owner:$owner, name:$repo) {
    pullRequests(first:$recent, states:MERGED, orderBy:{field:UPDATED_AT, direction:DESC}) {
      nodes {
        number
        title
        url
        reviewThreads(first:100) {
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
  }
}`

const singlePRReviewThreadsQuery = `
query($owner:String!, $repo:String!, $number:Int!) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$number) {
      number
      title
      url
      reviewThreads(first:100) {
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
	runner commandRunner,
	repoRoot string,
	repo githubRepo,
	recent int,
) ([]DODReviewThread, error) {
	out, err := runner.Output(
		ctx,
		repoRoot,
		os.Environ(),
		githubCLI,
		"api",
		"graphql",
		"-f",
		"query="+mergedPRReviewThreadsQuery,
		"-f",
		"owner="+repo.Owner,
		"-f",
		"repo="+repo.Name,
		"-F",
		"recent="+strconv.Itoa(recent),
	)
	if err != nil {
		return nil, fmt.Errorf("dod fetch merged review threads: %w", err)
	}

	var response mergedPRThreadsResponse
	if err := json.Unmarshal(out, &response); err != nil {
		return nil, fmt.Errorf("decode merged review threads: %w", err)
	}

	return reviewThreadsFromMergedResponse(response), nil
}

func fetchPullRequestReviewThreads(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	repo githubRepo,
	prNumber int,
) ([]DODReviewThread, error) {
	out, err := runner.Output(
		ctx,
		repoRoot,
		os.Environ(),
		githubCLI,
		"api",
		"graphql",
		"-f",
		"query="+singlePRReviewThreadsQuery,
		"-f",
		"owner="+repo.Owner,
		"-f",
		"repo="+repo.Name,
		"-F",
		"number="+strconv.Itoa(prNumber),
	)
	if err != nil {
		return nil, fmt.Errorf("dod fetch PR review threads: %w", err)
	}

	var response singlePRThreadsResponse
	if err := json.Unmarshal(out, &response); err != nil {
		return nil, fmt.Errorf("decode PR review threads: %w", err)
	}
	pr := response.Data.Repository.PullRequest
	return reviewThreadsFromPR(pr.Number, pr.Title, pr.URL, pr.ReviewThreads.Nodes), nil
}

type mergedPRThreadsResponse struct {
	Data struct {
		Repository struct {
			PullRequests struct {
				Nodes []graphqlPullRequest
			}
		}
	}
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
	ReviewThreads struct {
		Nodes []graphqlReviewThread
	}
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

func reviewThreadsFromMergedResponse(response mergedPRThreadsResponse) []DODReviewThread {
	threads := []DODReviewThread{}
	for i := range response.Data.Repository.PullRequests.Nodes {
		pr := response.Data.Repository.PullRequests.Nodes[i]
		threads = append(threads, reviewThreadsFromPR(pr.Number, pr.Title, pr.URL, pr.ReviewThreads.Nodes)...)
	}
	return threads
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

func waitForPRCI(ctx context.Context, runner commandRunner, repoRoot string, prNumber int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		state, err := loadPullRequestState(ctx, runner, repoRoot, prNumber)
		if err != nil {
			return err
		}
		status, reason := classifyPRChecks(state.StatusCheckRollup)
		switch status {
		case prCheckStatePass:
			return nil
		case prCheckStateFail:
			return fmt.Errorf("dod pre-merge PR CI failed: %s", reason)
		case prCheckStatePending:
			if time.Now().After(deadline) {
				return fmt.Errorf("dod pre-merge PR CI timed out after %s: %s", timeout, reason)
			}
			timer := time.NewTimer(prCIPollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("dod pre-merge PR CI wait: %w", ctx.Err())
			case <-timer.C:
			}
		}
	}
}

func classifyPRChecks(checks []prCheckRun) (prCheckState, string) {
	required := map[string]bool{
		"verify":      false,
		"integration": false,
		"e2e":         false,
	}
	for i := range checks {
		check := checks[i]
		name := strings.ToLower(check.Name)
		status := strings.ToLower(check.Status)
		conclusion := strings.ToLower(check.Conclusion)
		if _, ok := required[name]; ok {
			if status != checkStatusCompleted {
				return prCheckStatePending, check.Name + " is " + status
			}
			if conclusion != checkConclusionSuccess {
				return prCheckStateFail, check.Name + " concluded " + conclusion
			}
			required[name] = true
			continue
		}
		if name == "stress" &&
			status == checkStatusCompleted &&
			conclusion != checkConclusionSuccess &&
			conclusion != checkConclusionSkipped {
			return prCheckStateFail, check.Name + " concluded " + conclusion
		}
	}
	missing := []string{}
	for name, seen := range required {
		if !seen {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return prCheckStatePending, "missing required checks: " + strings.Join(missing, ", ")
	}
	return prCheckStatePass, "required PR checks passed"
}

func loadPullRequestState(ctx context.Context, runner commandRunner, repoRoot string, prNumber int) (pullRequestState, error) {
	out, err := runner.Output(
		ctx,
		repoRoot,
		os.Environ(),
		githubCLI,
		"pr",
		"view",
		strconv.Itoa(prNumber),
		"--json",
		"state,mergedAt,mergeCommit,statusCheckRollup,url",
	)
	if err != nil {
		return pullRequestState{}, fmt.Errorf("dod gh pr view %d: %w", prNumber, err)
	}
	var state pullRequestState
	if err := json.Unmarshal(out, &state); err != nil {
		return pullRequestState{}, fmt.Errorf("decode gh pr view %d: %w", prNumber, err)
	}
	return state, nil
}

func applyManifestReviewThreads(ctx context.Context, runner commandRunner, opts *VerifyOptions, stdout io.Writer) error {
	repo, err := loadGitHubRepo(ctx, runner, opts.RepoRoot)
	if err != nil {
		return err
	}
	currentPRThreads, err := fetchPullRequestReviewThreads(ctx, runner, opts.RepoRoot, repo, opts.DODPR)
	if err != nil {
		return err
	}
	manifestPath := dodManifestPath(opts)
	manifest, err := loadDODCommentManifest(manifestPath)
	if err != nil {
		return err
	}
	fetchedThreads := make([]DODReviewThread, 0, len(currentPRThreads)+len(manifest.Threads))
	fetchedThreads = append(fetchedThreads, currentPRThreads...)
	fetchedThreads = append(fetchedThreads, manifest.Threads...)
	manifest = mergeDODManifest(manifest, repo, dodRecentMerged(opts), fetchedThreads)
	writeErr := writeDODCommentManifest(manifestPath, manifest)
	if writeErr != nil {
		return writeErr
	}
	validateErr := validateDODCommentManifest(manifest)
	if validateErr != nil {
		return validateErr
	}
	commit, err := gitHeadShort(ctx, runner, opts.RepoRoot)
	if err != nil {
		return err
	}
	for i := range manifest.Threads {
		thread := &manifest.Threads[i]
		if thread.IsResolved {
			continue
		}
		body := renderDODThreadReply(thread, opts.DODPR, commit)
		if _, err := postReviewThreadReply(ctx, runner, opts.RepoRoot, thread.ThreadID, body); err != nil {
			return err
		}
		if err := resolveReviewThread(ctx, runner, opts.RepoRoot, thread.ThreadID); err != nil {
			return err
		}
		if err := writeStatus(stdout, fmt.Sprintf("dod pre-merge: commented and resolved %s\n", thread.ThreadID)); err != nil {
			return err
		}
	}
	return nil
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

func postReviewThreadReply(ctx context.Context, runner commandRunner, repoRoot string, threadID string, body string) (string, error) {
	name, args := buildAddThreadReplyArgs(threadID, body)
	out, err := runner.Output(ctx, repoRoot, os.Environ(), name, args...)
	if err != nil {
		return "", fmt.Errorf("dod reply to review thread %s: %w", threadID, err)
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

func resolveReviewThread(ctx context.Context, runner commandRunner, repoRoot string, threadID string) error {
	name, args := buildResolveThreadArgs(threadID)
	if _, err := runner.Output(ctx, repoRoot, os.Environ(), name, args...); err != nil {
		return fmt.Errorf("dod resolve review thread %s: %w", threadID, err)
	}
	return nil
}

func buildAddThreadReplyArgs(threadID string, body string) (string, []string) {
	return githubCLI, []string{
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

func buildResolveThreadArgs(threadID string) (string, []string) {
	return githubCLI, []string{
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
	runner commandRunner,
	repoRoot string,
	prNumber int,
	recentMerged int,
) error {
	repo, err := loadGitHubRepo(ctx, runner, repoRoot)
	if err != nil {
		return err
	}
	threads, err := fetchPullRequestReviewThreads(ctx, runner, repoRoot, repo, prNumber)
	if err != nil {
		return err
	}
	unresolved := unresolvedThreadIDs(threads)
	if len(unresolved) > 0 {
		return fmt.Errorf(
			"dod pre-merge unresolved review threads remain on PR #%d: %s",
			prNumber,
			strings.Join(unresolved, ", "),
		)
	}
	recent, err := fetchRecentMergedReviewThreads(ctx, runner, repoRoot, repo, recentMerged)
	if err != nil {
		return err
	}
	if unresolved = unresolvedThreadIDs(recent); len(unresolved) > 0 {
		return fmt.Errorf(
			"dod pre-merge unresolved recent merged-PR review threads remain: %s",
			strings.Join(unresolved, ", "),
		)
	}
	return nil
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

func mergePR(ctx context.Context, runner commandRunner, repoRoot string, prNumber int) (pullRequestState, error) {
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
		return requirePRMerged(ctx, runner, repoRoot, prNumber)
	}
	if !isMainWorktreeMergeQuirk(string(output), err) {
		return pullRequestState{}, fmt.Errorf(
			"dod merge PR #%d: %s: %w",
			prNumber,
			strings.TrimSpace(string(output)),
			err,
		)
	}
	return requirePRMerged(ctx, runner, repoRoot, prNumber)
}

func isMainWorktreeMergeQuirk(output string, err error) bool {
	text := strings.ToLower(output + " " + err.Error())
	return strings.Contains(text, "main") && strings.Contains(text, "already used by worktree")
}

func requirePRMerged(ctx context.Context, runner commandRunner, repoRoot string, prNumber int) (pullRequestState, error) {
	state, err := loadPullRequestState(ctx, runner, repoRoot, prNumber)
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
	if err := deleteDODIncrementBranches(ctx, runner, mainRoot, branch, stderr); err != nil {
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
	if err := os.Remove(filepath.Join(worktreePath, ".testdata")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("dod cleanup remove worktree .testdata: %w", err)
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
	stderr io.Writer,
) error {
	if err := runDODCleanupGit(ctx, runner, mainRoot, stderr, "branch", "-D", branch); err != nil &&
		!isMissingLocalBranchError(err) {
		return fmt.Errorf("dod cleanup delete local branch: %w", err)
	}
	if err := runDODCleanupGit(ctx, runner, mainRoot, stderr, "push", "origin", "--delete", branch); err != nil &&
		!isMissingRemoteBranchError(err) {
		return fmt.Errorf("dod cleanup delete remote branch: %w", err)
	}
	return nil
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

func isMissingLocalBranchError(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "branch") && (strings.Contains(text, "not found") || strings.Contains(text, "not a branch"))
}

func isMissingRemoteBranchError(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "remote ref does not exist") || strings.Contains(text, "unable to delete")
}

func waitForPostMergePushCI(
	ctx context.Context,
	runner commandRunner,
	mainRoot string,
	mergeCommit string,
	timeout time.Duration,
) error {
	if mergeCommit == "" {
		return fmt.Errorf("dod post-merge: missing merge commit")
	}
	subjectOut, err := runner.Output(ctx, mainRoot, os.Environ(), "git", "show", "-s", "--format=%s", mergeCommit)
	if err != nil {
		return fmt.Errorf("dod post-merge commit subject: %w", err)
	}
	commitSubject := strings.TrimSpace(string(subjectOut))
	deadline := time.Now().Add(timeout)
	for {
		runs, err := loadMainRunsForCommit(ctx, runner, mainRoot, mergeCommit)
		if err != nil {
			return err
		}
		status, reason := classifyPostMergeRuns(runs, commitSubject, mergeCommit)
		switch status {
		case prCheckStatePass:
			return nil
		case prCheckStateFail:
			return fmt.Errorf("dod post-merge CI is not clean: %s", reason)
		case prCheckStatePending:
			if time.Now().After(deadline) {
				return fmt.Errorf("dod post-merge CI timed out after %s: %s", timeout, reason)
			}
			timer := time.NewTimer(prCIPollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("dod post-merge CI wait: %w", ctx.Err())
			case <-timer.C:
			}
		}
	}
}

func loadMainRunsForCommit(ctx context.Context, runner commandRunner, repoRoot string, commit string) ([]githubRun, error) {
	out, err := runner.Output(
		ctx,
		repoRoot,
		os.Environ(),
		githubCLI,
		"run",
		"list",
		"--branch",
		mainBranchName,
		"--commit",
		commit,
		"--json",
		"databaseId,name,event,status,conclusion,headSha,createdAt,updatedAt,url",
		"--limit",
		postMergeRunListLimit,
	)
	if err != nil {
		return nil, fmt.Errorf("dod post-merge run list: %w", err)
	}
	var runs []githubRun
	if err := json.Unmarshal(out, &runs); err != nil {
		return nil, fmt.Errorf("decode post-merge run list: %w", err)
	}
	return runs, nil
}

func classifyPostMergeRuns(runs []githubRun, commitSubject string, commit string) (prCheckState, string) {
	if len(runs) == 0 {
		return prCheckStatePending, "no main push CI run found for " + commit
	}
	for i := range runs {
		run := runs[i]
		if run.HeadSHA != commit {
			continue
		}
		status := strings.ToLower(run.Status)
		conclusion := strings.ToLower(run.Conclusion)
		if status != checkStatusCompleted {
			return prCheckStatePending, run.Name + " is " + status
		}
		switch conclusion {
		case checkConclusionSuccess:
			return prCheckStatePass, run.Name + " succeeded"
		case checkConclusionSkipped:
			if run.Event == "push" && strings.Contains(commitSubject, "(#") {
				return prCheckStatePass, run.Name + " skipped by squash-merge push rule"
			}
			return prCheckStateFail, run.Name + " skipped without squash-merge subject"
		default:
			return prCheckStateFail, run.Name + " concluded " + conclusion
		}
	}
	return prCheckStatePending, "no run matched merge commit " + commit
}
