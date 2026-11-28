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

	// githubRepoPath is the REST path of the checkout's repository: gh fills
	// in {owner} and {repo} from the git remote, with no GraphQL request.
	githubRepoPath = "repos/{owner}/{repo}"
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

// prCheckRun is one check run of a pull request's head commit, as REST
// `GET .../commits/{sha}/check-runs` lists it. The list holds the latest run
// of each check (the endpoint's default filter); a rerun's newer StartedAt
// still tells it from an older one should both appear.
type prCheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"details_url"`
	StartedAt  string `json:"started_at"`
}

// githubPullRequest is a pull request as REST `GET .../pulls/{n}` returns it.
// The DoD stages and the nightly gate read pull requests over REST alone, so
// they run wherever gh reaches the REST API: on a developer's machine, in CI,
// and in a Claude Code cloud session, which refuses GraphQL.
type githubPullRequest struct {
	State          string `json:"state"`
	Title          string `json:"title"`
	Merged         bool   `json:"merged"`
	MergedAt       string `json:"merged_at"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Body           string `json:"body"`
	HTMLURL        string `json:"html_url"`
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

// githubRun is one workflow run as `gh run list --json` and `gh run view
// --json` print it; each command fills the fields it was asked for. Like the
// other gh response structs it is untagged: encoding/json matches gh's
// camelCase keys case-insensitively.
type githubRun struct {
	DatabaseID int64
	Name       string
	Event      string
	HeadBranch string
	Status     string
	Conclusion string
	HeadSHA    string
	CreatedAt  string
	// StartedAt is when the run's latest attempt started: a rerun starts a
	// new attempt and keeps CreatedAt.
	StartedAt string
	UpdatedAt string
	URL       string
	// Attempt counts the run's attempts, 1 before any rerun (`gh run view`
	// only, when asked for).
	Attempt int
	// Jobs are the jobs of the run's latest attempt (`gh run view` only).
	Jobs []githubJob
}

// githubJob is one job of a workflow run.
type githubJob struct {
	DatabaseID int64
	Name       string
	Conclusion string
	// StartedAt is set once the job is queued, so it alone does not tell a
	// job that ran from one canceled while it waited for a runner.
	StartedAt string
	// Steps are the job's steps; a runner adds the first when it picks the
	// job up.
	Steps []json.RawMessage
}

// started reports whether a runner picked the job up. A job canceled while
// it waited for a runner or for its concurrency group lists no step.
func (j githubJob) started() bool {
	startedAt, err := time.Parse(time.RFC3339, j.StartedAt)

	return err == nil && !startedAt.IsZero() && len(j.Steps) > 0
}

func RunVerifyDOD(ctx context.Context, runner commandRunner, opts *VerifyOptions) error {
	if opts == nil {
		return errors.New("dod verify options are required")
	}
	stdout, stderr := resolveVerifyWriters(opts)
	stage := opts.DODStage
	if stage == "" {
		return errors.New("dod verify: --stage is required")
	}
	gh := newGHClient(runner, nil)

	switch stage {
	case DODStageStart:
		return runDODStart(ctx, runner, gh, opts, stdout)
	case DODStagePrePR:
		return runDODPrePR(ctx, runner, gh, opts, stdout)
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
	if writeErr := writeStatus(stdout, fmt.Sprintf("dod start: manifest ready at %s\n", manifestPath)); writeErr != nil {
		return writeErr
	}
	owner, err := dodIncrementBranch(ctx, runner, opts)
	if err != nil {
		return err
	}
	return runDODNightly(ctx, runner, gh, opts.RepoRoot, DODStageStart, owner, time.Now, stdout)
}

// dodIncrementBranch is the branch that owns this increment's nightly
// claims: --branch, or the checked-out branch, or "" on main.
func dodIncrementBranch(ctx context.Context, runner commandRunner, opts *VerifyOptions) (string, error) {
	if opts.DODBranch != "" {
		return opts.DODBranch, nil
	}
	return currentIncrementBranch(ctx, runner, opts.RepoRoot)
}

// runDODNightly is the nightly step of every stage. The start stage makes
// the latest nightly part of the increment: each open failure is claimed for
// it or already held by another one, and the stage fails when a failure
// would be left without an owner, when the nightly has no verdict, or when
// GitHub still fails a read after the client's retries, in one plain
// sentence. The later stages renew the branch's claims (pre-pr and
// pre-merge; post-merge marks them fixed instead) and print the latest
// verdict, and turn every failure of this step into a warning: an increment
// that has started is not stopped by GitHub, nor by a nightly it did not
// touch.
func runDODNightly(
	ctx context.Context,
	runner commandRunner,
	gh ghClient,
	repoRoot string,
	stage DODStage,
	owner string,
	now func() time.Time,
	stdout io.Writer,
) error {
	if stage == DODStageStart {
		return plainGitHubFailure(checkNightly(ctx, runner, gh, repoRoot, owner, now, stdout))
	}

	var b strings.Builder
	warn := func(err error) {
		fmt.Fprintf(&b, "dod %s: warning: %s\n", stage, plainGitHubFailure(err))
	}
	if owner != "" && stage != DODStagePostMerge {
		renewed, err := renewNightlyClaims(ctx, gh, repoRoot, owner, now())
		for _, key := range renewed {
			fmt.Fprintf(&b, "nightly: renewed %s's claim on %s\n", owner, key)
		}
		if err != nil {
			warn(err)
		}
	}
	verdict, err := loadNightlyVerdict(ctx, gh, repoRoot, now())
	switch {
	case err != nil:
		warn(err)
	case verdict.finding != "":
		warn(errors.New(verdict.finding))
	case verdict.found:
		writeNightlyRunVerdict(&b, &verdict.run, verdict.reports)
	}

	return writeStatus(stdout, b.String())
}

// runDODPrePR is the last local gate before a pull request. With no profile
// named it runs `pr`, what the required PR check runs on Linux: the full race
// tier does not fit its deadline on a small host, and the nightly run owns the
// race, coverage and exhaustive failures that reach main (R-6.10.22). Naming
// `public` keeps the full tier as this stage's gate.
func runDODPrePR(ctx context.Context, runner commandRunner, gh ghClient, opts *VerifyOptions, stdout io.Writer) error {
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
	if validateErr := validateDODCommentManifest(manifest); validateErr != nil {
		return validateErr
	}
	if writeErr := writeStatus(stdout, "dod pre-pr: local verifier, rebase, and PR-comment manifest gates passed\n"); writeErr != nil {
		return writeErr
	}
	owner, err := dodIncrementBranch(ctx, runner, opts)
	if err != nil {
		if writeErr := writeStatus(stdout, fmt.Sprintf("dod pre-pr: warning: %s; no nightly claim renewed\n", err)); writeErr != nil {
			return writeErr
		}
	}
	return runDODNightly(ctx, runner, gh, opts.RepoRoot, DODStagePrePR, owner, time.Now, stdout)
}

func runDODPreMerge(ctx context.Context, runner commandRunner, gh ghClient, opts *VerifyOptions, stdout io.Writer) error {
	if opts.DODPR == 0 {
		return errors.New("dod pre-merge: --pr is required")
	}
	ciWait, err := newCIWaitConfig(opts, opts.RepoRoot, ciEventPullRequest, stdout)
	if err != nil {
		return err
	}
	err = runDODPreMergeNightly(ctx, runner, gh, opts.RepoRoot, opts.DODPR, time.Now, stdout)
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

// runDODPreMergeNightly is pre-merge's nightly step. The pull request's head
// branch owns its claims: the gate refuses a body that does not name each of
// them, which fails the stage, and the step then renews them and prints the
// latest verdict, warning instead of failing as every later stage does.
func runDODPreMergeNightly(
	ctx context.Context,
	runner commandRunner,
	gh ghClient,
	repoRoot string,
	prNumber int,
	now func() time.Time,
	stdout io.Writer,
) error {
	head, err := requireNightlyClaimsInPR(ctx, gh, repoRoot, prNumber)
	if err != nil {
		return plainGitHubFailure(err)
	}

	return runDODNightly(ctx, runner, gh, repoRoot, DODStagePreMerge, head, now, stdout)
}

func runDODPostMerge(ctx context.Context, runner commandRunner, gh ghClient, opts *VerifyOptions, stdout, stderr io.Writer) error {
	if opts.DODPR == 0 {
		return errors.New("dod post-merge: --pr is required")
	}
	// The wait's policy is read before the merge, so a missing workflow fails
	// the stage while nothing has happened yet.
	ciWait, err := newCIWaitConfig(opts, opts.RepoRoot, ciEventPush, stdout)
	if err != nil {
		return err
	}
	branch, err := postMergeBranch(ctx, runner, opts)
	if err != nil {
		return err
	}
	pr, err := loadPullRequest(ctx, gh, opts.RepoRoot, opts.DODPR)
	if err != nil {
		return err
	}
	if pr.Head.Ref != branch {
		return fmt.Errorf("dod post-merge: --branch %s is not the head branch of PR #%d, %s", branch, opts.DODPR, pr.Head.Ref)
	}
	// The branch is deleted once the merge is in, so a change it does not
	// hold yet would be lost: refuse before anything happens.
	if cleanErr := requireCleanCheckout(ctx, runner, opts.RepoRoot); cleanErr != nil {
		return cleanErr
	}
	if headErr := requireBranchAtHead(ctx, runner, opts.RepoRoot, branch, pr.Head.SHA); headErr != nil {
		return headErr
	}

	merged, err := mergePR(ctx, gh, opts.RepoRoot, opts.DODPR, &pr)
	if err != nil {
		return err
	}
	if err := markNightlyClaimsFixed(ctx, gh, opts.RepoRoot, branch, merged.MergeCommitSHA, time.Now()); err != nil {
		return err
	}
	if err := returnToMain(ctx, runner, opts.RepoRoot, branch, stderr); err != nil {
		return err
	}
	if err := waitForPostMergePushCI(ctx, runner, opts.RepoRoot, merged.MergeCommitSHA, ciWait); err != nil {
		return err
	}
	if err := PruneMergedWorktrees(ctx, runner, WorktreePruneOptions{RepoRoot: opts.RepoRoot, Stdout: stdout}); err != nil {
		return fmt.Errorf("dod post-merge: %w", err)
	}
	if err := runCleanupAudit(ctx, runner, cleanupAuditOptions{
		RepoRoot: opts.RepoRoot,
		Stdout:   stdout,
		Stderr:   stderr,
	}); err != nil {
		return fmt.Errorf("dod post-merge cleanup audit: %w", err)
	}
	if err := runDODNightly(ctx, runner, gh, opts.RepoRoot, DODStagePostMerge, branch, time.Now, stdout); err != nil {
		return err
	}
	return writeStatus(stdout, "dod post-merge: merge, return to main, and post-merge CI interpretation passed\n")
}

// postMergeBranch is the increment's branch: --branch, else the branch the
// checkout is on, the increment's branch.
func postMergeBranch(ctx context.Context, runner commandRunner, opts *VerifyOptions) (string, error) {
	if opts.DODBranch != "" {
		return opts.DODBranch, nil
	}
	branch, err := currentIncrementBranch(ctx, runner, opts.RepoRoot)
	if err != nil {
		return "", err
	}
	if branch == "" {
		return "", errors.New("dod post-merge: run it on the increment's branch, or pass --branch")
	}
	return branch, nil
}

// requireCleanCheckout refuses a checkout holding a change git tracks or a
// file it does not: the increment's branch is about to be deleted.
func requireCleanCheckout(ctx context.Context, runner commandRunner, repoRoot string) error {
	out, err := runner.Output(ctx, repoRoot, os.Environ(), "git", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("dod post-merge read checkout status: %w", err)
	}
	if changed := strings.TrimSpace(string(out)); changed != "" {
		return fmt.Errorf("dod post-merge: the checkout holds changes the merge does not; commit or remove them first:\n%s", changed)
	}
	return nil
}

// returnToMain moves the checkout from the merged increment's branch to a
// fresh main and deletes the local branch. A branch is never reused after
// its pull request merged, and nothing pushes it again: GitHub deletes the
// remote branch on merge, and the next increment cuts a new one from
// origin/main. The caller has checked that the checkout is clean and that
// the branch's tip is the head GitHub merged, so the delete loses nothing.
func returnToMain(ctx context.Context, runner commandRunner, repoRoot string, branch string, stderr io.Writer) error {
	for _, args := range [][]string{
		{"fetch", "--prune", "origin"},
		{"checkout", "-B", "main", originMainRef},
		{"branch", "-D", branch},
	} {
		if err := runner.Run(ctx, repoRoot, os.Environ(), io.Discard, stderr, "git", args...); err != nil {
			return fmt.Errorf("dod post-merge return to %s from %s: git %s: %w", originMainRef, branch, args[0], err)
		}
	}
	return nil
}

// requireBranchAtHead refuses a local branch whose tip is not the pull
// request's head: a commit not pushed would be lost when the branch is
// deleted after the merge.
func requireBranchAtHead(ctx context.Context, runner commandRunner, repoRoot string, branch string, head string) error {
	out, err := runner.Output(ctx, repoRoot, os.Environ(), "git", "rev-parse", "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("dod post-merge read %s: %w", branch, err)
	}
	if tip := strings.TrimSpace(string(out)); tip != head {
		return fmt.Errorf("dod post-merge: %s is at %s, not the pull request's head %s; push it or reset it first", branch, tip, head)
	}
	return nil
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
	out, err := gh.output(ctx, repoRoot, "dod github repo", "api", githubRepoPath)
	if err != nil {
		return githubRepo{}, err
	}
	var response struct {
		FullName string `json:"full_name"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return githubRepo{}, fmt.Errorf("decode GitHub repository: %w", err)
	}
	owner, name, ok := strings.Cut(response.FullName, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return githubRepo{}, fmt.Errorf("decode GitHub repository: invalid full_name %q", response.FullName)
	}
	return githubRepo{Owner: owner, Name: name}, nil
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

func loadPullRequest(ctx context.Context, gh ghClient, repoRoot string, prNumber int) (githubPullRequest, error) {
	number := strconv.Itoa(prNumber)
	out, err := gh.output(ctx, repoRoot, "dod github pull request "+number, "api", githubRepoPath+"/pulls/"+number)
	if err != nil {
		return githubPullRequest{}, err
	}
	var pr githubPullRequest
	if err := json.Unmarshal(out, &pr); err != nil {
		return githubPullRequest{}, fmt.Errorf("decode pull request %d: %w", prNumber, err)
	}
	return pr, nil
}

// loadCommitCheckRuns lists the check runs of commit, every page of them.
func loadCommitCheckRuns(ctx context.Context, gh ghClient, repoRoot string, commit string) ([]prCheckRun, error) {
	out, err := gh.output(ctx, repoRoot, "dod github check runs of "+shortSHA(commit),
		"api", "--paginate", githubRepoPath+"/commits/"+commit+"/check-runs?per_page=100", "--jq", ".check_runs[]")
	if err != nil {
		return nil, err
	}
	// --jq prints one check run per line, across every page.
	checks := []prCheckRun{}
	decoder := json.NewDecoder(bytes.NewReader(out))
	for decoder.More() {
		var check prCheckRun
		if err := decoder.Decode(&check); err != nil {
			return nil, fmt.Errorf("decode check runs of %s: %w", shortSHA(commit), err)
		}
		checks = append(checks, check)
	}
	return checks, nil
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

// mergeTrailer ends every squash commit's message. The repository squashes
// with the pull request's body as the message, which drops the commits'
// trailers, so the merge states the attribution itself.
const mergeTrailer = "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"

// squashMessage is the squash commit's message: the pull request's body,
// then mergeTrailer unless the body already ends with it.
func squashMessage(body string) string {
	body = strings.TrimRight(body, "\n")
	if strings.HasSuffix(body, mergeTrailer) {
		return body
	}
	if body == "" {
		return mergeTrailer
	}
	return body + "\n\n" + mergeTrailer
}

// mergePR squash-merges the pull request over REST, pinned to the head commit
// the stage checked: GitHub refuses the merge when the branch moved since. The
// commit is titled like GitHub's own squash ("<title> (#N)") and carries
// squashMessage. The request is sent once; whether it merged, also when it
// failed after GitHub carried it out or when the pull request was merged
// already, is the server's merge state, read next.
func mergePR(ctx context.Context, gh ghClient, repoRoot string, prNumber int, pr *githubPullRequest) (githubPullRequest, error) {
	number := strconv.Itoa(prNumber)
	_, mergeErr := gh.outputOnce(ctx, repoRoot, "dod merge PR #"+number+" (PUT repos/{owner}/{repo}/pulls/"+number+"/merge)",
		"api", "-X", "PUT", githubRepoPath+"/pulls/"+number+"/merge", "-f", "merge_method=squash", "-f", "sha="+pr.Head.SHA,
		"-f", "commit_title="+pr.Title+" (#"+number+")", "-f", "commit_message="+squashMessage(pr.Body))
	merged, stateErr := requirePRMerged(ctx, gh, repoRoot, prNumber)
	if stateErr == nil {
		return merged, nil
	}
	if mergeErr != nil {
		return githubPullRequest{}, fmt.Errorf("dod merge PR #%d: %w (server merge check: %w)", prNumber, mergeErr, stateErr)
	}
	return githubPullRequest{}, stateErr
}

func requirePRMerged(ctx context.Context, gh ghClient, repoRoot string, prNumber int) (githubPullRequest, error) {
	pr, err := loadPullRequest(ctx, gh, repoRoot, prNumber)
	if err != nil {
		return githubPullRequest{}, err
	}
	if !pr.Merged || pr.MergedAt == "" || pr.MergeCommitSHA == "" {
		return githubPullRequest{}, fmt.Errorf("dod merge PR #%d: server state is not merged", prNumber)
	}
	return pr, nil
}
